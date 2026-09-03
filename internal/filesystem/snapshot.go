package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Snapshot is a point-in-time read-only view of a workload root. Releasing it
// deletes the snapshot; forgetting to release leaks disk space, so callers
// always pair Create with a deferred Release.
type Snapshot struct {
	// subvolume is the read-only snapshot itself, named after the workload
	// root's base name so an archive taken from its parent directory is
	// byte-for-byte laid out like an archive taken from the live root.
	subvolume string
	// wrapper is the directory the subvolume lives in.
	wrapper string
}

// Path is the snapshot's view of the workload root.
func (snapshot *Snapshot) Path() string {
	return snapshot.subvolume
}

// ArchiveParent is the directory an archiver should pack from: it contains
// exactly one entry, the workload root's base name, which is the snapshot.
func (snapshot *Snapshot) ArchiveParent() string {
	return snapshot.wrapper
}

// Release deletes the snapshot and its wrapper directory. A read-only
// snapshot on old btrfs-progs must be made writable before deletion; that
// retry keeps cleanup working across versions instead of silently leaking
// the snapshot.
func (snapshot *Snapshot) Release() error {
	if snapshot.subvolume == "" {
		return nil
	}
	if err := runBtrfs("subvolume", "delete", snapshot.subvolume); err != nil {
		if !strings.Contains(err.Error(), "read-only") {
			return err
		}
		if err := runBtrfs("property", "set", snapshot.subvolume, "ro", "false"); err != nil {
			return err
		}
		if err := runBtrfs("subvolume", "delete", snapshot.subvolume); err != nil {
			return err
		}
	}
	return os.Remove(snapshot.wrapper)
}

// SnapshotProvider creates atomic point-in-time snapshots of a directory. A
// machine whose filesystems cannot snapshot has no provider, and SHIFT
// captures from the frozen workload root instead — never a torn archive.
type SnapshotProvider interface {
	// Name identifies the mechanism in the checkpoint manifest.
	Name() string
	// Create snapshots source under a sibling wrapper directory named after
	// id, which must be unique while the snapshot exists.
	Create(ctx context.Context, source, id string) (Snapshot, error)
}

// btrfsProvider snapshots btrfs subvolumes. btrfs subvolume snapshots are
// atomic and copy-on-write: the snapshot costs nothing but metadata and is a
// consistent view of the whole tree at one instant, which is what lets SHIFT
// resume the workload before the (potentially slow) archive is even read.
type btrfsProvider struct{}

func (btrfsProvider) Name() string { return "btrfs-subvolume" }

func (btrfsProvider) Create(ctx context.Context, source, id string) (Snapshot, error) {
	if strings.TrimSpace(id) == "" {
		return Snapshot{}, errors.New("snapshot id is required")
	}
	source = filepath.Clean(source)
	base := filepath.Base(source)
	wrapper := filepath.Join(filepath.Dir(source), ".shift-snapshot-"+id)
	if _, err := os.Lstat(wrapper); err == nil {
		return Snapshot{}, fmt.Errorf("snapshot destination %s already exists", wrapper)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	if err := os.Mkdir(wrapper, 0o700); err != nil {
		return Snapshot{}, err
	}
	subvolume := filepath.Join(wrapper, base)
	command := exec.CommandContext(ctx, "btrfs", "subvolume", "snapshot", "-r", source, subvolume)
	output, err := command.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(wrapper)
		return Snapshot{}, fmt.Errorf("btrfs subvolume snapshot: %w: %s", err, firstLine(output))
	}
	return Snapshot{subvolume: subvolume, wrapper: wrapper}, nil
}

func runBtrfs(arguments ...string) error {
	output, err := exec.Command("btrfs", arguments...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("btrfs %s: %w: %s", strings.Join(arguments, " "), err, firstLine(output))
	}
	return nil
}

func firstLine(output []byte) string {
	text := strings.TrimSpace(string(output))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

// DetectSnapshotProvider returns the snapshot provider for the probed
// filesystem, or nil when the filesystem has no snapshot mechanism SHIFT can
// drive. A btrfs filesystem without btrfs-progs installed is reported as
// unusable rather than silently degraded: an operator who put workloads on
// btrfs for the snapshot capability should learn the tooling is missing.
func DetectSnapshotProvider(fs Filesystem) SnapshotProvider {
	if !fs.SubvolumeSnapshot {
		return nil
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		return nil
	}
	return btrfsProvider{}
}
