package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// Channel is a release train. A machine only ever considers releases from the
// channel it is configured for, so a beta build cannot reach a stable fleet.
type Channel string

const (
	ChannelStable  Channel = "stable"
	ChannelBeta    Channel = "beta"
	ChannelNightly Channel = "nightly"
)

// Valid reports whether the channel is one this implementation knows.
func (channel Channel) Valid() bool {
	switch channel {
	case ChannelStable, ChannelBeta, ChannelNightly:
		return true
	default:
		return false
	}
}

// Component names what an artifact replaces. The agent and the desktop
// application update independently: a desktop release must never be installed
// over an agent, so the component is part of what is signed.
type Component string

const (
	ComponentAgent   Component = "agent"
	ComponentDesktop Component = "desktop"
	ComponentCLI     Component = "cli"
)

// Valid reports whether the component is one this implementation knows.
func (component Component) Valid() bool {
	switch component {
	case ComponentAgent, ComponentDesktop, ComponentCLI:
		return true
	default:
		return false
	}
}

// Format is how an artifact is published. Only single executables are
// supported, plain or gzip-compressed: an update never unpacks an archive of
// arbitrary paths, so a malicious release cannot write outside the staging
// directory.
type Format string

const (
	FormatBinary Format = "binary"
	FormatGzip   Format = "gzip"
)

// Valid reports whether the format is one this implementation knows.
func (format Format) Valid() bool {
	return format == FormatBinary || format == FormatGzip
}

// Artifact is one executable for one platform. Both digests are recorded: the
// published bytes are checked as downloaded, and the executable is checked again
// after decompression, so a corrupt or substituted payload is caught at whichever
// stage it was introduced.
type Artifact struct {
	Component    Component `json:"component"`
	OS           string    `json:"os"`
	Architecture string    `json:"architecture"`
	URL          string    `json:"url"`
	Format       Format    `json:"format"`
	SizeBytes    int64     `json:"size_bytes"`
	SHA256       string    `json:"sha256"`
	BinarySHA256 string    `json:"binary_sha256"`
}

// Release is one published version of SHIFT. Everything an installer needs to
// decide whether the release is applicable is part of the release itself, and
// therefore part of what the signature covers.
type Release struct {
	Version     string    `json:"version"`
	Channel     Channel   `json:"channel"`
	PublishedAt time.Time `json:"published_at"`
	Notes       string    `json:"notes,omitempty"`
	// MinimumFromVersion refuses a direct jump from versions older than this,
	// so upgrades that need an intermediate migration step cannot skip it.
	MinimumFromVersion string `json:"minimum_from_version,omitempty"`
	// ProtocolVersion is the agent-to-control-plane protocol this release
	// speaks, and MinimumProtocolVersion the oldest it still understands. A
	// machine that would end up unable to talk to its control plane is not
	// upgraded.
	ProtocolVersion        int `json:"protocol_version"`
	MinimumProtocolVersion int `json:"minimum_protocol_version"`
	// ConfigVersion is the on-disk configuration schema the release expects.
	ConfigVersion int `json:"config_version"`
	// StateVersion is the on-disk agent state schema the release expects. An
	// upgrade that would leave captured checkpoints unreadable is refused.
	StateVersion int        `json:"state_version"`
	Mandatory    bool       `json:"mandatory,omitempty"`
	Rollout      Rollout    `json:"rollout"`
	Artifacts    []Artifact `json:"artifacts"`
}

// SignedRelease is a release together with the detached signatures over its
// canonical bytes. Signatures live outside the release so the signed bytes do
// not depend on how many signers there were.
type SignedRelease struct {
	Release    Release     `json:"release"`
	Signatures []Signature `json:"signatures"`
}

// Signature is one detached Ed25519 signature over a release's canonical bytes.
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// Feed is the document an update server publishes. Revoked lists versions that
// must never be installed even though they were signed, which is how a bad
// release is withdrawn from machines that have not taken it yet.
type Feed struct {
	Version     int             `json:"version"`
	Channel     Channel         `json:"channel"`
	GeneratedAt time.Time       `json:"generated_at"`
	Releases    []SignedRelease `json:"releases"`
	Revoked     []string        `json:"revoked,omitempty"`
}

// feedSchemaVersion is the only feed layout this implementation reads. An
// unknown version is refused rather than guessed at.
const feedSchemaVersion = 1

