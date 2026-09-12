package update

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// managerFixture is a machine with an installed agent, a signed feed, and a
// manager wired to both.
type managerFixture struct {
	installer *installerFixture
	signer    *Signer
	ring      *KeyRing
	manager   *Manager
	statePath string
	feed      Feed
	feedError error
	feedCalls int
}

func newManagerFixture(t *testing.T, runningVersion string, policy Policy, gate Gate) *managerFixture {
	t.Helper()
	fixture := &managerFixture{}
	fixture.installer = newInstallerFixture(t, versionScript("shift-agent", runningVersion), gate, 0)
	fixture.signer = newSigner(t)
	fixture.ring = keyRingFor(t, 1, fixture.signer)
	fixture.statePath = filepath.Join(fixture.installer.root, "state", "updates.json")
	fixture.feed = Feed{Version: feedSchemaVersion, Channel: ChannelStable, GeneratedAt: testNow}
	fixture.manager = fixture.newManager(t, runningVersion, policy)
	return fixture
}

func (fixture *managerFixture) newManager(t *testing.T, runningVersion string, policy Policy) *Manager {
	t.Helper()
	manager, err := NewManager(ManagerConfig{
		Installation: installationFor(ComponentAgent, runningVersion, fixture.installer.executablePath),
		Source: feedFunc(func(context.Context) (Feed, error) {
			fixture.feedCalls++
			if fixture.feedError != nil {
				return Feed{}, fixture.feedError
			}
			return fixture.feed, nil
		}),
		KeyRing:       fixture.ring,
		Installer:     fixture.installer.installer,
		StatePath:     fixture.statePath,
		Policy:        policy,
		CheckInterval: time.Hour,
		Now:           func() time.Time { return fixture.installer.clock },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build update manager: %v", err)
	}
	return manager
}

// publish adds a signed release for a payload the fetcher will serve.
func (fixture *managerFixture) publish(t *testing.T, version string, payload []byte, adjust func(*Release)) {
	t.Helper()
	url := "https://releases.example/agent-" + version
	artifact := binaryArtifact(ComponentAgent, url, payload)
	fixture.installer.fetcher.add(url, payload)
	release := releaseFor(version, artifact)
	if adjust != nil {
		adjust(&release)
	}
	signed, err := fixture.signer.SignRelease(release)
	if err != nil {
		t.Fatalf("sign release %s: %v", version, err)
	}
	fixture.feed.Releases = append(fixture.feed.Releases, signed)
}

func historyKinds(status Status) []EventKind {
	kinds := make([]EventKind, 0, len(status.History))
	for _, event := range status.History {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

func containsKind(status Status, kind EventKind) bool {
	for _, event := range status.History {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func TestManagerCheckReportsWhatIsAvailable(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)

	result, err := fixture.manager.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if result.Available == nil || result.Available.Version != "1.1.0" {
		t.Fatalf("expected 1.1.0 to be available, got %+v", result.Available)
	}
	status := fixture.manager.Status()
	if status.LastCheckedAt == nil || !status.LastCheckedAt.Equal(testNow) {
		t.Fatalf("expected the check time to be recorded, got %+v", status.LastCheckedAt)
	}
	if status.NextCheckAt == nil || !status.NextCheckAt.Equal(testNow.Add(time.Hour)) {
		t.Fatalf("expected the next check to be scheduled, got %+v", status.NextCheckAt)
	}
	if !containsKind(status, EventChecked) {
		t.Fatalf("expected a checked event, got %v", historyKinds(status))
	}
	if status.RestartRequired {
		t.Fatal("a machine that has not installed anything does not need a restart")
	}
}

func TestManagerAppliesAnAvailableRelease(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	payload := versionScript("shift-agent", "1.1.0")
	fixture.publish(t, "1.1.0", payload, nil)

	installed, err := fixture.manager.Apply(context.Background(), "")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if installed.Version != "1.1.0" {
		t.Fatalf("expected 1.1.0 to be installed, got %s", installed.Version)
	}
	if fixture.installer.installed(t) != string(payload) {
		t.Fatal("the new binary is not on disk")
	}
	status := fixture.manager.Status()
	if !containsKind(status, EventInstalled) {
		t.Fatalf("expected an installed event, got %v", historyKinds(status))
	}
	if status.PendingVersion != "1.1.0" || !status.RestartRequired {
		t.Fatalf("a swapped binary takes effect on restart; status says %+v", status)
	}
	if len(status.Backups) != 1 {
		t.Fatalf("expected one preserved binary, got %d", len(status.Backups))
	}
}

func TestManagerDoesNotReinstallAStagedVersionWhileWaitingForARestart(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyAutomatic, nil)
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)

	if _, err := fixture.manager.Apply(context.Background(), ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	downloadsAfterFirst := fixture.installer.fetcher.calls

	result, err := fixture.manager.Check(context.Background())
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if result.Available != nil {
		t.Fatalf("the staged version must not be offered again, got %+v", result.Available)
	}
	fixture.manager.runOnce(context.Background())
	if fixture.installer.fetcher.calls != downloadsAfterFirst {
		t.Fatal("the automatic loop reinstalled a version that is already staged")
	}
}

func TestManagerForgetsThePendingVersionOnceItIsRunning(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)
	if _, err := fixture.manager.Apply(context.Background(), ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The service restarts into the new version.
	restarted := fixture.newManager(t, "1.1.0", PolicyManual)
	status := restarted.Status()
	if status.PendingVersion != "" || status.RestartRequired {
		t.Fatalf("a restarted service is no longer pending, status says %+v", status)
	}
	if len(status.History) == 0 {
		t.Fatal("history must survive a restart")
	}
}

func TestManagerBlocksAReleaseThatFailedToInstall(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyAutomatic, nil)
	previous := versionScript("shift-agent", "1.0.0")
	fixture.publish(t, "1.1.0", brokenScript(), nil)

	if _, err := fixture.manager.Apply(context.Background(), ""); err == nil {
		t.Fatal("a broken release must not install")
	}
	if fixture.installer.installed(t) != string(previous) {
		t.Fatal("the machine must still run the previous binary")
	}
	status := fixture.manager.Status()
	if len(status.Blocked) != 1 || status.Blocked[0].Version != "1.1.0" {
		t.Fatalf("expected 1.1.0 to be blocked locally, got %+v", status.Blocked)
	}
	if !containsKind(status, EventFailed) {
		t.Fatalf("expected a failure to be recorded, got %v", historyKinds(status))
	}

	// A second attempt must not retry the release that just broke.
	result, err := fixture.manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Available != nil {
		t.Fatalf("a blocked release must not be offered, got %+v", result.Available)
	}
	if len(result.Evaluations) != 1 || result.Evaluations[0].Code != SkipBlocked {
		t.Fatalf("expected %s, got %+v", SkipBlocked, result.Evaluations)
	}
	if _, err := fixture.manager.Apply(context.Background(), "1.1.0"); err == nil {
		t.Fatal("applying a blocked release must be refused")
	}
	downloads := fixture.installer.fetcher.calls
	fixture.manager.runOnce(context.Background())
	if fixture.installer.fetcher.calls != downloads {
		t.Fatal("the automatic loop retried a blocked release")
	}
}

func TestManagerUnblockAllowsARetry(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	payload := versionScript("shift-agent", "1.1.0")
	fixture.publish(t, "1.1.0", payload, nil)
	if err := fixture.manager.Block("1.1.0", "operator hold"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Apply(context.Background(), "1.1.0"); err == nil {
		t.Fatal("a held release must not install")
	}
	if err := fixture.manager.Unblock("1.2.0"); err == nil {
		t.Fatal("unblocking a version that is not blocked must be an error")
	}
	if err := fixture.manager.Unblock("1.1.0"); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if _, err := fixture.manager.Apply(context.Background(), "1.1.0"); err != nil {
		t.Fatalf("apply after unblocking: %v", err)
	}
	if fixture.installer.installed(t) != string(payload) {
		t.Fatal("the release was not installed after being unblocked")
	}
}

func TestManagerRollbackBlocksTheVersionItUndid(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyAutomatic, nil)
	previous := versionScript("shift-agent", "1.0.0")
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)
	if _, err := fixture.manager.Apply(context.Background(), ""); err != nil {
		t.Fatalf("apply: %v", err)
	}
	fixture.installer.clock = fixture.installer.clock.Add(time.Minute)

	restored, err := fixture.manager.Rollback(context.Background(), "")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if restored.Version != "1.0.0" {
		t.Fatalf("expected 1.0.0 to be restored, got %s", restored.Version)
	}
	if fixture.installer.installed(t) != string(previous) {
		t.Fatal("the previous binary was not restored")
	}
	status := fixture.manager.Status()
	if !containsKind(status, EventRolledBack) {
		t.Fatalf("expected a rollback to be recorded, got %v", historyKinds(status))
	}
	blocked := false
	for _, entry := range status.Blocked {
		if entry.Version == "1.1.0" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("the rolled-back version must not be reinstalled automatically, blocked=%+v", status.Blocked)
	}
	downloads := fixture.installer.fetcher.calls
	fixture.manager.runOnce(context.Background())
	if fixture.installer.fetcher.calls != downloads {
		t.Fatal("the automatic loop reinstalled a rolled-back release")
	}
}

func TestManagerRollbackNeedsAPreservedBinary(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	if _, err := fixture.manager.Rollback(context.Background(), ""); err == nil {
		t.Fatal("rolling back with nothing preserved must be an error")
	}
	if _, err := fixture.manager.Rollback(context.Background(), "no-such-backup"); err == nil {
		t.Fatal("rolling back to an unknown backup must be an error")
	}
}

func TestManagerDefersWhileTheMachineIsBusyWithoutBlockingTheRelease(t *testing.T) {
	gate := GateFunc(func(context.Context) error {
		return errors.New("migration mig-7 is restoring")
	})
	fixture := newManagerFixture(t, "1.0.0", PolicyAutomatic, gate)
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)

	_, err := fixture.manager.Apply(context.Background(), "")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected %v, got %v", ErrNotReady, err)
	}
	status := fixture.manager.Status()
	if !containsKind(status, EventDeferred) {
		t.Fatalf("expected a deferred event, got %v", historyKinds(status))
	}
	if len(status.Blocked) != 0 {
		t.Fatalf("a deferred release must not be blocked, got %+v", status.Blocked)
	}
	if status.RestartRequired {
		t.Fatal("nothing was installed, so no restart is required")
	}
}

