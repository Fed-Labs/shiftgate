// Package filesystem implements SHIFT's filesystem mobility layer: probing
// what a workload root actually sits on, taking point-in-time snapshots where
// the filesystem supports them, cloning trees copy-on-write where the kernel
// allows it, tracking which files changed between checkpoints, and discovering
// the files a workload's command depends on outside its process image.
//
// The package is honest about capability boundaries. A filesystem without
// snapshots gets no snapshot: the caller captures from the frozen workload
// root instead, and the checkpoint manifest records which of the two happened.
// A filesystem without reflink support gets a byte copy, not a silent claim of
// copy-on-write. Device nodes are only cloned when the process runs as root
// and the node is inside a workload root.
package filesystem

import (
	"fmt"
	"syscall"
)

// Filesystem magic numbers from linux/magic.h. Only the ones that change what
// SHIFT can do are listed; everything else reports as "unknown".
const (
	magicBtrfs   = 0x9123683E
	magicXFS     = 0x58465342
	magicEXT     = 0xEF53 // ext2/3/4 share this magic
	magicOverlay = 0x794C7630
	magicTmpfs   = 0x01021994
	magicZFS     = 0x2FC12FC1
)

// Filesystem describes what the kernel says about the filesystem holding a
// workload root, and what that enables.
type Filesystem struct {
	// Name is the human-readable filesystem type ("btrfs", "xfs", ...).
	Name string
	// Reflink is true when the filesystem supports FICLONE — one file may be
	// cloned from another without copying the bytes.
	Reflink bool
	// SubvolumeSnapshot is true when a workload root that is a btrfs subvolume
	// can be snapshotted atomically with `btrfs subvolume snapshot -r`.
	SubvolumeSnapshot bool
}

// Probe asks the kernel what filesystem holds the path.
func Probe(path string) (Filesystem, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return Filesystem{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	switch uint64(stat.Type) {
	case magicBtrfs:
		return Filesystem{Name: "btrfs", Reflink: true, SubvolumeSnapshot: true}, nil
	case magicXFS:
		return Filesystem{Name: "xfs", Reflink: true}, nil
	case magicEXT:
		return Filesystem{Name: "ext4", Reflink: false}, nil
	case magicOverlay:
		return Filesystem{Name: "overlay", Reflink: false}, nil
	case magicTmpfs:
		return Filesystem{Name: "tmpfs", Reflink: false}, nil
	case magicZFS:
		return Filesystem{Name: "zfs", Reflink: false}, nil
	default:
		return Filesystem{Name: fmt.Sprintf("unknown-0x%x", uint64(stat.Type))}, nil
	}
}
