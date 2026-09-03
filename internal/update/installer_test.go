package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installerFixture is a machine with a binary installed and an installer wired to
// replace it.
type installerFixture struct {
	root           string
	executablePath string
	installer      *Installer
	fetcher        *stubFetcher
	clock          time.Time
}

func newInstallerFixture(t *testing.T, current []byte, gate Gate, keepBackups int) *installerFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &installerFixture{
		root:           root,
		executablePath: filepath.Join(root, "bin", "shift-agent"),
		fetcher:        newStubFetcher(),
		clock:          testNow,
	}
	if current != nil {
		writeExecutable(t, fixture.executablePath, current)
	} else if err := os.MkdirAll(filepath.Dir(fixture.executablePath), 0o755); err != nil {
		t.Fatalf("create bin directory: %v", err)
	}
	installer, err := NewInstaller(InstallerConfig{
		Component:      ComponentAgent,
		ExecutablePath: fixture.executablePath,
		StagingRoot:    filepath.Join(root, "staging"),
		BackupRoot:     filepath.Join(root, "backups"),
		Fetcher:        fixture.fetcher,
		Gate:           gate,
		KeepBackups:    keepBackups,
		Now:            func() time.Time { return fixture.clock },
	})
	if err != nil {
		t.Fatalf("build installer: %v", err)
	}
	fixture.installer = installer
	return fixture
}

