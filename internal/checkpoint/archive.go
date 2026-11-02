package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/filesystem"
	"shift.dev/shift/internal/model"
)

// tarCommand is the one tar binary every capture and extraction runs: pinned
// at startup from an absolute path (never a PATH lookup at call time), so a
// caller that influences the agent's environment cannot redirect archiving
// through a planted binary.
var tarCommand = resolveTar()

func resolveTar() string {
	if info, err := os.Stat("/usr/bin/tar"); err == nil && info.Mode().IsRegular() {
		return "/usr/bin/tar"
	}
	if resolved, err := exec.LookPath("tar"); err == nil {
		return resolved
	}
	return "/usr/bin/tar"
}

// tarEnvironment is the minimal environment the tar processes run with: a
// pinned system PATH and nothing inherited from the agent's callers.
func tarEnvironment() []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
}

// captureDirectory archives one or more entries of a directory tree into the
// chunk store. Bases name archive members relative to root; a CRIU image chain
// is captured as several sibling bases because its image sets reference each
// other through parent symlinks and must never be flattened into one
// directory. Exclusions are matched against base-relative member paths.
func captureDirectory(ctx context.Context, store *chunkstore.Store, workloadID, assetName, root string, bases, exclusions []string, restoreOrder int) (chunkstore.PutResult, error) {
	if len(bases) == 0 {
		return chunkstore.PutResult{}, errors.New("asset " + assetName + " has no archive bases")
	}
	args := []string{
		"--create", "--format=pax", "--numeric-owner", "--acls", "--xattrs", "--xattrs-include=*", "--sparse",
		"--one-file-system", "--directory", root,
	}
	for _, base := range bases {
		for _, exclusion := range exclusions {
			args = append(args, "--exclude", filepath.ToSlash(filepath.Join(base, exclusion)))
		}
	}
	args = append(args, "--")
	args = append(args, bases...)
	// A capture that spans a mountpoint boundary is silently incomplete:
	// tar's --one-file-system archives a crossed mountpoint as an empty
	// directory and drops everything mounted there, exit 0, no warning. The
	// manifest would still claim an exact capture, and the restore would
	// "succeed" over a filesystem missing every mounted volume's contents —
	// so the boundary is refused up front, loudly, before any bytes move.
	if err := checkCaptureMountpoints(root, bases, exclusions); err != nil {
		return chunkstore.PutResult{}, err
	}
	command := exec.CommandContext(ctx, tarCommand, args...)
	command.Env = tarEnvironment()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return chunkstore.PutResult{}, err
	}
	var stderr bytes.Buffer
	command.Stderr = &limitedBuffer{buffer: &stderr, remaining: 1 << 20}
	if err := command.Start(); err != nil {
		return chunkstore.PutResult{}, err
	}
	result, storeErr := store.PutAsset(ctx, workloadID, assetName, "application/vnd.shift.filesystem-tar", stdout, true, restoreOrder)
	if storeErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if storeErr != nil {
		return chunkstore.PutResult{}, storeErr
	}
	if waitErr != nil {
		return chunkstore.PutResult{}, fmt.Errorf("archive %s: %w: %s", assetName, waitErr, strings.TrimSpace(stderr.String()))
	}
	// Even a clean exit must not hide dropped content: some tars warn about
	// skipped subtrees instead of failing, and a capture that quietly left
	// data out is exactly the corruption this pipeline must never accept.
	if captureDroppedContent(stderr.String()) {
		return chunkstore.PutResult{}, fmt.Errorf("archive %s: tar reported dropped content and the capture would be incomplete: %s", assetName, strings.TrimSpace(stderr.String()))
	}
	return result, nil
}

