package update

import (
	"bytes"
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

// ErrBadRelease reports a deterministic failure: the release itself cannot be
// installed on this machine (a digest mismatch, a binary that will not run, or
// a binary that reports the wrong version). Retrying cannot help, so the
// manager blocks the version locally.
var ErrBadRelease = errors.New("release cannot be installed on this machine")

// ErrTransient reports a failure that says nothing about the release: a
// network blip while fetching, or disk trouble while staging or swapping. The
// same release may install cleanly on a later attempt, so the manager records
// the failure without blocking and retries on the next check.
var ErrTransient = errors.New("transient install failure")

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
	// CleanupWarning records a post-swap housekeeping problem (a backup that
	// could not be pruned, a state file that could not be written). The new
	// binary is in place either way: a cleanup warning is never an install
	// failure and never blocks the version.
	CleanupWarning string `json:"cleanup_warning,omitempty"`
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
		installer.selfTest = func(ctx context.Context, path string) (Version, error) {
			return ProbeVersion(ctx, path, string(installer.component))
		}
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
// failure at any step leaves the machine running what it ran before. Failures
// are typed: a deterministic failure (ErrBadRelease) proves the release is bad
// here, while a transient one (ErrTransient) may succeed on a retry and must
// never permanently block the version.
func (installer *Installer) Install(ctx context.Context, release Release, artifact Artifact, currentVersion string) (Installed, error) {
	expected, err := release.SemanticVersion()
	if err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrBadRelease, err.Error())
	}
	if artifact.Component != installer.component {
		return Installed{}, fmt.Errorf("%w: artifact is for component %s, this installer manages %s", ErrBadRelease, artifact.Component, installer.component)
	}
	if err := artifact.Validate(); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrBadRelease, err.Error())
	}
	if err := installer.gate.Ready(ctx); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrNotReady, err.Error())
	}
	stagingDir, err := os.MkdirTemp(installer.stagingRoot, "stage-")
	if err != nil {
		return Installed{}, fmt.Errorf("%w: create staging directory: %s", ErrTransient, err.Error())
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	staged, err := installer.stage(ctx, stagingDir, artifact)
	if err != nil {
		return Installed{}, err
	}
	reported, err := installer.selfTest(ctx, staged)
	if err != nil {
		return Installed{}, fmt.Errorf("%w: staged %s binary failed its self-check: %s", ErrBadRelease, installer.component, err.Error())
	}
	if !reported.SameRelease(expected) {
		return Installed{}, fmt.Errorf("%w: staged binary reports version %s, the release promises %s", ErrBadRelease, reported, expected)
	}
	// The gate is checked again: verifying and downloading takes time, and work
	// may have started on the machine while it ran.
	if err := installer.gate.Ready(ctx); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrNotReady, err.Error())
	}
	backup, err := installer.backupCurrent(currentVersion)
	if err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrTransient, err.Error())
	}
	if err := installer.swap(staged); err != nil {
		return Installed{}, fmt.Errorf("%w: %s", ErrTransient, err.Error())
	}
	// Everything below this line runs after the new binary is committed. A
	// failure here never reports the install as failed and never blocks the
	// version: the machine is already running (or staged to run) the new
	// release, and only housekeeping is outstanding.
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
	if err := installer.pruneBackups(); err != nil {
		installed.CleanupWarning = "backup pruning failed after the swap: " + err.Error()
	}
	return installed, nil
}