func (fixture *installerFixture) installed(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(fixture.executablePath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	return string(content)
}

// publish registers an uncompressed artifact and returns the release carrying it.
func (fixture *installerFixture) publish(version string, payload []byte) (Release, Artifact) {
	url := "https://releases.example/agent-" + version
	artifact := binaryArtifact(ComponentAgent, url, payload)
	fixture.fetcher.add(url, payload)
	release := releaseFor(version, artifact)
	return release, artifact
}

func TestInstallSwapsTheBinaryAndPreservesThePrevious(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	fixture := newInstallerFixture(t, previous, nil, 0)
	payload := versionScript("shift-agent", "1.1.0")
	release, artifact := fixture.publish("1.1.0", payload)

	installed, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if fixture.installed(t) != string(payload) {
		t.Fatal("the new binary was not installed")
	}
	if installed.Version != "1.1.0" || installed.PreviousVersion != "1.0.0" {
		t.Fatalf("unexpected install record %+v", installed)
	}
	if installed.SHA256 != artifact.BinarySHA256 {
		t.Fatalf("install record digest %s does not match the artifact", installed.SHA256)
	}
	information, err := os.Stat(fixture.executablePath)
	if err != nil {
		t.Fatal(err)
	}
	if information.Mode().Perm() != 0o755 {
		t.Fatalf("installed binary has mode %s", information.Mode().Perm())
	}
	backups, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected the previous binary to be preserved, got %d backups", len(backups))
	}
	if backups[0].Version != "1.0.0" || backups[0].ID != installed.BackupID {
		t.Fatalf("unexpected backup %+v", backups[0])
	}
	preserved, err := os.ReadFile(backups[0].Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != string(previous) {
		t.Fatal("the preserved binary is not the one that was replaced")
	}
	entries, err := os.ReadDir(filepath.Join(fixture.root, "staging"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging directory was not cleaned up: %v", entries)
	}
}

func TestInstallAcceptsGzipArtifacts(t *testing.T) {
	fixture := newInstallerFixture(t, versionScript("shift-agent", "1.0.0"), nil, 0)
	payload := versionScript("shift-agent", "1.2.0")
	artifact, compressed := gzipArtifact(t, ComponentAgent, "https://releases.example/agent-1.2.0.gz", payload)
	fixture.fetcher.add(artifact.URL, compressed)
	release := releaseFor("1.2.0", artifact)

	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err != nil {
		t.Fatalf("install a compressed artifact: %v", err)
	}
	if fixture.installed(t) != string(payload) {
		t.Fatal("the decompressed binary was not installed")
	}
}

func TestInstallRefusesADownloadWhoseDigestDoesNotMatch(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	fixture := newInstallerFixture(t, previous, nil, 0)
	promised := versionScript("shift-agent", "1.1.0")
	substituted := versionScript("shift-agent", "1.1.9")
	if len(promised) != len(substituted) {
		t.Fatal("the test payloads must be the same length so the digest is what fails")
	}
	release, artifact := fixture.publish("1.1.0", promised)
	fixture.fetcher.add(artifact.URL, substituted)

	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err == nil {
		t.Fatal("a substituted download must be refused")
	}
	if fixture.installed(t) != string(previous) {
		t.Fatal("a refused install must leave the running binary in place")
	}
	backups, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 0 {
		t.Fatal("nothing should have been replaced, so nothing should have been backed up")
	}
}

func TestInstallRefusesAGzipArtifactWhoseExecutableDigestDoesNotMatch(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	fixture := newInstallerFixture(t, previous, nil, 0)
	payload := versionScript("shift-agent", "1.2.0")
	artifact, compressed := gzipArtifact(t, ComponentAgent, "https://releases.example/agent-1.2.0.gz", payload)
	artifact.BinarySHA256 = digestOf(brokenScript())
	fixture.fetcher.add(artifact.URL, compressed)
	release := releaseFor("1.2.0", artifact)

	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err == nil {
		t.Fatal("a decompressed executable that does not match its digest must be refused")
	}
	if fixture.installed(t) != string(previous) {
		t.Fatal("a refused install must leave the running binary in place")
	}
}

func TestInstallRefusesABinaryThatWillNotRun(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	fixture := newInstallerFixture(t, previous, nil, 0)
	release, artifact := fixture.publish("1.1.0", brokenScript())

	_, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0")
	if err == nil {
		t.Fatal("a binary that fails its self-check must not be installed")
	}
	if fixture.installed(t) != string(previous) {
		t.Fatal("a broken release must leave the machine running the previous binary")
	}
}

func TestInstallRefusesABinaryReportingAnotherVersion(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	fixture := newInstallerFixture(t, previous, nil, 0)
	// The payload is a valid, runnable binary, but it is not the version the
	// signed release promised.
	release, artifact := fixture.publish("1.1.0", versionScript("shift-agent", "1.4.0"))

	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err == nil {
		t.Fatal("a binary reporting the wrong version must not be installed")
	}
	if fixture.installed(t) != string(previous) {
		t.Fatal("the previous binary must still be in place")
	}
}

func TestInstallIsRefusedWhileTheMachineIsBusy(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	busy := GateFunc(func(context.Context) error {
		return errors.New("migration mig-1 is transferring")
	})
	fixture := newInstallerFixture(t, previous, busy, 0)
	release, artifact := fixture.publish("1.1.0", versionScript("shift-agent", "1.1.0"))

	_, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0")
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected %v, got %v", ErrNotReady, err)
	}
	if fixture.fetcher.calls != 0 {
		t.Fatal("a busy machine should not even download the artifact")
	}
	if fixture.installed(t) != string(previous) {
		t.Fatal("the previous binary must still be in place")
	}
}

func TestFirstInstallHasNothingToPreserve(t *testing.T) {
	fixture := newInstallerFixture(t, nil, nil, 0)
	payload := versionScript("shift-agent", "1.0.0")
	release, artifact := fixture.publish("1.0.0", payload)

	installed, err := fixture.installer.Install(context.Background(), release, artifact, "")
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if installed.BackupID != "" {
		t.Fatalf("a first install has no backup, got %q", installed.BackupID)
	}
	if fixture.installed(t) != string(payload) {
		t.Fatal("the binary was not installed")
	}
}

func TestRollbackRestoresThePreviousBinary(t *testing.T) {
	previous := versionScript("shift-agent", "1.0.0")
	fixture := newInstallerFixture(t, previous, nil, 0)
	release, artifact := fixture.publish("1.1.0", versionScript("shift-agent", "1.1.0"))
	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err != nil {
		t.Fatalf("install: %v", err)
	}
	fixture.clock = fixture.clock.Add(time.Minute)

	backups, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := fixture.installer.Rollback(context.Background(), backups[0], "1.1.0")
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if restored.Version != "1.0.0" || restored.PreviousVersion != "1.1.0" {
		t.Fatalf("unexpected rollback record %+v", restored)
	}
	if fixture.installed(t) != string(previous) {
		t.Fatal("rollback did not restore the previous binary")
	}
	// The rollback preserved what it replaced, so the machine can go forward again.
	after, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("expected the replaced 1.1.0 binary to be preserved too, got %d backups", len(after))
	}
	if after[0].Version != "1.1.0" {
		t.Fatalf("expected the newest backup to be 1.1.0, got %+v", after[0])
	}
}