func extractDirectory(ctx context.Context, store *chunkstore.Store, workloadID string, asset model.AssetManifest, destination string, policy RestorePolicy) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return err
	}
	// Pin the staging directory's identity before a byte lands in it: the
	// extract and the sanitizing walk below run against a path, and a
	// swapped-in directory between them would launder root-owned archive
	// content through whatever replaced ours.
	before, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	beforeStat, _ := before.Sys().(*syscall.Stat_t)
	command := exec.CommandContext(ctx, tarCommand,
		"--extract", "--numeric-owner", "--acls", "--xattrs", "--xattrs-include=*", "--sparse",
		"--same-owner", "--same-permissions", "--delay-directory-restore",
		"--no-overwrite-dir", "--directory", destination)
	command.Env = tarEnvironment()
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	command.Stderr = &limitedBuffer{buffer: &stderr, remaining: 1 << 20}
	if err := command.Start(); err != nil {
		return err
	}
	restoreErr := store.RestoreAsset(ctx, workloadID, asset, stdin)
	closeErr := stdin.Close()
	if restoreErr != nil || closeErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if restoreErr != nil {
		return restoreErr
	}
	if closeErr != nil {
		return closeErr
	}
	if waitErr != nil {
		return fmt.Errorf("extract %s: %w: %s", asset.Name, waitErr, strings.TrimSpace(stderr.String()))
	}
	// The archive is fully extracted; before anything in it is trusted,
	// prove the tree is still the one we created and harden it in place:
	// setuid/setgid stripped (unless the policy keeps them), device nodes
	// refused (unless the policy keeps them), and every symlink proven
	// contained. This runs before extractDirectory returns, so no caller can
	// observe — and no crash can leave standing — an unsanitized tree.
	after, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	afterStat, _ := after.Sys().(*syscall.Stat_t)
	if beforeStat != nil && afterStat != nil && (beforeStat.Dev != afterStat.Dev || beforeStat.Ino != afterStat.Ino) {
		return fmt.Errorf("extraction destination %s was replaced during extraction", destination)
	}
	return sanitizeExtractedTree(destination, policy)
}

// directoryBytes sums the regular files under a directory, skipping CRIU's
// "work" scratch directory (it holds the pass log, not image data). For a
// pre-dump pass this is the dirty-memory delta that pass carried — the
// signal the iterative pre-copy loop converges on.
func directoryBytes(root string) int64 {
	total := int64(0)
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "work" && path == filepath.Join(root, "work") {
				return filepath.SkipDir
			}
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// materializeRoot moves a staged, extracted workload root into its final
// place. When staging and the target sit on different filesystems a rename
// cannot cross the boundary, so the tree is cloned instead — copy-on-write
// where the filesystem supports it, a plain copy otherwise — and the staged
// copy is removed. A cross-device restore or fork is a real operator need;
// failing on EXDEV would force operators to co-locate state with every
// workload root.
//
// The switch is symlink- and TOCTOU-safe: the target's parent is resolved
// without following any symlink, pinned open, identity-checked immediately
// before the move, and the rename runs through the pinned handle — so a
// parent swapped for a symlink in the extraction window can neither redirect
// the tree nor pass unnoticed.
func materializeRoot(staged, target string) error {
	dir, err := pinTargetDir(target)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return materializeRootAt(dir, staged)
}

// materializeRootAt performs the switch through an already-pinned target
// directory. When the staging tree sits inside the pinned parent (the
// restore and fork layouts), the whole switch is one renameat across that
// single verified handle; a staging tree elsewhere gets its own symlink-free
// pin on the source side and still lands through the pinned target handle.
func materializeRootAt(dir *targetDir, staged string) error {
	stagedParent := filepath.Dir(staged)
	stagedName := filepath.Base(staged)
	sourceFD, sourceStat, err := openDirNoSymlinks(stagedParent)
	if err != nil {
		return fmt.Errorf("open staging directory: %w", err)
	}
	defer func() { _ = syscall.Close(sourceFD) }()
	var targetStat syscall.Stat_t
	if err := syscall.Fstat(dir.fd, &targetStat); err != nil {
		return err
	}
	var renameErr error
	if sourceStat.Dev == targetStat.Dev && sourceStat.Ino == targetStat.Ino {
		relative := stagedName
		if rel, relErr := filepath.Rel(dir.path, filepath.Clean(staged)); relErr == nil {
			relative = rel
		}
		renameErr = dir.renameWithin(relative, dir.name)
	} else {
		renameErr = dir.renameInto(sourceFD, stagedName)
	}
	if renameErr == nil {
		return nil
	}
	if !errors.Is(renameErr, syscall.EXDEV) {
		return renameErr
	}
	// sourceFD keeps the staging directory reachable and identity-pinned for
	// the copy; the destination is addressed through the pinned target
	// handle, never through the mutable path.
	if _, err := filesystem.CloneTree(staged, dir.targetViaFD()); err != nil {
		// A partial clone must not survive: the caller's rollback assumes
		// the target either fully appeared or never did.
		_ = os.RemoveAll(dir.targetViaFD())
		return err
	}
	return os.RemoveAll(staged)
}

// cloneTreeInto materializes a fresh target root by copy (clone sets never
// rename the shared staging — every member copies from it), through the same
// pinned, symlink-free target handling as materializeRoot.
func cloneTreeInto(staged, target string) (filesystem.CloneTreeStats, error) {
	dir, err := pinTargetDir(target)
	if err != nil {
		return filesystem.CloneTreeStats{}, err
	}
	defer func() { _ = dir.Close() }()
	if err := dir.verify(); err != nil {
		return filesystem.CloneTreeStats{}, err
	}
	stats, err := filesystem.CloneTree(staged, dir.targetViaFD())
	if err != nil {
		_ = os.RemoveAll(dir.targetViaFD())
		return filesystem.CloneTreeStats{}, err
	}
	return stats, nil
}

type limitedBuffer struct {
	buffer    *bytes.Buffer
	remaining int
}

func (w *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if w.remaining <= 0 {
		return original, nil
	}
	if len(value) > w.remaining {
		value = value[:w.remaining]
	}
	_, _ = w.buffer.Write(value)
	w.remaining -= len(value)
	return original, nil
}

// mountinfoPath is the mount table consulted at capture time. A variable so
// regression tests can exercise the parser against synthetic tables.
var mountinfoPath = "/proc/self/mountinfo"

// unescapeMountInfoPath decodes the octal escapes /proc/self/mountinfo uses
// inside mount-point paths: space, tab, newline, and backslash become \040,
// \011, \012, and \134. Decoding only some of them (e.g. just \040) silently
// mismatches any root whose path carries the others.
func unescapeMountInfoPath(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+3 < len(value) {
			if decoded, err := strconv.ParseUint(value[index+1:index+4], 8, 8); err == nil {
				builder.WriteByte(byte(decoded))
				index += 3
				continue
			}
		}
		builder.WriteByte(value[index])
	}
	return builder.String()
}