// canonicalPrefix domain-separates release signatures. A signature over a
// release can then never be replayed as a signature over any other kind of
// SHIFT document.
const canonicalPrefix = "shift.dev/release/v1\n"

// Normalize trims and lowercases the fields that are compared or matched, so
// two spellings of the same release cannot produce two different canonical
// forms. Normalization is idempotent: signing and verifying both apply it.
func (release *Release) Normalize() {
	release.Version = strings.TrimPrefix(strings.TrimSpace(release.Version), "v")
	release.Channel = Channel(strings.ToLower(strings.TrimSpace(string(release.Channel))))
	release.Notes = strings.TrimSpace(release.Notes)
	release.MinimumFromVersion = strings.TrimPrefix(strings.TrimSpace(release.MinimumFromVersion), "v")
	release.PublishedAt = release.PublishedAt.UTC().Truncate(time.Second)
	for index := range release.Artifacts {
		artifact := &release.Artifacts[index]
		artifact.Component = Component(strings.ToLower(strings.TrimSpace(string(artifact.Component))))
		artifact.OS = strings.ToLower(strings.TrimSpace(artifact.OS))
		artifact.Architecture = strings.ToLower(strings.TrimSpace(artifact.Architecture))
		artifact.URL = strings.TrimSpace(artifact.URL)
		artifact.Format = Format(strings.ToLower(strings.TrimSpace(string(artifact.Format))))
		artifact.SHA256 = strings.ToLower(strings.TrimSpace(artifact.SHA256))
		artifact.BinarySHA256 = strings.ToLower(strings.TrimSpace(artifact.BinarySHA256))
		if artifact.Format == FormatBinary && artifact.BinarySHA256 == "" {
			artifact.BinarySHA256 = artifact.SHA256
		}
	}
	release.Rollout.Normalize()
}

// Validate checks a release for the things an installer must be able to rely on.
func (release *Release) Validate() error {
	release.Normalize()
	version, err := ParseVersion(release.Version)
	if err != nil {
		return fmt.Errorf("release version: %w", err)
	}
	release.Version = version.String()
	if !release.Channel.Valid() {
		return fmt.Errorf("unsupported release channel %q", release.Channel)
	}
	if release.PublishedAt.IsZero() {
		return errors.New("a release requires a publication time")
	}
	if release.MinimumFromVersion != "" {
		minimum, err := ParseVersion(release.MinimumFromVersion)
		if err != nil {
			return fmt.Errorf("release minimum_from_version: %w", err)
		}
		if version.Precedes(minimum) {
			return errors.New("minimum_from_version cannot be newer than the release itself")
		}
		release.MinimumFromVersion = minimum.String()
	}
	if release.ProtocolVersion <= 0 || release.ConfigVersion <= 0 || release.StateVersion <= 0 {
		return errors.New("a release requires positive protocol, config, and state versions")
	}
	if release.MinimumProtocolVersion <= 0 || release.MinimumProtocolVersion > release.ProtocolVersion {
		return errors.New("minimum_protocol_version must be positive and no newer than protocol_version")
	}
	if err := release.Rollout.Validate(); err != nil {
		return fmt.Errorf("release rollout: %w", err)
	}
	if len(release.Artifacts) == 0 {
		return errors.New("a release requires at least one artifact")
	}
	seen := make(map[string]bool, len(release.Artifacts))
	for index := range release.Artifacts {
		if err := release.Artifacts[index].Validate(); err != nil {
			return fmt.Errorf("release artifact %d: %w", index, err)
		}
		artifact := release.Artifacts[index]
		key := string(artifact.Component) + "/" + artifact.OS + "/" + artifact.Architecture
		if seen[key] {
			return fmt.Errorf("release has two artifacts for %s", key)
		}
		seen[key] = true
	}
	return nil
}

