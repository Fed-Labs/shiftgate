package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// This file is the destination-side filesystem safety core: every path a
// checkpoint may replace is resolved without following a single symlink, the
// parent directory is pinned by an O_PATH handle, and the switch itself runs
// as a renameat through that handle. Lexical checks alone are not a boundary:
// a planted intermediate symlink (target <allowed>/lnk/x with lnk -> /etc)
// or a parent directory swapped for a symlink between admission and the
// switch would otherwise redirect a root-owned tree into any system
// directory. openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) does the
// resolution in one kernel-checked step where the kernel supports it; the
// component-by-component O_NOFOLLOW walk is the fallback. Both paths end in
// the same realpath containment assert.

// targetDir is a restore/fork/clone target's parent directory, pinned by an
// open handle no symlink can redirect. All mutations of the target — the
// backup rename, the switch, the rollback restores — happen as renameat
// through this handle after a dev/ino re-check of the path, so a parent
// swapped mid-operation can neither redirect the tree nor go unnoticed.
type targetDir struct {
	fd   int    // O_PATH|O_NOFOLLOW handle on the verified parent directory
	path string // cleaned absolute parent path
	name string // the target's final path component
	dev  uint64
	ino  uint64
}

// pinTargetDir resolves targetRoot's parent directory without following any
// symlink and pins it open. The parent must already exist and must be a real
// directory reachable with every component a real directory — what may be
// targeted at all (system trees, ownership, operator registration) is policy
// admission's job (RestorePolicy), not this handle's. The target's final
// component may be absent, must not be a symlink, and — when present — must
// be a real directory.
func pinTargetDir(targetRoot string) (*targetDir, error) {
	target := filepath.Clean(targetRoot)
	if target == "" || target == string(filepath.Separator) || !filepath.IsAbs(target) {
		return nil, fmt.Errorf("%w: %q is not a usable absolute workload root", ErrRestoreTargetDenied, targetRoot)
	}
	parent := filepath.Dir(target)
	name := filepath.Base(target)
	fd, stat, err := openDirNoSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: %s must be reachable as a real directory without symlinks: %v", ErrRestoreTargetDenied, parent, err)
	}
	dir := &targetDir{fd: fd, path: parent, name: name, dev: uint64(stat.Dev), ino: stat.Ino}
	ok := false
	defer func() {
		if !ok {
			_ = dir.Close()
		}
	}()
	// Final realpath containment assert (belt and braces on top of the
	// kernel-checked walk): if anything still resolves the parent through a
	// link, the resolved path must equal the lexical one.
	resolved, evalErr := filepath.EvalSymlinks(parent)
	if evalErr != nil {
		return nil, fmt.Errorf("%w: cannot resolve %s: %v", ErrRestoreTargetDenied, parent, evalErr)
	}
	if resolved != parent {
		return nil, fmt.Errorf("%w: %s resolves to %s through a symlink", ErrRestoreTargetDenied, parent, resolved)
	}
	ok = true
	return dir, nil
}

// verify re-checks, immediately before a mutation, that the pinned handle is
// still the directory the pinned path names. Both sides are compared as
// dev/ino identities without following any symlink: the handle must still
// open the directory it was pinned to, and the path must still name that same
// directory as a real directory — a parent renamed away and replaced by a
// symlink is refused even when the symlink points back at the same directory
// inode, because what the path names changed even if the handle could not be
// redirected.
func (d *targetDir) verify() error {
	var handle syscall.Stat_t
	if err := syscall.Fstat(d.fd, &handle); err != nil {
		return fmt.Errorf("%w: target parent %s is no longer reachable: %v", ErrRestoreTargetDenied, d.path, err)
	}
	if uint64(handle.Dev) != d.dev || handle.Ino != d.ino {
		return fmt.Errorf("%w: %s was replaced during the operation (identity changed)", ErrRestoreTargetDenied, d.path)
	}
	info, err := os.Lstat(d.path)
	if err != nil {
		return fmt.Errorf("%w: target parent %s is no longer reachable: %v", ErrRestoreTargetDenied, d.path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s was replaced during the operation (identity changed)", ErrRestoreTargetDenied, d.path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot determine identity of %s", ErrRestoreTargetDenied, d.path)
	}
	if uint64(stat.Dev) != d.dev || stat.Ino != d.ino {
		return fmt.Errorf("%w: %s was replaced during the operation (identity changed)", ErrRestoreTargetDenied, d.path)
	}
	return nil
}

// lstatTarget stats the target's final component without following it. It
// reports the raw stat (zeroed when the component is absent).
func (d *targetDir) lstatTarget() (syscall.Stat_t, bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(d.fd, d.name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, syscall.ENOENT) {
		return syscall.Stat_t{}, false, nil
	}
	if err != nil {
		return syscall.Stat_t{}, false, err
	}
	return syscall.Stat_t{Dev: uint64(stat.Dev), Ino: stat.Ino, Uid: stat.Uid, Gid: stat.Gid, Mode: stat.Mode}, true, nil
}

