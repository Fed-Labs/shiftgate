package update

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/persistence"
)

// SkipBlocked marks a release this machine refuses locally, because installing
// it already failed here or because it was rolled back. It is a local decision,
// unlike the skip codes derived from the release itself.
const SkipBlocked = "BLOCKED_ON_THIS_MACHINE"

const (
	stateSchemaVersion   = 1
	defaultCheckInterval = 6 * time.Hour
	maximumHistory       = 100
)

// MinimumCheckInterval is the shortest interval an update manager will poll a
// feed on. Checking more often than this cannot make a fleet meaningfully more
// current, and a fleet that polls a release server every few seconds is
// indistinguishable from one attacking it.
const MinimumCheckInterval = 15 * time.Minute

// Policy decides how much the machine may do on its own.
type Policy string

const (
	// PolicyManual checks for releases and reports them; an operator applies them.
	PolicyManual Policy = "manual"
	// PolicyMandatory applies only releases the publisher marked mandatory,
	// which is the setting for fleets that want security fixes without
	// unattended feature upgrades.
	PolicyMandatory Policy = "mandatory"
	// PolicyAutomatic applies every applicable release once its rollout stage
	// reaches this machine.
	PolicyAutomatic Policy = "automatic"
)

// Valid reports whether the policy is one this package implements.
func (policy Policy) Valid() bool {
	switch policy {
	case PolicyManual, PolicyMandatory, PolicyAutomatic:
		return true
	default:
		return false
	}
}

// EventKind names what happened in the update history.
type EventKind string

const (
	EventChecked    EventKind = "checked"
	EventInstalled  EventKind = "installed"
	EventRolledBack EventKind = "rolled_back"
	EventFailed     EventKind = "failed"
	EventDeferred   EventKind = "deferred"
)

// Event is one entry in the machine's update history. Failures are recorded with
// the same weight as successes: an update that did not happen is the thing an
// operator most often needs to explain.
type Event struct {
	At          time.Time `json:"at"`
	Kind        EventKind `json:"kind"`
	Component   Component `json:"component"`
	FromVersion string    `json:"from_version,omitempty"`
	ToVersion   string    `json:"to_version,omitempty"`
	Code        string    `json:"code,omitempty"`
	Detail      string    `json:"detail,omitempty"`
}

// BlockedVersion is a release this machine will not install again without an
// explicit instruction.
type BlockedVersion struct {
	Version   string    `json:"version"`
	BlockedAt time.Time `json:"blocked_at"`
	Reason    string    `json:"reason"`
}

// State is the persisted update state. It survives restarts so a machine that
// crashed mid-update does not forget that a version misbehaved here.
type State struct {
	SchemaVersion  int        `json:"schema_version"`
	Component      Component  `json:"component"`
	LastCheckedAt  *time.Time `json:"last_checked_at,omitempty"`
	LastCheckError string     `json:"last_check_error,omitempty"`
	LastInstalled  *Installed `json:"last_installed,omitempty"`
	// PendingVersion is a version that is installed on disk but is not the
	// version this process is running, because a binary swap only takes effect
	// when the service restarts. It stops an automatic loop from installing the
	// same release over and over while waiting for that restart.
	PendingVersion string           `json:"pending_version,omitempty"`
	Blocked        []BlockedVersion `json:"blocked,omitempty"`
	History        []Event          `json:"history,omitempty"`
}

// CheckResult is the outcome of consulting the release feed.
type CheckResult struct {
	CheckedAt      time.Time    `json:"checked_at"`
	Component      Component    `json:"component"`
	CurrentVersion string       `json:"current_version"`
	PendingVersion string       `json:"pending_version,omitempty"`
	Channel        Channel      `json:"channel"`
	Policy         Policy       `json:"policy"`
	Available      *Evaluation  `json:"available,omitempty"`
	Evaluations    []Evaluation `json:"evaluations"`
}

// Status is the full update picture for one component, including what an
// operator needs to decide whether to intervene.
type Status struct {
	Component       Component        `json:"component"`
	CurrentVersion  string           `json:"current_version"`
	PendingVersion  string           `json:"pending_version,omitempty"`
	RestartRequired bool             `json:"restart_required,omitempty"`
	Channel         Channel          `json:"channel"`
	Policy          Policy           `json:"policy"`
	ExecutablePath  string           `json:"executable_path,omitempty"`
	LastCheckedAt   *time.Time       `json:"last_checked_at,omitempty"`
	LastCheckError  string           `json:"last_check_error,omitempty"`
	NextCheckAt     *time.Time       `json:"next_check_at,omitempty"`
	Available       *Evaluation      `json:"available,omitempty"`
	Evaluations     []Evaluation     `json:"evaluations,omitempty"`
	LastInstalled   *Installed       `json:"last_installed,omitempty"`
	Blocked         []BlockedVersion `json:"blocked,omitempty"`
	Backups         []Backup         `json:"backups,omitempty"`
	History         []Event          `json:"history,omitempty"`
}