// Validate checks one artifact. A digest that is not a full SHA-256 hex string
// is a configuration error, not something to discover mid-download.
func (artifact *Artifact) Validate() error {
	if !artifact.Component.Valid() {
		return fmt.Errorf("unsupported component %q", artifact.Component)
	}
	if artifact.OS == "" || artifact.Architecture == "" {
		return errors.New("an artifact requires an operating system and an architecture")
	}
	if !artifact.Format.Valid() {
		return fmt.Errorf("unsupported artifact format %q", artifact.Format)
	}
	parsed, err := url.Parse(artifact.URL)
	if err != nil {
		return fmt.Errorf("artifact url: %w", err)
	}
	// An artifact is fetched over https, or from an absolute file url when the
	// fleet is air-gapped and the artifacts were copied in with the feed. Both
	// transports deliver the same bytes to the same digest check, so accepting
	// file urls does not weaken verification: the digest is what the release
	// signature covers.
	switch parsed.Scheme {
	case "https":
		if parsed.Host == "" {
			return errors.New("an https artifact url requires a host")
		}
	case "file":
		if parsed.Host != "" || !path.IsAbs(parsed.Path) {
			return errors.New("a file artifact url must be an absolute file:// path")
		}
	default:
		return errors.New("artifact url must be an absolute https or file url")
	}
	if artifact.SizeBytes <= 0 {
		return errors.New("an artifact requires a positive size so a download can be bounded")
	}
	if err := validDigest(artifact.SHA256); err != nil {
		return fmt.Errorf("artifact sha256: %w", err)
	}
	if err := validDigest(artifact.BinarySHA256); err != nil {
		return fmt.Errorf("artifact binary_sha256: %w", err)
	}
	if artifact.Format == FormatBinary && artifact.BinarySHA256 != artifact.SHA256 {
		return errors.New("an uncompressed artifact must record the same digest for its bytes and its executable")
	}
	return nil
}

func validDigest(value string) error {
	if len(value) != 2*sha256.Size {
		return fmt.Errorf("expected %d hex characters, got %d", 2*sha256.Size, len(value))
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("not hexadecimal: %w", err)
	}
	return nil
}

// SemanticVersion parses the release's own version. Validate has already
// checked it, so callers that hold a validated release can rely on the value.
func (release Release) SemanticVersion() (Version, error) {
	return ParseVersion(release.Version)
}

// ArtifactFor returns the artifact for one component and platform.
func (release Release) ArtifactFor(component Component, operatingSystem, architecture string) (Artifact, bool) {
	for _, artifact := range release.Artifacts {
		if artifact.Component == component &&
			artifact.OS == strings.ToLower(operatingSystem) &&
			artifact.Architecture == strings.ToLower(architecture) {
			return artifact, true
		}
	}
	return Artifact{}, false
}

// CanonicalBytes is the exact byte string a release signature covers: a domain
// separator followed by the deterministic JSON encoding of the normalized
// release. Signatures never cover the transport encoding, so reformatting a
// feed cannot invalidate a release.
func CanonicalBytes(release Release) ([]byte, error) {
	release.Normalize()
	encoded, err := json.Marshal(release)
	if err != nil {
		return nil, fmt.Errorf("encode release for signing: %w", err)
	}
	message := make([]byte, 0, len(canonicalPrefix)+len(encoded))
	message = append(message, canonicalPrefix...)
	message = append(message, encoded...)
	return message, nil
}

// Validate checks a feed document before any of its releases are considered.
func (feed *Feed) Validate() error {
	if feed.Version != feedSchemaVersion {
		return fmt.Errorf("unsupported update feed version %d", feed.Version)
	}
	feed.Channel = Channel(strings.ToLower(strings.TrimSpace(string(feed.Channel))))
	if !feed.Channel.Valid() {
		return fmt.Errorf("unsupported feed channel %q", feed.Channel)
	}
	for index := range feed.Revoked {
		version, err := ParseVersion(feed.Revoked[index])
		if err != nil {
			return fmt.Errorf("revoked entry %d: %w", index, err)
		}
		feed.Revoked[index] = version.String()
	}
	for index := range feed.Releases {
		if err := feed.Releases[index].Release.Validate(); err != nil {
			return fmt.Errorf("feed release %d: %w", index, err)
		}
		if feed.Releases[index].Release.Channel != feed.Channel {
			return fmt.Errorf("feed release %d belongs to channel %q, not %q",
				index, feed.Releases[index].Release.Channel, feed.Channel)
		}
		if len(feed.Releases[index].Signatures) == 0 {
			return fmt.Errorf("feed release %d carries no signature", index)
		}
	}
	return nil
}

// IsRevoked reports whether a version was withdrawn by the feed.
func (feed Feed) IsRevoked(version Version) bool {
	for _, entry := range feed.Revoked {
		revoked, err := ParseVersion(entry)
		if err != nil {
			continue
		}
		if revoked.SameRelease(version) {
			return true
		}
	}
	return false
}