func TestManagerPolicyDecidesWhatTheLoopApplies(t *testing.T) {
	optional := newManagerFixture(t, "1.0.0", PolicyMandatory, nil)
	optional.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)
	optional.manager.runOnce(context.Background())
	if optional.installer.fetcher.calls != 0 {
		t.Fatal("a mandatory-only policy must not apply an optional release")
	}

	required := newManagerFixture(t, "1.0.0", PolicyMandatory, nil)
	payload := versionScript("shift-agent", "1.1.0")
	required.publish(t, "1.1.0", payload, func(release *Release) { release.Mandatory = true })
	required.manager.runOnce(context.Background())
	if required.installer.installed(t) != string(payload) {
		t.Fatal("a mandatory release must be applied under the mandatory policy")
	}

	manual := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	manual.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), func(release *Release) { release.Mandatory = true })
	manual.manager.runOnce(context.Background())
	if manual.installer.fetcher.calls != 0 {
		t.Fatal("a manual policy must never install on its own")
	}
	if status := manual.manager.Status(); status.Available == nil {
		t.Fatal("a manual policy still reports what is available")
	}
}

func TestManagerRefusesAnUnknownOrInapplicableRequest(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), func(release *Release) {
		release.Rollout = Rollout{Stages: []RolloutStage{
			{StartsAt: testNow.Add(-time.Hour), Percent: 0},
			{StartsAt: testNow.Add(time.Hour), Percent: 100},
		}}
	})
	if _, err := fixture.manager.Apply(context.Background(), "2.0.0"); err == nil {
		t.Fatal("a version that is not in the feed must be refused")
	}
	if _, err := fixture.manager.Apply(context.Background(), "1.1.0"); err == nil {
		t.Fatal("a release whose rollout has not reached this machine must be refused")
	}
	if _, err := fixture.manager.Apply(context.Background(), "not-a-version"); err == nil {
		t.Fatal("a malformed version must be refused")
	}
	if _, err := fixture.manager.Apply(context.Background(), ""); err == nil {
		t.Fatal("applying with nothing applicable must be an error, not a silent success")
	}
	if fixture.installer.fetcher.calls != 0 {
		t.Fatal("no artifact should have been downloaded")
	}
}

