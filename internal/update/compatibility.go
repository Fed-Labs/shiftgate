package update

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Reasons an available release is not applied. They are typed for the same
// reason the scheduler's rejections are: an operator asking "why has this
// machine not updated?" gets an answer instead of silence.
const (
	SkipChannel       = "CHANNEL_MISMATCH"
	SkipComponent     = "COMPONENT_MISSING"
	SkipPlatform      = "PLATFORM_UNSUPPORTED"
	SkipRevoked       = "RELEASE_REVOKED"
	SkipNotNewer      = "NOT_NEWER_THAN_INSTALLED"
	SkipPreRelease    = "PRE_RELEASE_NOT_ALLOWED"
	SkipUpgradePath   = "INTERMEDIATE_UPGRADE_REQUIRED"
	SkipProtocol      = "CONTROL_PLANE_PROTOCOL_UNSUPPORTED"
	SkipConfigSchema  = "CONFIG_SCHEMA_TOO_NEW"
	SkipStateSchema   = "STATE_SCHEMA_TOO_NEW"
	SkipRolloutCohort = "ROLLOUT_NOT_REACHED"
	SkipUntrusted     = "SIGNATURE_UNTRUSTED"
)

// Installation describes the software an update would replace. It is what the
// running process knows about itself, so the compatibility checks compare a
// release against reality rather than against configuration.
type Installation struct {
	Component      Component `json:"component"`
	Version        string    `json:"version"`
	Channel        Channel   `json:"channel"`
	OS             string    `json:"os"`
	Architecture   string    `json:"architecture"`
	MachineID      string    `json:"machine_id"`
	ExecutablePath string    `json:"executable_path,omitempty"`
	// ProtocolVersion is the agent-to-control-plane protocol the running
	// software speaks, ConfigVersion the on-disk configuration schema it wrote,
	// and StateVersion the on-disk state schema it wrote. A release that cannot
	// read what is already on this machine is not an upgrade.
	ProtocolVersion int `json:"protocol_version"`
	ConfigVersion   int `json:"config_version"`
	StateVersion    int `json:"state_version"`
	// PeerProtocolVersion is the protocol the control plane currently requires,
	// or zero when the machine has no control plane to answer to. An upgrade
	// that would leave the agent unable to report in is refused.
	PeerProtocolVersion int `json:"peer_protocol_version,omitempty"`
	// AllowDowngrade is set only by a rollback, which deliberately installs an
	// older version.
	AllowDowngrade bool `json:"allow_downgrade,omitempty"`
}

// Validate checks the description of the running installation.
func (installation *Installation) Validate() error {
	installation.Component = Component(strings.ToLower(strings.TrimSpace(string(installation.Component))))
	installation.Channel = Channel(strings.ToLower(strings.TrimSpace(string(installation.Channel))))
	installation.OS = strings.ToLower(strings.TrimSpace(installation.OS))
	installation.Architecture = strings.ToLower(strings.TrimSpace(installation.Architecture))
	installation.MachineID = strings.TrimSpace(installation.MachineID)
	if !installation.Component.Valid() {
		return fmt.Errorf("unsupported component %q", installation.Component)
	}
	if !installation.Channel.Valid() {
		return fmt.Errorf("unsupported channel %q", installation.Channel)
	}
	version, err := ParseVersion(installation.Version)
	if err != nil {
		return fmt.Errorf("installed version: %w", err)
	}
	installation.Version = version.String()
	if installation.OS == "" || installation.Architecture == "" {
		return errors.New("an installation requires an operating system and an architecture")
	}
	if installation.MachineID == "" {
		return errors.New("an installation requires a machine identity for rollout cohorts")
	}
	if installation.ProtocolVersion <= 0 || installation.ConfigVersion <= 0 || installation.StateVersion <= 0 {
		return errors.New("an installation requires positive protocol, config, and state versions")
	}
	if installation.PeerProtocolVersion < 0 {
		return errors.New("peer protocol version cannot be negative")
	}
	return nil
}

