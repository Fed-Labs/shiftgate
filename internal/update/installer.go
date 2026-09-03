package update

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"shift.dev/shift/internal/persistence"
)

// ErrNotReady reports that the machine is doing work an update must not
// interrupt. It is not a failure: the caller retries later.
var ErrNotReady = errors.New("machine is not ready for an update")

// selfTestTimeout bounds the version self-check of a staged binary. A binary
// that cannot answer within this window is treated as broken and never installed.
const selfTestTimeout = 30 * time.Second

// defaultKeptBackups is how many previous versions stay on disk. More than one
// means a machine can roll back past a bad release to the version before it.
const defaultKeptBackups = 3

// executableMode is the permission a staged binary is installed with.
const executableMode os.FileMode = 0o755

// Gate decides whether an update may interrupt the machine right now. The agent
// implements it over its own in-flight work, which is what makes upgrades
// migration-safe: a swap never happens underneath a running migration,
// checkpoint, or restore.
type Gate interface {
	Ready(ctx context.Context) error
}

// GateFunc adapts a function to the Gate interface.
type GateFunc func(ctx context.Context) error

// Ready implements Gate.
func (gate GateFunc) Ready(ctx context.Context) error {
	return gate(ctx)
}

// AlwaysReady is the gate for installations with nothing to interrupt, such as
// the command-line tool updating itself.
func AlwaysReady() Gate {
	return GateFunc(func(context.Context) error { return nil })
}

// Backup is one preserved previous binary. Rollback works from these, so a
// broken release can be undone without network access.
type Backup struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	SavedAt   time.Time `json:"saved_at"`
	SHA256    string    `json:"sha256"`
	Component Component `json:"component"`
	path      string
}

// Path is the location of the preserved binary.
func (backup Backup) Path() string {
	return backup.path
}