// stage downloads the artifact, checks both recorded digests, and returns the
// path of a verified executable inside the staging directory. Delivery
// failures are transient (a truncated or interrupted transfer may succeed on a
// retry); everything the signed digests already settle is deterministic.
func (installer *Installer) stage(ctx context.Context, stagingDir string, artifact Artifact) (string, error) {
	downloadPath := filepath.Join(stagingDir, "download")
	download, err := os.OpenFile(downloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("%w: create download file: %s", ErrTransient, err.Error())
	}
	digest := sha256.New()
	fetchErr := installer.fetcher.Fetch(ctx, artifact, io.MultiWriter(download, digest))
	syncErr := download.Sync()
	closeErr := download.Close()
	switch {
	case fetchErr != nil:
		// A transport failure — including a body that stops short of the size
		// the signed release records — is retryable: the truncation is
		// detected here, and a later fetch may deliver the full artifact.
		if errors.Is(fetchErr, ErrTransient) || errors.Is(fetchErr, ErrBadRelease) {
			return "", fetchErr
		}
		return "", fmt.Errorf("%w: %s", ErrTransient, fetchErr.Error())
	case syncErr != nil:
		return "", fmt.Errorf("%w: sync download: %s", ErrTransient, syncErr.Error())
	case closeErr != nil:
		return "", fmt.Errorf("%w: close download: %s", ErrTransient, closeErr.Error())
	}
	if actual := hex.EncodeToString(digest.Sum(nil)); actual != artifact.SHA256 {
		// The full promised length arrived and the bytes are wrong: the
		// artifact itself is not what the release signed. Deterministic.
		return "", fmt.Errorf("%w: downloaded artifact digest %s does not match the signed %s", ErrBadRelease, actual, artifact.SHA256)
	}
	binaryPath := filepath.Join(stagingDir, "binary")
	switch artifact.Format {
	case FormatBinary:
		if err := os.Rename(downloadPath, binaryPath); err != nil {
			return "", fmt.Errorf("%w: stage binary: %s", ErrTransient, err.Error())
		}
	case FormatGzip:
		if err := decompressGzip(downloadPath, binaryPath); err != nil {
			return "", fmt.Errorf("%w: %s", ErrBadRelease, err.Error())
		}
	default:
		return "", fmt.Errorf("%w: unsupported artifact format %q", ErrBadRelease, artifact.Format)
	}
	binaryDigest, err := fileDigest(binaryPath)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrTransient, err.Error())
	}
	if binaryDigest != artifact.BinarySHA256 {
		return "", fmt.Errorf("%w: staged executable digest %s does not match the signed %s", ErrBadRelease, binaryDigest, artifact.BinarySHA256)
	}
	if err := os.Chmod(binaryPath, executableMode); err != nil {
		return "", fmt.Errorf("%w: mark staged binary executable: %s", ErrTransient, err.Error())
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

// ProbeVersion runs one artifact's version self-report and parses it. The
// artifact is executed with --version; when that yields no parseable version
// and component names the command-line tool, the `version` subcommand is tried
// as well, because shiftgate spells it that way. Only stdout is parsed — a
// banner or warning on stderr is noise, and a date-shaped token in front of
// the real version must never be mistaken for it: the version is the last
// field the binary prints on stdout. This is the check that keeps a broken
// update from bricking a machine: a binary that will not start, or that
// reports the wrong version, is never swapped in.
func ProbeVersion(ctx context.Context, path, component string) (Version, error) {
	reported, runErr := runVersionProbe(ctx, path, "--version")
	if runErr == nil {
		if version, err := parseVersionReport(reported); err == nil {
			return version, nil
		}
	}
	if component == string(ComponentCLI) {
		if reported, runErr = runVersionProbe(ctx, path, "version"); runErr == nil {
			if version, err := parseVersionReport(reported); err == nil {
				return version, nil
			}
		}
	}
	if runErr != nil {
		return Version{}, runErr
	}
	return Version{}, errors.New("binary printed no recognizable version")
}

// runVersionProbe executes one version invocation and returns its stdout. The
// standard error stream is captured separately and discarded: parseVersionReport
// must only ever see what the binary prints deliberately.
func runVersionProbe(ctx context.Context, path string, argument string) (string, error) {
	testContext, cancel := context.WithTimeout(ctx, selfTestTimeout)
	defer cancel()
	command := exec.CommandContext(testContext, path, argument)
	command.Env = append(os.Environ(), "SHIFT_SELF_TEST=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("run %s %s: %w", filepath.Base(path), argument, err)
	}
	return stdout.String(), nil
}

// parseVersionReport reads the version out of a binary's --version output. The
// version is the last whitespace-separated field: the format every SHIFT
// binary prints is "name version", and taking the last field is what keeps a
// leading banner (a date, a build id) from being accepted as the version.
func parseVersionReport(output string) (Version, error) {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return Version{}, errors.New("no version printed")
	}
	return ParseVersion(strings.Trim(fields[len(fields)-1], "\"',():"))
}

func decompressGzip(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open compressed artifact: %w", err)
	}
	defer func() { _ = source.Close() }()
	reader, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("read compressed artifact: %w", err)
	}
	defer func() { _ = reader.Close() }()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create decompressed artifact: %w", err)
	}
	// The decompressed size is bounded too, and the bound is checked rather
	// than enforced by silent truncation: a compressed bomb must fail loudly,
	// not half-write an executable whose digest then misleads the diagnosis.
	written, err := io.Copy(target, io.LimitReader(reader, maxArtifactBytes+1))
	if err != nil {
		_ = target.Close()
		return fmt.Errorf("decompress artifact: %w", err)
	}
	if written > maxArtifactBytes {
		_ = target.Close()
		return fmt.Errorf("decompressed artifact exceeds %d bytes", maxArtifactBytes)
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
	defer func() { _ = file.Close() }()
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
	defer func() { _ = source.Close() }()
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
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}