// checkCaptureMountpoints refuses a capture whose tree spans a mountpoint
// boundary. tar's --one-file-system archives a crossed mountpoint as an empty
// directory and drops everything mounted there — exit 0, often no warning —
// so without this check a workload root with a bind-mounted data directory is
// silently captured incomplete while the manifest claims an exact capture.
func checkCaptureMountpoints(root string, bases, exclusions []string) error {
	content, err := os.ReadFile(mountinfoPath)
	if err != nil {
		return fmt.Errorf("read mount table %s: %w", mountinfoPath, err)
	}
	return checkCaptureMountpointsTable(string(content), root, bases, exclusions)
}

func checkCaptureMountpointsTable(table, root string, bases, exclusions []string) error {
	for _, line := range strings.Split(table, "\n") {
		if line == "" {
			continue
		}
		left := strings.SplitN(line, " - ", 2)[0]
		fields := strings.Fields(left)
		if len(fields) < 5 {
			continue
		}
		mountpoint := filepath.Clean(unescapeMountInfoPath(fields[4]))
		for _, base := range bases {
			basePath := filepath.Clean(filepath.Join(root, base))
			if mountpoint == basePath || !pathContains(basePath, mountpoint) {
				continue
			}
			if excludedFromCapture(basePath, mountpoint, exclusions) {
				continue
			}
			return fmt.Errorf("workload tree %s contains the mounted volume %s: --one-file-system would silently capture it as an empty directory; unmount it, exclude that subtree explicitly in the workload paths, or capture with a snapshot adapter", basePath, mountpoint)
		}
	}
	return nil
}

// excludedFromCapture reports whether a mountpoint falls under a subtree the
// capture already excludes — where dropping its content is the caller's
// stated intent, not a silent loss.
func excludedFromCapture(basePath, mountpoint string, exclusions []string) bool {
	relative, err := filepath.Rel(basePath, mountpoint)
	if err != nil {
		return false
	}
	relative = filepath.ToSlash(relative)
	for _, exclusion := range exclusions {
		exclusion = filepath.ToSlash(filepath.Clean(exclusion))
		if exclusion == "." || exclusion == "" {
			continue
		}
		if relative == exclusion || strings.HasPrefix(relative, exclusion+"/") {
			return true
		}
		if matched, err := filepath.Match(exclusion, relative); err == nil && matched {
			return true
		}
	}
	return false
}