// renameWithin renames oldName to newName inside the pinned directory. The
// rename runs through the pinned handle, so even a path swap between verify
// and the syscall can only land the tree in the already-verified directory —
// never in wherever the swapped path points.
func (d *targetDir) renameWithin(oldName, newName string) error {
	if err := d.verify(); err != nil {
		return err
	}
	if err := unix.Renameat(d.fd, oldName, d.fd, newName); err != nil {
		return err
	}
	return nil
}

// renameInto moves sourceName from a separately pinned source directory to
// the target's final name inside the pinned target directory.
func (d *targetDir) renameInto(sourceFD int, sourceName string) error {
	if err := d.verify(); err != nil {
		return err
	}
	return unix.Renameat(sourceFD, sourceName, d.fd, d.name)
}

// targetViaFD names the target through the pinned handle for operations that
// need a path (clone-tree copies): /proc resolves the handle to the verified
// directory whatever the mounted path now says.
func (d *targetDir) targetViaFD() string {
	return filepath.Join("/proc/self/fd", fmt.Sprint(d.fd), d.name)
}

func (d *targetDir) Close() error {
	if d.fd < 0 {
		return nil
	}
	err := syscall.Close(d.fd)
	d.fd = -1
	return err
}

// openDirNoSymlinks opens an existing directory without following any symlink
// anywhere along the path. Where the kernel provides openat2, one call with
// RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS does the whole
// resolution atomically; otherwise the path is walked component by component
// with O_NOFOLLOW and every hop is a separate kernel-checked open. Either way
// the caller receives an O_PATH handle plus the directory's identity.
func openDirNoSymlinks(path string) (int, syscall.Stat_t, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return 0, syscall.Stat_t{}, fmt.Errorf("%q is not absolute", path)
	}
	relative := strings.TrimPrefix(clean, string(filepath.Separator))
	if fd, stat, err := openDirOpenat2(relative); err == nil {
		return fd, stat, nil
	} else if !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EINVAL) {
		return 0, syscall.Stat_t{}, err
	}
	return openDirWalk(relative)
}

// openDirOpenat2 resolves the path relative to a pinned "/" with openat2 in
// its strictest mode. ENOSYS (kernel without openat2) is passed through so the
// caller falls back to the walk; any resolution refusal is a real refusal.
func openDirOpenat2(relative string) (int, syscall.Stat_t, error) {
	rootFD, err := syscall.Open("/", int(unix.O_PATH)|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, syscall.Stat_t{}, err
	}
	defer func() { _ = syscall.Close(rootFD) }()
	fd, err := unix.Openat2(rootFD, relative, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return 0, syscall.Stat_t{}, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = syscall.Close(fd)
		return 0, syscall.Stat_t{}, err
	}
	return fd, stat, nil
}

// openDirWalk is the openat2-less fallback: every component is opened with
// O_NOFOLLOW under its parent's handle, so a symlink anywhere in the chain is
// a hard refusal rather than a redirection.
func openDirWalk(relative string) (int, syscall.Stat_t, error) {
	fd, err := syscall.Open("/", int(unix.O_PATH)|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, syscall.Stat_t{}, err
	}
	if relative == "" {
		var stat syscall.Stat_t
		if err := syscall.Fstat(fd, &stat); err != nil {
			_ = syscall.Close(fd)
			return 0, syscall.Stat_t{}, err
		}
		return fd, stat, nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		next, openErr := syscall.Openat(fd, component, int(unix.O_PATH)|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, syscall.ELOOP) || errors.Is(openErr, syscall.ENOTDIR) {
				return 0, syscall.Stat_t{}, fmt.Errorf("%s is a symlink or not a directory: %w", component, syscall.ELOOP)
			}
			return 0, syscall.Stat_t{}, openErr
		}
		fd = next
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		_ = syscall.Close(fd)
		return 0, syscall.Stat_t{}, err
	}
	return fd, stat, nil
}

// targetStats resolves a target's parent symlink-free and reports the
// parent's identity plus the target's final component as it exists right now
// (absent, a real directory, or anything else). Policy admission runs on
// exactly these facts — never on a path a symlink could have redirected.
type targetStats struct {
	path         string
	parentStat   syscall.Stat_t
	targetExists bool
	targetStat   syscall.Stat_t
}

// inspectTarget resolves targetRoot the same way the switch will: no
// symlinks anywhere, the parent pinned, the final component read with
// AT_SYMLINK_NOFOLLOW. The returned handle is the caller's to close.
func inspectTarget(targetRoot string) (*targetDir, *targetStats, error) {
	dir, err := pinTargetDir(targetRoot)
	if err != nil {
		return nil, nil, err
	}
	stats := &targetStats{path: filepath.Clean(targetRoot)}
	var parentStat syscall.Stat_t
	if err := syscall.Fstat(dir.fd, &parentStat); err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	stats.parentStat = parentStat
	targetStat, exists, err := dir.lstatTarget()
	if err != nil {
		_ = dir.Close()
		return nil, nil, err
	}
	stats.targetExists = exists
	stats.targetStat = targetStat
	return dir, stats, nil
}