func TestManagerRecordsAnUnreachableFeed(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyAutomatic, nil)
	fixture.feedError = errors.New("dial releases.example: connection refused")

	if _, err := fixture.manager.Check(context.Background()); err == nil {
		t.Fatal("an unreachable feed must be reported")
	}
	status := fixture.manager.Status()
	if status.LastCheckError == "" {
		t.Fatal("the failure must be recorded for an operator to see")
	}
	if !containsKind(status, EventFailed) {
		t.Fatalf("expected a failure event, got %v", historyKinds(status))
	}
	// The loop keeps working once the feed comes back.
	fixture.feedError = nil
	payload := versionScript("shift-agent", "1.1.0")
	fixture.publish(t, "1.1.0", payload, nil)
	fixture.manager.runOnce(context.Background())
	if fixture.installer.installed(t) != string(payload) {
		t.Fatal("a transient feed failure must not stop later updates")
	}
	if fixture.manager.Status().LastCheckError != "" {
		t.Fatal("a successful check must clear the recorded failure")
	}
}

func TestManagerStateSurvivesRestartsAndRejectsForeignState(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	if err := fixture.manager.Block("1.9.0", "known bad on this hardware"); err != nil {
		t.Fatal(err)
	}
	reopened := fixture.newManager(t, "1.0.0", PolicyManual)
	status := reopened.Status()
	if len(status.Blocked) != 1 || status.Blocked[0].Version != "1.9.0" {
		t.Fatalf("blocked versions must survive a restart, got %+v", status.Blocked)
	}

	var state State
	if err := readState(fixture.statePath, &state); err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != stateSchemaVersion {
		t.Fatalf("unexpected schema version %d", state.SchemaVersion)
	}
	state.Component = ComponentDesktop
	if err := writeState(fixture.statePath, state); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(ManagerConfig{
		Installation: installationFor(ComponentAgent, "1.0.0", fixture.installer.executablePath),
		Source:       feedFunc(func(context.Context) (Feed, error) { return fixture.feed, nil }),
		KeyRing:      fixture.ring,
		Installer:    fixture.installer.installer,
		StatePath:    fixture.statePath,
		Policy:       PolicyManual,
	}); err == nil {
		t.Fatal("state belonging to another component must be refused")
	}
}