// Evaluation is the verdict on one release for one installation.
type Evaluation struct {
	Version    string        `json:"version"`
	Channel    Channel       `json:"channel"`
	Mandatory  bool          `json:"mandatory"`
	Applicable bool          `json:"applicable"`
	Code       string        `json:"code,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Rollout    RolloutStatus `json:"rollout"`
	SignedBy   []string      `json:"signed_by,omitempty"`
	Artifact   *Artifact     `json:"artifact,omitempty"`
	release    Release
}

// Release returns the release this evaluation covers.
func (evaluation Evaluation) Release() Release {
	return evaluation.release
}

// Evaluate checks every release in a feed against one installation and returns a
// verdict for each, newest first. Signature verification happens here, before
// any decision is taken on a release, so an unsigned or wrongly signed release
// can never become the selected update.
func Evaluate(feed Feed, installation Installation, ring *KeyRing, now time.Time) ([]Evaluation, error) {
	if ring == nil {
		return nil, errors.New("evaluating releases requires a trusted key ring")
	}
	if err := feed.Validate(); err != nil {
		return nil, err
	}
	if err := installation.Validate(); err != nil {
		return nil, err
	}
	installed, err := ParseVersion(installation.Version)
	if err != nil {
		return nil, err
	}
	evaluations := make([]Evaluation, 0, len(feed.Releases))
	for _, signed := range feed.Releases {
		evaluations = append(evaluations, evaluateRelease(feed, signed, installation, installed, ring, now))
	}
	sortEvaluationsNewestFirst(evaluations)
	return evaluations, nil
}

func evaluateRelease(feed Feed, signed SignedRelease, installation Installation, installed Version, ring *KeyRing, now time.Time) Evaluation {
	release := signed.Release
	evaluation := Evaluation{
		Version:   release.Version,
		Channel:   release.Channel,
		Mandatory: release.Mandatory,
		release:   release,
	}
	version, err := release.SemanticVersion()
	if err != nil {
		return evaluation.skip(SkipNotNewer, err.Error())
	}
	evaluation.Rollout = release.Rollout.Status(installation.MachineID, version, now)

	signedBy, err := ring.Verify(signed)
	if err != nil {
		return evaluation.skip(SkipUntrusted, err.Error())
	}
	evaluation.SignedBy = signedBy

	if release.Channel != installation.Channel {
		return evaluation.skip(SkipChannel, fmt.Sprintf("release is on channel %s, this machine follows %s", release.Channel, installation.Channel))
	}
	if feed.IsRevoked(version) {
		return evaluation.skip(SkipRevoked, "release was withdrawn by the update feed")
	}
	if !installation.AllowDowngrade && !installed.Precedes(version) {
		return evaluation.skip(SkipNotNewer, fmt.Sprintf("release %s is not newer than the installed %s", version, installed))
	}
	if !version.Stable() && installation.Channel == ChannelStable {
		return evaluation.skip(SkipPreRelease, "a pre-release is not installed on the stable channel")
	}
	artifact, found := release.ArtifactFor(installation.Component, installation.OS, installation.Architecture)
	if !found {
		if !hasComponent(release, installation.Component) {
			return evaluation.skip(SkipComponent, fmt.Sprintf("release publishes no %s artifact", installation.Component))
		}
		return evaluation.skip(SkipPlatform, fmt.Sprintf("release publishes no %s artifact for %s/%s",
			installation.Component, installation.OS, installation.Architecture))
	}
	if release.MinimumFromVersion != "" && !installation.AllowDowngrade {
		minimum, err := ParseVersion(release.MinimumFromVersion)
		if err != nil {
			return evaluation.skip(SkipUpgradePath, err.Error())
		}
		if installed.Precedes(minimum) {
			return evaluation.skip(SkipUpgradePath, fmt.Sprintf("upgrading to %s requires %s or newer first, this machine runs %s",
				version, minimum, installed))
		}
	}
	if installation.PeerProtocolVersion > 0 {
		if installation.PeerProtocolVersion < release.MinimumProtocolVersion ||
			installation.PeerProtocolVersion > release.ProtocolVersion {
			return evaluation.skip(SkipProtocol, fmt.Sprintf("release speaks control-plane protocol %d..%d, this machine's control plane requires %d",
				release.MinimumProtocolVersion, release.ProtocolVersion, installation.PeerProtocolVersion))
		}
	}
	if release.ConfigVersion < installation.ConfigVersion {
		return evaluation.skip(SkipConfigSchema, fmt.Sprintf("release reads configuration schema %d but this machine has written schema %d",
			release.ConfigVersion, installation.ConfigVersion))
	}
	if release.StateVersion < installation.StateVersion {
		return evaluation.skip(SkipStateSchema, fmt.Sprintf("release reads state schema %d but this machine has written schema %d",
			release.StateVersion, installation.StateVersion))
	}
	if !evaluation.Rollout.Eligible {
		return evaluation.skip(SkipRolloutCohort, fmt.Sprintf("release is open to %d%% of machines, this machine is in bucket %d of %d",
			evaluation.Rollout.PercentOpen, evaluation.Rollout.Cohort, evaluation.Rollout.Buckets))
	}
	evaluation.Applicable = true
	evaluation.Artifact = &artifact
	return evaluation
}

func hasComponent(release Release, component Component) bool {
	for _, artifact := range release.Artifacts {
		if artifact.Component == component {
			return true
		}
	}
	return false
}

func (evaluation Evaluation) skip(code, reason string) Evaluation {
	evaluation.Applicable = false
	evaluation.Code = code
	evaluation.Reason = reason
	evaluation.Artifact = nil
	return evaluation
}

func sortEvaluationsNewestFirst(evaluations []Evaluation) {
	for outer := 1; outer < len(evaluations); outer++ {
		for inner := outer; inner > 0; inner-- {
			left, leftErr := ParseVersion(evaluations[inner-1].Version)
			right, rightErr := ParseVersion(evaluations[inner].Version)
			if leftErr != nil || rightErr != nil || !left.Precedes(right) {
				break
			}
			evaluations[inner-1], evaluations[inner] = evaluations[inner], evaluations[inner-1]
		}
	}
}

// Select picks the newest applicable release from a set of evaluations. It
// returns false when nothing is applicable, which is the ordinary case for a
// machine that is already current.
func Select(evaluations []Evaluation) (Evaluation, bool) {
	for _, evaluation := range evaluations {
		if evaluation.Applicable {
			return evaluation, true
		}
	}
	return Evaluation{}, false
}