// ManagerConfig configures the update manager.
type ManagerConfig struct {
	Installation  Installation
	Source        FeedSource
	KeyRing       *KeyRing
	Installer     *Installer
	StatePath     string
	Policy        Policy
	CheckInterval time.Duration
	Now           func() time.Time
	Logger        *slog.Logger
	// PeerProtocolVersion reports the protocol version the control plane
	// currently speaks. It is a function rather than a number because a control
	// plane can be upgraded underneath a long-running agent, and a release must
	// be judged against what the control plane requires now. A nil function, or
	// one that returns zero, means this machine has no control plane to answer
	// to and the protocol check does not apply.
	PeerProtocolVersion func() int
}

// Manager ties the feed, the trust decision, and the installer together, and
// keeps the durable record of what this machine did.
type Manager struct {
	mutex         sync.Mutex
	installation  Installation
	source        FeedSource
	ring          *KeyRing
	installer     *Installer
	statePath     string
	policy        Policy
	checkInterval time.Duration
	now           func() time.Time
	logger        *slog.Logger
	peerProtocol  func() int
	state         State
	lastCheck     *CheckResult
}

// NewManager validates the configuration and loads any persisted state.
func NewManager(configuration ManagerConfig) (*Manager, error) {
	installation := configuration.Installation
	if err := installation.Validate(); err != nil {
		return nil, err
	}
	if configuration.Source == nil {
		return nil, errors.New("update manager requires a release feed source")
	}
	if configuration.KeyRing == nil {
		return nil, errors.New("update manager requires a trusted key ring")
	}
	if configuration.Installer == nil {
		return nil, errors.New("update manager requires an installer")
	}
	if configuration.Installer.component != installation.Component {
		return nil, fmt.Errorf("installer manages %s but the installation describes %s", configuration.Installer.component, installation.Component)
	}
	if !filepath.IsAbs(configuration.StatePath) {
		return nil, errors.New("update manager requires an absolute state path")
	}
	policy := configuration.Policy
	if policy == "" {
		policy = PolicyManual
	}
	if !policy.Valid() {
		return nil, fmt.Errorf("unsupported update policy %q", policy)
	}
	interval := configuration.CheckInterval
	if interval <= 0 {
		interval = defaultCheckInterval
	}
	if interval < MinimumCheckInterval {
		return nil, fmt.Errorf("check interval must be at least %s", MinimumCheckInterval)
	}
	manager := &Manager{
		installation:  installation,
		source:        configuration.Source,
		ring:          configuration.KeyRing,
		installer:     configuration.Installer,
		statePath:     configuration.StatePath,
		policy:        policy,
		checkInterval: interval,
		now:           configuration.Now,
		logger:        configuration.Logger,
		peerProtocol:  configuration.PeerProtocolVersion,
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	if manager.logger == nil {
		manager.logger = slog.Default()
	}
	if err := manager.load(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (manager *Manager) load() error {
	if err := os.MkdirAll(filepath.Dir(manager.statePath), 0o700); err != nil {
		return fmt.Errorf("create update state directory: %w", err)
	}
	var state State
	err := persistence.ReadJSON(manager.statePath, &state)
	switch {
	case errors.Is(err, os.ErrNotExist):
		manager.state = State{SchemaVersion: stateSchemaVersion, Component: manager.installation.Component}
		return nil
	case err != nil:
		return fmt.Errorf("read update state: %w", err)
	}
	if state.SchemaVersion != stateSchemaVersion {
		return fmt.Errorf("unsupported update state schema %d", state.SchemaVersion)
	}
	if state.Component != "" && state.Component != manager.installation.Component {
		return fmt.Errorf("update state belongs to component %s, not %s", state.Component, manager.installation.Component)
	}
	state.Component = manager.installation.Component
	if state.PendingVersion != "" && sameVersion(state.PendingVersion, manager.installation.Version) {
		// The service restarted into the staged version, so it is no longer pending.
		state.PendingVersion = ""
	}
	manager.state = state
	return nil
}

// sameVersion compares two version strings by semantic precedence, so build
// metadata differences do not read as a different release.
func sameVersion(first, second string) bool {
	left, leftErr := ParseVersion(first)
	right, rightErr := ParseVersion(second)
	if leftErr != nil || rightErr != nil {
		return strings.TrimSpace(first) == strings.TrimSpace(second)
	}
	return left.SameRelease(right)
}

// effectiveInstallation is the installation releases are evaluated against. When
// a newer version is already staged on disk awaiting a restart, that version is
// what a release must beat: otherwise an automatic loop would install the same
// release again every time it checked.
func (manager *Manager) effectiveInstallation() Installation {
	installation := manager.installation
	if manager.peerProtocol != nil {
		if peer := manager.peerProtocol(); peer > 0 {
			installation.PeerProtocolVersion = peer
		}
	}
	if manager.state.PendingVersion == "" {
		return installation
	}
	running, runningErr := ParseVersion(installation.Version)
	pending, pendingErr := ParseVersion(manager.state.PendingVersion)
	if runningErr == nil && pendingErr == nil && running.Precedes(pending) {
		installation.Version = pending.String()
	}
	return installation
}

// persist writes the state file. The caller holds the mutex.
func (manager *Manager) persist() error {
	if len(manager.state.History) > maximumHistory {
		manager.state.History = manager.state.History[len(manager.state.History)-maximumHistory:]
	}
	if err := persistence.WriteJSON(manager.statePath, manager.state, 0o600); err != nil {
		return fmt.Errorf("write update state: %w", err)
	}
	return nil
}

// record appends one history entry. The caller holds the mutex.
func (manager *Manager) record(event Event) {
	event.At = manager.now().UTC()
	event.Component = manager.installation.Component
	manager.state.History = append(manager.state.History, event)
}

// Check consults the feed and evaluates every release against this machine.
func (manager *Manager) Check(ctx context.Context) (CheckResult, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	return manager.checkLocked(ctx)
}

func (manager *Manager) checkLocked(ctx context.Context) (CheckResult, error) {
	checkedAt := manager.now().UTC()
	result := CheckResult{
		CheckedAt:      checkedAt,
		Component:      manager.installation.Component,
		CurrentVersion: manager.installation.Version,
		PendingVersion: manager.state.PendingVersion,
		Channel:        manager.installation.Channel,
		Policy:         manager.policy,
	}
	feed, err := manager.source.Feed(ctx)
	if err != nil {
		manager.state.LastCheckedAt = &checkedAt
		manager.state.LastCheckError = err.Error()
		manager.record(Event{Kind: EventFailed, Code: "FEED_UNAVAILABLE", Detail: err.Error()})
		if persistErr := manager.persist(); persistErr != nil {
			return CheckResult{}, persistErr
		}
		return CheckResult{}, err
	}
	evaluations, err := Evaluate(feed, manager.effectiveInstallation(), manager.ring, checkedAt)
	if err != nil {
		manager.state.LastCheckedAt = &checkedAt
		manager.state.LastCheckError = err.Error()
		if persistErr := manager.persist(); persistErr != nil {
			return CheckResult{}, persistErr
		}
		return CheckResult{}, err
	}
	evaluations = manager.applyBlocks(evaluations)
	result.Evaluations = evaluations
	if selected, ok := Select(evaluations); ok {
		available := selected
		result.Available = &available
	}
	manager.state.LastCheckedAt = &checkedAt
	manager.state.LastCheckError = ""
	detail := "no newer release"
	if result.Available != nil {
		detail = result.Available.Version
	}
	manager.record(Event{Kind: EventChecked, FromVersion: manager.installation.Version, ToVersion: detail})
	if err := manager.persist(); err != nil {
		return CheckResult{}, err
	}
	stored := result
	manager.lastCheck = &stored
	return result, nil
}

// applyBlocks turns locally blocked releases into inapplicable ones. The caller
// holds the mutex.
func (manager *Manager) applyBlocks(evaluations []Evaluation) []Evaluation {
	if len(manager.state.Blocked) == 0 {
		return evaluations
	}
	blocked := make(map[string]string, len(manager.state.Blocked))
	for _, entry := range manager.state.Blocked {
		blocked[entry.Version] = entry.Reason
	}
	for index, evaluation := range evaluations {
		reason, found := blocked[evaluation.Version]
		if !found || !evaluation.Applicable {
			continue
		}
		evaluations[index] = evaluation.skip(SkipBlocked, reason)
	}
	return evaluations
}

// Apply installs a release. An empty version installs whatever the current check
// selects; a specific version installs only that one, so an operator can pin an
// upgrade without racing the feed.
func (manager *Manager) Apply(ctx context.Context, version string) (Installed, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	result, err := manager.checkLocked(ctx)
	if err != nil {
		return Installed{}, err
	}
	chosen, err := manager.choose(result, strings.TrimSpace(version))
	if err != nil {
		return Installed{}, err
	}
	onDisk := manager.effectiveInstallation().Version
	installed, err := manager.installer.Install(ctx, chosen.Release(), *chosen.Artifact, onDisk)
	if err != nil {
		if errors.Is(err, ErrNotReady) {
			manager.record(Event{Kind: EventDeferred, FromVersion: manager.installation.Version, ToVersion: chosen.Version, Code: "BUSY", Detail: err.Error()})
			if persistErr := manager.persist(); persistErr != nil {
				return Installed{}, persistErr
			}
			return Installed{}, err
		}
		manager.blockLocked(chosen.Version, err.Error())
		manager.record(Event{Kind: EventFailed, FromVersion: manager.installation.Version, ToVersion: chosen.Version, Code: "INSTALL_FAILED", Detail: err.Error()})
		if persistErr := manager.persist(); persistErr != nil {
			return Installed{}, persistErr
		}
		return Installed{}, err
	}
	record := installed
	manager.state.LastInstalled = &record
	manager.state.PendingVersion = ""
	if !sameVersion(installed.Version, manager.installation.Version) {
		manager.state.PendingVersion = installed.Version
	}
	manager.record(Event{Kind: EventInstalled, FromVersion: installed.PreviousVersion, ToVersion: installed.Version, Detail: installed.BackupID})
	if err := manager.persist(); err != nil {
		return Installed{}, err
	}
	manager.logger.Info("update installed",
		"component", string(installed.Component),
		"from", installed.PreviousVersion,
		"to", installed.Version,
		"backup", installed.BackupID)
	return installed, nil
}

// choose resolves the requested version against the evaluations, returning a
// typed explanation when the request cannot be honored.
func (manager *Manager) choose(result CheckResult, version string) (Evaluation, error) {
	if version == "" {
		if result.Available == nil {
			return Evaluation{}, errors.New("no applicable release is available")
		}
		return *result.Available, nil
	}
	requested, err := ParseVersion(version)
	if err != nil {
		return Evaluation{}, err
	}
	for _, evaluation := range result.Evaluations {
		candidate, parseErr := ParseVersion(evaluation.Version)
		if parseErr != nil || !candidate.SameRelease(requested) {
			continue
		}
		if !evaluation.Applicable {
			return Evaluation{}, fmt.Errorf("release %s is not applicable here: %s (%s)", evaluation.Version, evaluation.Reason, evaluation.Code)
		}
		return evaluation, nil
	}
	return Evaluation{}, fmt.Errorf("release %s is not in the %s feed", requested, manager.installation.Channel)
}

// Rollback reinstalls the previous binary and blocks the version being undone so
// the automatic loop does not immediately reinstall it.
func (manager *Manager) Rollback(ctx context.Context, backupID string) (Installed, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	backups, err := manager.installer.Backups()
	if err != nil {
		return Installed{}, err
	}
	if len(backups) == 0 {
		return Installed{}, errors.New("no preserved binary is available to roll back to")
	}
	target := backups[0]
	if trimmed := strings.TrimSpace(backupID); trimmed != "" {
		found := false
		for _, backup := range backups {
			if backup.ID == trimmed {
				target, found = backup, true
				break
			}
		}
		if !found {
			return Installed{}, fmt.Errorf("no preserved binary with identifier %s", trimmed)
		}
	}
	current := manager.effectiveInstallation().Version
	installed, err := manager.installer.Rollback(ctx, target, current)
	if err != nil {
		manager.record(Event{Kind: EventFailed, FromVersion: current, ToVersion: target.Version, Code: "ROLLBACK_FAILED", Detail: err.Error()})
		if persistErr := manager.persist(); persistErr != nil {
			return Installed{}, persistErr
		}
		return Installed{}, err
	}
	if current != "" && current != installed.Version {
		manager.blockLocked(current, "rolled back on this machine")
	}
	record := installed
	manager.state.LastInstalled = &record
	manager.state.PendingVersion = ""
	if !sameVersion(installed.Version, manager.installation.Version) {
		manager.state.PendingVersion = installed.Version
	}
	manager.record(Event{Kind: EventRolledBack, FromVersion: current, ToVersion: installed.Version, Detail: target.ID})
	if err := manager.persist(); err != nil {
		return Installed{}, err
	}
	manager.logger.Warn("update rolled back",
		"component", string(installed.Component),
		"from", current,
		"to", installed.Version,
		"backup", target.ID)
	return installed, nil
}

// Block refuses a version on this machine until it is explicitly unblocked.
func (manager *Manager) Block(version, reason string) error {
	parsed, err := ParseVersion(version)
	if err != nil {
		return err
	}
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.blockLocked(parsed.String(), strings.TrimSpace(reason))
	return manager.persist()
}

// Unblock allows a previously blocked version again.
func (manager *Manager) Unblock(version string) error {
	parsed, err := ParseVersion(version)
	if err != nil {
		return err
	}
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	kept := make([]BlockedVersion, 0, len(manager.state.Blocked))
	removed := false
	for _, entry := range manager.state.Blocked {
		if entry.Version == parsed.String() {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return fmt.Errorf("version %s is not blocked", parsed)
	}
	manager.state.Blocked = kept
	return manager.persist()
}

// blockLocked records a blocked version. The caller holds the mutex.
func (manager *Manager) blockLocked(version, reason string) {
	if version == "" {
		return
	}
	if reason == "" {
		reason = "blocked on this machine"
	}
	for index, entry := range manager.state.Blocked {
		if entry.Version == version {
			manager.state.Blocked[index].Reason = reason
			manager.state.Blocked[index].BlockedAt = manager.now().UTC()
			return
		}
	}
	manager.state.Blocked = append(manager.state.Blocked, BlockedVersion{
		Version:   version,
		BlockedAt: manager.now().UTC(),
		Reason:    reason,
	})
	sort.SliceStable(manager.state.Blocked, func(first, second int) bool {
		return manager.state.Blocked[first].Version < manager.state.Blocked[second].Version
	})
}

// Status reports everything known about updates for this component.
func (manager *Manager) Status() Status {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	status := Status{
		Component:       manager.installation.Component,
		CurrentVersion:  manager.installation.Version,
		PendingVersion:  manager.state.PendingVersion,
		RestartRequired: manager.state.PendingVersion != "",
		Channel:         manager.installation.Channel,
		Policy:          manager.policy,
		ExecutablePath:  manager.installer.ExecutablePath(),
		LastCheckedAt:   manager.state.LastCheckedAt,
		LastCheckError:  manager.state.LastCheckError,
		LastInstalled:   manager.state.LastInstalled,
		Blocked:         manager.state.Blocked,
	}
	if manager.state.LastCheckedAt != nil {
		next := manager.state.LastCheckedAt.Add(manager.checkInterval)
		status.NextCheckAt = &next
	}
	if manager.lastCheck != nil {
		status.Available = manager.lastCheck.Available
		status.Evaluations = manager.lastCheck.Evaluations
	}
	if backups, err := manager.installer.Backups(); err == nil {
		status.Backups = backups
	}
	if length := len(manager.state.History); length > 0 {
		history := make([]Event, length)
		copy(history, manager.state.History)
		sort.SliceStable(history, func(first, second int) bool {
			return history[first].At.After(history[second].At)
		})
		status.History = history
	}
	return status
}

// Policy is the configured automation level.
func (manager *Manager) Policy() Policy {
	return manager.policy
}

// Run checks periodically and, when the policy allows, applies what it finds. It
// returns when the context is canceled. Failures are logged and recorded rather
// than ending the loop, because a machine that cannot reach the feed today must
// still try tomorrow.
func (manager *Manager) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		manager.runOnce(ctx)
		timer.Reset(manager.checkInterval)
	}
}

func (manager *Manager) runOnce(ctx context.Context) {
	result, err := manager.Check(ctx)
	if err != nil {
		if ctx.Err() == nil {
			manager.logger.Warn("update check failed", "component", string(manager.installation.Component), "error", err)
		}
		return
	}
	if result.Available == nil || manager.policy == PolicyManual {
		return
	}
	if manager.policy == PolicyMandatory && !result.Available.Mandatory {
		manager.logger.Info("update available but not mandatory",
			"component", string(manager.installation.Component),
			"version", result.Available.Version)
		return
	}
	if _, err := manager.Apply(ctx, result.Available.Version); err != nil {
		if errors.Is(err, ErrNotReady) {
			manager.logger.Info("update deferred while the machine is busy",
				"component", string(manager.installation.Component),
				"version", result.Available.Version)
			return
		}
		if ctx.Err() == nil {
			manager.logger.Error("automatic update failed",
				"component", string(manager.installation.Component),
				"version", result.Available.Version,
				"error", err)
		}
	}
}