// captureDroppedContent reports whether tar's own output says it left content
// out of the archive. Different builds word the skip differently; both known
// forms ("not dumped", "different filesystem") mean the capture is incomplete.
func captureDroppedContent(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "not dumped") || strings.Contains(lowered, "different filesystem")
}

func validateExtractedRoot(staging, expectedBase string) (string, error) {
	entries, err := os.ReadDir(staging)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || entries[0].Name() != expectedBase {
		return "", errors.New("filesystem archive does not contain the expected single workload root")
	}
	root := filepath.Join(staging, expectedBase)
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("restored workload root is not a directory")
	}
	return root, nil
}

// sanitizeExtractedTree hardens a freshly extracted checkpoint tree. The
// archive is untrusted input produced by the source machine: root-level
// extraction reproduces whatever it carries, so anything that turns a workload
// directory into local privilege escalation is neutralized here — setuid and
// setgid bits are stripped unless the destination policy explicitly keeps
// them, device nodes, FIFOs, and sockets are refused unless explicitly
// allowed, and privilege-bearing extended attributes (file capabilities and
// every other security.* / trusted.* label) are stripped unless the policy
// explicitly keeps file capabilities.
//
// Symlinks are resolved in two phases, once the whole tree exists: an
// absolute target is an escape outright (restored workloads are not chrooted,
// so /etc/shadow means the host's), and every relative chain — a -> b,
// b -> /etc included — must resolve to a path inside the extraction root,
// lexically and after real resolution through the tree's own links.
func sanitizeExtractedTree(root string, policy RestorePolicy) error {
	links := make([][2]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if err := SanitizeExtractedEntry(path, mode, policy); err != nil {
			return err
		}
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			links = append(links, [2]string{path, target})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, link := range links {
		if err := checkExtractedSymlink(root, link[0], link[1]); err != nil {
			return err
		}
	}
	return nil
}

// checkExtractedSymlink proves one extracted symlink cannot reach outside the
// extraction root. Absolute targets are escapes outright. Relative targets
// must stay inside the root lexically, and their full chain — resolved
// through the tree's own links once every entry exists — must land inside the
// root as well, so a contained hop cannot fan out through a second link to
// the host filesystem.
func checkExtractedSymlink(root, path, target string) error {
	if filepath.IsAbs(target) {
		return fmt.Errorf("checkpoint archive symlink %s has the absolute target %s, which escapes the extraction root", path, target)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
	if !pathContains(root, resolved) {
		return fmt.Errorf("checkpoint archive symlink %s escapes the extraction root", path)
	}
	// Follow the chain the tree itself defines: every hop must be relative
	// and contained, and a hop that does not resolve (a dangling link) is
	// acceptable only while it stays inside the root.
	current, currentTarget := path, target
	for hop := 0; hop < 40; hop++ {
		if filepath.IsAbs(currentTarget) {
			return fmt.Errorf("checkpoint archive symlink %s chains through %s to the absolute target %s, which escapes the extraction root", path, current, currentTarget)
		}
		next := filepath.Clean(filepath.Join(filepath.Dir(current), currentTarget))
		if !pathContains(root, next) {
			return fmt.Errorf("checkpoint archive symlink %s chains through %s and escapes the extraction root", path, current)
		}
		info, err := os.Lstat(next)
		if err != nil {
			break
		}
		if info.Mode()&os.ModeSymlink == 0 {
			break
		}
		currentTarget, err = os.Readlink(next)
		if err != nil {
			return err
		}
		current = next
	}
	// Final resolution assert on the real filesystem: whatever the kernel
	// would make of this link must not leave the extraction root.
	if final, err := filepath.EvalSymlinks(path); err == nil {
		if !pathContains(root, final) {
			return fmt.Errorf("checkpoint archive symlink %s resolves to %s, which escapes the extraction root", path, final)
		}
	}
	return nil
}