// Installed records a completed swap.
type Installed struct {
	Component       Component `json:"component"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	InstalledAt     time.Time `json:"installed_at"`
	ExecutablePath  string    `json:"executable_path"`
	BackupID        string    `json:"backup_id,omitempty"`
	SHA256          string    `json:"sha256"`
}

// InstallerConfig configures an installer.
type InstallerConfig struct {
	Component      Component
	ExecutablePath string
	StagingRoot    string
	BackupRoot     string
	Fetcher        Fetcher
	Gate           Gate
	KeepBackups    int
	Now            func() time.Time
	// SelfTest runs a staged binary and reports the version it claims. The
	// default executes the binary with --version. It is replaceable so a
	// component whose executable is not a command-line program can supply its
	// own check, never to skip the check.
	SelfTest func(ctx context.Context, path string) (Version, error)
}

// Installer performs verified, reversible binary swaps.
type Installer struct {
	component      Component
	executablePath string
	stagingRoot    string
	backupRoot     string
	fetcher        Fetcher
	gate           Gate
	keepBackups    int
	now            func() time.Time
	selfTest       func(ctx context.Context, path string) (Version, error)
}

// NewInstaller validates the configuration and prepares the staging and backup
// directories.
func NewInstaller(configuration InstallerConfig) (*Installer, error) {
	if !configuration.Component.Valid() {
		return nil, fmt.Errorf("unsupported component %q", configuration.Component)
	}
	if !filepath.IsAbs(configuration.ExecutablePath) {
		return nil, errors.New("installer requires an absolute executable path")
	}
	if !filepath.IsAbs(configuration.StagingRoot) || !filepath.IsAbs(configuration.BackupRoot) {
		return nil, errors.New("installer requires absolute staging and backup directories")
	}
	if configuration.Fetcher == nil {
		return nil, errors.New("installer requires an artifact fetcher")
	}
	installer := &Installer{
		component:      configuration.Component,
		executablePath: configuration.ExecutablePath,
		stagingRoot:    configuration.StagingRoot,
		backupRoot:     configuration.BackupRoot,
		fetcher:        configuration.Fetcher,
		gate:           configuration.Gate,
		keepBackups:    configuration.KeepBackups,
		now:            configuration.Now,
		selfTest:       configuration.SelfTest,
	}
	if installer.gate == nil {
		installer.gate = AlwaysReady()
	}
	if installer.keepBackups <= 0 {
		installer.keepBackups = defaultKeptBackups
	}
	if installer.now == nil {
		installer.now = time.Now
	}
	if installer.selfTest == nil {
		installer.selfTest = versionSelfTest
	}
	for _, directory := range []string{installer.stagingRoot, installer.backupRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", directory, err)
		}
	}
	return installer, nil
}

// ExecutablePath is the binary this installer replaces.
func (installer *Installer) ExecutablePath() string {
	return installer.executablePath
}

// Install downloads, verifies, self-tests, and swaps in one artifact. Nothing is
// swapped until the staged binary has proved that it runs and reports the version
// the signed release promised, and the previous binary is preserved first, so a
// failure at any step leaves the machine running what it ran before.
func (installer *Installer) Install(ctx context.Context, release Release, artifact Artifact, currentVersion string) (Installed, error) {
	expected, err := release.SemanticVersion()
	if err != nil {
		return Installed{}, err
	}
	if artifact.Component != installer.component {
		return Installed{}, fmt.Errorf("artifact is for component %s, this installer manages %s", artifact.Component, installer.component)
	}
	if err := artifact.Validate(); err != nil {
		return Installed{}, err
	}
	if err := installer.gate.Ready(ctx); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrNotReady, err.Error())
	}
	stagingDir, err := os.MkdirTemp(installer.stagingRoot, "stage-")
	if err != nil {
		return Installed{}, fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	staged, err := installer.stage(ctx, stagingDir, artifact)
	if err != nil {
		return Installed{}, err
	}
	reported, err := installer.selfTest(ctx, staged)
	if err != nil {
		return Installed{}, fmt.Errorf("staged %s binary failed its self-check: %w", installer.component, err)
	}
	if !reported.SameRelease(expected) {
		return Installed{}, fmt.Errorf("staged binary reports version %s, the release promises %s", reported, expected)
	}
	// The gate is checked again: verifying and downloading takes time, and work
	// may have started on the machine while it ran.
	if err := installer.gate.Ready(ctx); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrNotReady, err.Error())
	}
	backup, err := installer.backupCurrent(currentVersion)
	if err != nil {
		return Installed{}, err
	}
	if err := installer.swap(staged); err != nil {
		return Installed{}, err
	}
	if err := installer.pruneBackups(); err != nil {
		return Installed{}, err
	}
	installed := Installed{
		Component:       installer.component,
		Version:         expected.String(),
		PreviousVersion: strings.TrimSpace(currentVersion),
		InstalledAt:     installer.now().UTC(),
		ExecutablePath:  installer.executablePath,
		SHA256:          artifact.BinarySHA256,
	}
	if backup != nil {
		installed.BackupID = backup.ID
	}
	return installed, nil
}

// stage downloads the artifact, checks both recorded digests, and returns the
// path of a verified executable inside the staging directory.
func (installer *Installer) stage(ctx context.Context, stagingDir string, artifact Artifact) (string, error) {
	downloadPath := filepath.Join(stagingDir, "download")
	download, err := os.OpenFile(downloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create download file: %w", err)
	}
	digest := sha256.New()
	fetchErr := installer.fetcher.Fetch(ctx, artifact, io.MultiWriter(download, digest))
	syncErr := download.Sync()
	closeErr := download.Close()
	switch {
	case fetchErr != nil:
		return "", fetchErr
	case syncErr != nil:
		return "", fmt.Errorf("sync download: %w", syncErr)
	case closeErr != nil:
		return "", fmt.Errorf("close download: %w", closeErr)
	}
	if actual := hex.EncodeToString(digest.Sum(nil)); actual != artifact.SHA256 {
		return "", fmt.Errorf("downloaded artifact digest %s does not match the signed %s", actual, artifact.SHA256)
	}
	binaryPath := filepath.Join(stagingDir, "binary")
	switch artifact.Format {
	case FormatBinary:
		if err := os.Rename(downloadPath, binaryPath); err != nil {
			return "", fmt.Errorf("stage binary: %w", err)
		}
	case FormatGzip:
		if err := decompressGzip(downloadPath, binaryPath); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unsupported artifact format %q", artifact.Format)
	}
	binaryDigest, err := fileDigest(binaryPath)
	if err != nil {
		return "", err
	}
	if binaryDigest != artifact.BinarySHA256 {
		return "", fmt.Errorf("staged executable digest %s does not match the signed %s", binaryDigest, artifact.BinarySHA256)
	}
	if err := os.Chmod(binaryPath, executableMode); err != nil {
		return "", fmt.Errorf("mark staged binary executable: %w", err)
	}
	return binaryPath, nil
}

// backupCurrent preserves the binary that is about to be replaced. A machine
// with no binary yet is a first install, which has nothing to preserve.
func (installer *Installer) backupCurrent(currentVersion string) (*Backup, error) {
	information, err := os.Stat(installer.executablePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect current executable: %w", err)
	}
	if !information.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", installer.executablePath)
	}
	digest, err := fileDigest(installer.executablePath)
	if err != nil {
		return nil, err
	}
	savedAt := installer.now().UTC()
	version := strings.TrimSpace(currentVersion)
	if version == "" {
		version = "unknown"
	}
	identifier := savedAt.Format("20060102T150405Z") + "-" + digest[:12]
	directory := filepath.Join(installer.backupRoot, identifier)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create backup directory: %w", err)
	}
	target := filepath.Join(directory, filepath.Base(installer.executablePath))
	if err := copyFile(installer.executablePath, target, executableMode); err != nil {
		return nil, fmt.Errorf("preserve current executable: %w", err)
	}
	backup := Backup{
		ID:        identifier,
		Version:   version,
		SavedAt:   savedAt,
		SHA256:    digest,
		Component: installer.component,
		path:      target,
	}
	if err := persistence.WriteJSON(filepath.Join(directory, "backup.json"), backup, 0o600); err != nil {
		return nil, err
	}
	return &backup, nil
}

// swap installs the staged binary in place. The staged file is copied next to
// its destination first so the final step is a rename within one directory,
// which either happens or does not: there is no window in which the executable
// is half-written.
func (installer *Installer) swap(stagedPath string) error {
	destination := installer.executablePath
	if resolved, err := filepath.EvalSymlinks(destination); err == nil {
		destination = resolved
	}
	directory := filepath.Dir(destination)
	pending := filepath.Join(directory, "."+filepath.Base(destination)+".shift-update")
	if err := copyFile(stagedPath, pending, executableMode); err != nil {
		return fmt.Errorf("place staged binary: %w", err)
	}
	if err := os.Rename(pending, destination); err != nil {
		_ = os.Remove(pending)
		return fmt.Errorf("commit staged binary: %w", err)
	}
	return syncDirectory(directory)
}

// Backups lists preserved binaries, newest first.
func (installer *Installer) Backups() ([]Backup, error) {
	entries, err := os.ReadDir(installer.backupRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backup directory: %w", err)
	}
	backups := make([]Backup, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var backup Backup
		metadataPath := filepath.Join(installer.backupRoot, entry.Name(), "backup.json")
		if err := persistence.ReadJSON(metadataPath, &backup); err != nil {
			// A backup whose metadata is unreadable is not offered for
			// rollback rather than aborting the listing.
			continue
		}
		backup.path = filepath.Join(installer.backupRoot, entry.Name(), filepath.Base(installer.executablePath))
		if information, err := os.Stat(backup.path); err != nil || !information.Mode().IsRegular() {
			continue
		}
		backups = append(backups, backup)
	}
	sort.SliceStable(backups, func(first, second int) bool {
		return backups[first].SavedAt.After(backups[second].SavedAt)
	})
	return backups, nil
}

// Rollback reinstalls a preserved binary. The preserved binary is digest-checked
// and self-tested exactly like a downloaded one, because a corrupt backup would
// otherwise turn a bad update into an unusable machine.
func (installer *Installer) Rollback(ctx context.Context, backup Backup, currentVersion string) (Installed, error) {
	if backup.path == "" {
		return Installed{}, errors.New("backup has no recorded path")
	}
	digest, err := fileDigest(backup.path)
	if err != nil {
		return Installed{}, err
	}
	if digest != backup.SHA256 {
		return Installed{}, fmt.Errorf("preserved binary digest %s does not match the recorded %s", digest, backup.SHA256)
	}
	if err := installer.gate.Ready(ctx); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrNotReady, err.Error())
	}
	reported, err := installer.selfTest(ctx, backup.path)
	if err != nil {
		return Installed{}, fmt.Errorf("preserved binary failed its self-check: %w", err)
	}
	if backup.Version != "unknown" {
		recorded, parseErr := ParseVersion(backup.Version)
		if parseErr == nil && !recorded.SameRelease(reported) {
			return Installed{}, fmt.Errorf("preserved binary reports version %s, the backup records %s", reported, recorded)
		}
	}
	if _, err := installer.backupCurrent(currentVersion); err != nil {
		return Installed{}, err
	}
	if err := installer.swap(backup.path); err != nil {
		return Installed{}, err
	}
	return Installed{
		Component:       installer.component,
		Version:         reported.String(),
		PreviousVersion: strings.TrimSpace(currentVersion),
		InstalledAt:     installer.now().UTC(),
		ExecutablePath:  installer.executablePath,
		BackupID:        backup.ID,
		SHA256:          digest,
	}, nil
}

func (installer *Installer) pruneBackups() error {
	backups, err := installer.Backups()
	if err != nil {
		return err
	}
	for index, backup := range backups {
		if index < installer.keepBackups {
			continue
		}
		if err := os.RemoveAll(filepath.Join(installer.backupRoot, backup.ID)); err != nil {
			return fmt.Errorf("prune backup %s: %w", backup.ID, err)
		}
	}
	return nil
}

// versionSelfTest runs a staged binary with --version and reads the version it
// prints. This is the check that keeps a broken update from bricking a machine:
// a binary that will not start, or that reports the wrong version, is never
// swapped in.
func versionSelfTest(ctx context.Context, path string) (Version, error) {
	testContext, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	command := exec.CommandContext(testContext, path, "--version")
	command.Env = append(os.Environ(), "SHIFT_SELF_TEST=1")
	output, err := command.CombinedOutput()
	if err != nil {
		return Version{}, fmt.Errorf("run %s --version: %w", filepath.Base(path), err)
	}
	for _, field := range strings.Fields(string(output)) {
		if version, err := ParseVersion(strings.Trim(field, "\"',()")); err == nil {
			return version, nil
		}
	}
	return Version{}, errors.New("binary printed no recognizable version")
}

func decompressGzip(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open compressed artifact: %w", err)
	}
	defer source.Close()
	reader, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("read compressed artifact: %w", err)
	}
	defer reader.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create decompressed artifact: %w", err)
	}
	if _, err := io.Copy(target, io.LimitReader(reader, maxArtifactBytes)); err != nil {
		_ = target.Close()
		return fmt.Errorf("decompress artifact: %w", err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return fmt.Errorf("sync decompressed artifact: %w", err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close decompressed artifact: %w", err)
	}
	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func copyFile(sourcePath, targetPath string, mode os.FileMode) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open %s: %w", sourcePath, err)
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", targetPath, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy to %s: %w", targetPath, err)
	}
	if err := target.Chmod(mode); err != nil {
		_ = target.Close()
		return fmt.Errorf("set mode on %s: %w", targetPath, err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return fmt.Errorf("sync %s: %w", targetPath, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close %s: %w", targetPath, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", path, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}
