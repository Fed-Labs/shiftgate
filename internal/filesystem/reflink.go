package filesystem

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ficlone is the Linux FICLONE ioctl: clone one open file into another
// copy-on-write, sharing the underlying extents instead of copying bytes.
// It is the same request cp --reflink issues.
const ficlone = 0x40049409

// CloneFile copies source to destination, cloning extents when the filesystem
// supports it and falling back to a byte copy when it does not. It reports
// whether the fast path was taken, so callers can log real copy-on-write
// usage rather than assume it. The destination must not exist.
func CloneFile(source, destination string) (cloned bool, err error) {
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer func() { _ = input.Close() }()
	info, err := input.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", source)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return false, err
	}
	// The explicit Sync below is what makes the copy durable; the close after
	// it cannot reveal anything the sync has not already surfaced.
	defer func() { _ = output.Close() }()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, output.Fd(), ficlone, input.Fd())
	switch errno {
	case 0:
		return true, output.Chmod(info.Mode().Perm())
	case syscall.ENOTSUP, syscall.EINVAL, syscall.EXDEV, syscall.EPERM:
		// The filesystem cannot share extents. The destination was created
		// empty by the open above, so a plain copy reproduces the file.
		if _, err = io.Copy(output, input); err != nil {
			return false, err
		}
		if err := output.Chmod(info.Mode().Perm()); err != nil {
			return false, err
		}
		if err := output.Sync(); err != nil {
			return false, err
		}
		return false, nil
	default:
		return false, errno
	}
}

// CloneTreeStats reports what a tree clone actually did, so a caller can tell
// the difference between "everything was copy-on-write" and "everything was
// copied" instead of assuming the cheaper one.
type CloneTreeStats struct {
	Files          int
	ClonedFiles    int
	Symlinks       int
	HardLinks      int
	Directories    int
	DeviceNodes    int
	SkippedSockets int
}

// Skipped reports entries the clone could not reproduce.
func (stats *CloneTreeStats) Skipped() int {
	return stats.SkippedSockets
}

// CloneTree copies a directory tree to a destination that must not yet exist.
// Files are cloned copy-on-write when the filesystem allows it; directory
// structure, permissions, ownership (for root), symlinks, hard links,
// extended attributes, and device nodes (for root, inside the tree) are
// reproduced. Unix sockets are skipped — they are runtime endpoints, not
// state — and counted, never silently dropped.
func CloneTree(source, destination string) (CloneTreeStats, error) {
	stats := CloneTreeStats{}
	if _, err := os.Lstat(destination); err == nil {
		return stats, fmt.Errorf("clone destination %s already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return stats, err
	}
	root, err := os.Lstat(source)
	if err != nil {
		return stats, err
	}
	if err := os.Mkdir(destination, root.Mode().Perm()); err != nil {
		return stats, err
	}
	if err := copyAttrs(source, destination, root); err != nil {
		return stats, err
	}
	stats.Directories++
	// hardlinkTargets remembers the first path created for each inode so the
	// second and later links of a hard-linked group are re-linked, not copied.
	hardlinkTargets := make(map[devInode]string)
	err = cloneWalk(source, destination, &stats, hardlinkTargets)
	if err != nil {
		return stats, err
	}
	return stats, nil
}

type devInode struct {
	device uint64
	inode  uint64
}

func cloneWalk(source, target string, stats *CloneTreeStats, hardlinks map[devInode]string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		targetPath := filepath.Join(target, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			if err := os.Mkdir(targetPath, mode.Perm()); err != nil {
				return err
			}
			if err := copyAttrs(sourcePath, targetPath, info); err != nil {
				return err
			}
			stats.Directories++
			if err := cloneWalk(sourcePath, targetPath, stats, hardlinks); err != nil {
				return err
			}
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(sourcePath)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, targetPath); err != nil {
				return err
			}
			stats.Symlinks++
		case mode.IsRegular():
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				key := devInode{device: stat.Dev, inode: stat.Ino}
				if first, exists := hardlinks[key]; exists {
					if err := os.Link(first, targetPath); err != nil {
						return err
					}
					stats.HardLinks++
					continue
				}
				hardlinks[key] = targetPath
			}
			cloned, err := CloneFile(sourcePath, targetPath)
			if err != nil {
				return err
			}
			if cloned {
				stats.ClonedFiles++
			}
			stats.Files++
			if err := copyAttrs(sourcePath, targetPath, info); err != nil {
				return err
			}
		default:
			if err := cloneSpecial(sourcePath, targetPath, info, stats); err != nil {
				return err
			}
		}
	}
	return nil
}

// cloneSpecial reproduces non-file, non-directory entries: device nodes and
// FIFOs via mknod (root only — a non-root clone cannot create them, and
// failing loudly beats shipping a tree whose devices silently vanished), and
// Unix sockets, which cannot exist without a live peer and are skipped.
func cloneSpecial(sourcePath, targetPath string, info os.FileInfo, stats *CloneTreeStats) error {
	mode := info.Mode()
	if mode&os.ModeSocket != 0 {
		stats.SkippedSockets++
		return nil
	}
	if mode&(os.ModeDevice|os.ModeNamedPipe) == 0 {
		return fmt.Errorf("%s has unsupported file type %s", sourcePath, mode.Type())
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("cloning %s requires root (device nodes cannot be created unprivileged)", sourcePath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s has no device information", sourcePath)
	}
	permissions := uint32(mode.Perm())
	switch {
	case mode&os.ModeNamedPipe != 0:
		permissions |= syscall.S_IFIFO
	case mode&os.ModeCharDevice != 0:
		permissions |= syscall.S_IFCHR
	default:
		permissions |= syscall.S_IFBLK
	}
	if err := syscall.Mknod(targetPath, permissions, int(stat.Rdev)); err != nil {
		return fmt.Errorf("recreate device node %s: %w", sourcePath, err)
	}
	stats.DeviceNodes++
	return copyAttrs(sourcePath, targetPath, info)
}

// copyAttrs reproduces ownership and extended attributes. Ownership is only
// changed when running as root; a non-root clone keeps the caller's ownership,
// which is the strongest an unprivileged process can do.
func copyAttrs(sourcePath, targetPath string, info os.FileInfo) error {
	if os.Geteuid() == 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			if err := os.Lchown(targetPath, int(stat.Uid), int(stat.Gid)); err != nil {
				return err
			}
		}
	}
	// A chmod after chown because chown can clear set-id bits.
	if info.Mode()&os.ModeSymlink == 0 {
		if err := os.Chmod(targetPath, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return copyXattrs(sourcePath, targetPath)
}