func TestManagerConfigurationIsValidated(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	base := ManagerConfig{
		Installation: installationFor(ComponentAgent, "1.0.0", fixture.installer.executablePath),
		Source:       feedFunc(func(context.Context) (Feed, error) { return fixture.feed, nil }),
		KeyRing:      fixture.ring,
		Installer:    fixture.installer.installer,
		StatePath:    filepath.Join(fixture.installer.root, "state", "other.json"),
		Policy:       PolicyManual,
	}
	withoutRing := base
	withoutRing.KeyRing = nil
	if _, err := NewManager(withoutRing); err == nil {
		t.Fatal("a manager without a key ring must be refused")
	}
	relativeState := base
	relativeState.StatePath = "updates.json"
	if _, err := NewManager(relativeState); err == nil {
		t.Fatal("a relative state path must be refused")
	}
	badPolicy := base
	badPolicy.Policy = Policy("whenever")
	if _, err := NewManager(badPolicy); err == nil {
		t.Fatal("an unknown policy must be refused")
	}
	tooFrequent := base
	tooFrequent.CheckInterval = time.Second
	if _, err := NewManager(tooFrequent); err == nil {
		t.Fatal("an unreasonably short check interval must be refused")
	}
	wrongComponent := base
	wrongComponent.Installation = installationFor(ComponentDesktop, "1.0.0", fixture.installer.executablePath)
	if _, err := NewManager(wrongComponent); err == nil {
		t.Fatal("an installation and installer for different components must be refused")
	}
	if _, err := NewManager(base); err != nil {
		t.Fatalf("a valid configuration must load: %v", err)
	}
}

func TestManagerRunStopsWithItsContext(t *testing.T) {
	fixture := newManagerFixture(t, "1.0.0", PolicyManual, nil)
	fixture.publish(t, "1.1.0", versionScript("shift-agent", "1.1.0"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		fixture.manager.Run(ctx)
		close(finished)
	}()
	deadline := time.After(5 * time.Second)
	for fixture.manager.Status().LastCheckedAt == nil {
		select {
		case <-deadline:
			t.Fatal("the update loop never performed its first check")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the update loop did not stop when its context was canceled")
	}
}

// readState and writeState read and rewrite the persisted state file directly, so
// a test can check what was stored and simulate a corrupted or foreign file.
func readState(path string, into *State) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeJSON(content, into)
}

func writeState(path string, state State) error {
	encoded, err := encodeJSON(state)
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}