func TestRollbackRefusesACorruptedBackup(t *testing.T) {
	fixture := newInstallerFixture(t, versionScript("shift-agent", "1.0.0"), nil, 0)
	payload := versionScript("shift-agent", "1.1.0")
	release, artifact := fixture.publish("1.1.0", payload)
	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err != nil {
		t.Fatalf("install: %v", err)
	}
	backups, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backups[0].Path(), brokenScript(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.installer.Rollback(context.Background(), backups[0], "1.1.0"); err == nil {
		t.Fatal("a corrupted backup must not be installed")
	}
	if fixture.installed(t) != string(payload) {
		t.Fatal("a refused rollback must leave the current binary in place")
	}
}

func TestRollbackIsRefusedWhileTheMachineIsBusy(t *testing.T) {
	var busy bool
	gate := GateFunc(func(context.Context) error {
		if busy {
			return errors.New("checkpoint in progress")
		}
		return nil
	})
	fixture := newInstallerFixture(t, versionScript("shift-agent", "1.0.0"), gate, 0)
	payload := versionScript("shift-agent", "1.1.0")
	release, artifact := fixture.publish("1.1.0", payload)
	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err != nil {
		t.Fatalf("install: %v", err)
	}
	backups, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	busy = true
	if _, err := fixture.installer.Rollback(context.Background(), backups[0], "1.1.0"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expected %v, got %v", ErrNotReady, err)
	}
	if fixture.installed(t) != string(payload) {
		t.Fatal("a deferred rollback must not change the installed binary")
	}
}

func TestOldBackupsArePruned(t *testing.T) {
	fixture := newInstallerFixture(t, versionScript("shift-agent", "1.0.0"), nil, 1)
	for _, version := range []string{"1.1.0", "1.2.0", "1.3.0"} {
		release, artifact := fixture.publish(version, versionScript("shift-agent", version))
		if _, err := fixture.installer.Install(context.Background(), release, artifact, "previous"); err != nil {
			t.Fatalf("install %s: %v", version, err)
		}
		fixture.clock = fixture.clock.Add(time.Minute)
	}
	backups, err := fixture.installer.Backups()
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected one preserved binary, got %d", len(backups))
	}
	preserved, err := os.ReadFile(backups[0].Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != string(versionScript("shift-agent", "1.2.0")) {
		t.Fatal("pruning kept the wrong binary")
	}
}

func TestInstallFollowsASymlinkedExecutablePath(t *testing.T) {
	root := t.TempDir()
	realPath := filepath.Join(root, "versions", "shift-agent")
	linkPath := filepath.Join(root, "bin", "shift-agent")
	previous := versionScript("shift-agent", "1.0.0")
	writeExecutable(t, realPath, previous)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	fetcher := newStubFetcher()
	installer, err := NewInstaller(InstallerConfig{
		Component:      ComponentAgent,
		ExecutablePath: linkPath,
		StagingRoot:    filepath.Join(root, "staging"),
		BackupRoot:     filepath.Join(root, "backups"),
		Fetcher:        fetcher,
		Now:            fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := versionScript("shift-agent", "1.1.0")
	url := "https://releases.example/agent-1.1.0"
	artifact := binaryArtifact(ComponentAgent, url, payload)
	fetcher.add(url, payload)
	if _, err := installer.Install(context.Background(), releaseFor("1.1.0", artifact), artifact, "1.0.0"); err != nil {
		t.Fatalf("install through a symlink: %v", err)
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("the symlink must survive the swap: %v", err)
	}
	if target != realPath {
		t.Fatalf("the symlink now points at %s", target)
	}
	content, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(payload) {
		t.Fatal("the file behind the symlink was not replaced")
	}
}

func TestInstallRefusesAnArtifactForAnotherComponent(t *testing.T) {
	fixture := newInstallerFixture(t, versionScript("shift-agent", "1.0.0"), nil, 0)
	payload := versionScript("shift-desktop", "1.1.0")
	url := "https://releases.example/desktop-1.1.0"
	artifact := binaryArtifact(ComponentDesktop, url, payload)
	fixture.fetcher.add(url, payload)
	release := releaseFor("1.1.0", artifact)

	if _, err := fixture.installer.Install(context.Background(), release, artifact, "1.0.0"); err == nil {
		t.Fatal("a desktop artifact must not be installed over the agent")
	}
}

func TestInstallerRequiresAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	_, err := NewInstaller(InstallerConfig{
		Component:      ComponentAgent,
		ExecutablePath: "shift-agent",
		StagingRoot:    filepath.Join(root, "staging"),
		BackupRoot:     filepath.Join(root, "backups"),
		Fetcher:        newStubFetcher(),
	})
	if err == nil {
		t.Fatal("a relative executable path must be refused")
	}
	if _, err := NewInstaller(InstallerConfig{
		Component:      ComponentAgent,
		ExecutablePath: filepath.Join(root, "shift-agent"),
		StagingRoot:    filepath.Join(root, "staging"),
		BackupRoot:     filepath.Join(root, "backups"),
	}); err == nil {
		t.Fatal("an installer without a fetcher must be refused")
	}
}
