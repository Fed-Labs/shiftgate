package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/filesystem"
	"shift.dev/shift/internal/model"
)

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
	command := exec.CommandContext(ctx, "tar", args...)
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
	return result, nil
}

func extractDirectory(ctx context.Context, store *chunkstore.Store, workloadID string, asset model.AssetManifest, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "tar",
		"--extract", "--numeric-owner", "--acls", "--xattrs", "--xattrs-include=*", "--sparse",
		"--same-owner", "--same-permissions", "--delay-directory-restore", "--directory", destination)
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
	return nil
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
func materializeRoot(staged, target string) error {
	renameErr := os.Rename(staged, target)
	if renameErr == nil {
		return nil
	}
	if !errors.Is(renameErr, syscall.EXDEV) {
		return renameErr
	}
	if _, err := filesystem.CloneTree(staged, target); err != nil {
		// A partial clone must not survive: the caller's rollback assumes
		// the target either fully appeared or never did.
		_ = os.RemoveAll(target)
		return err
	}
	return os.RemoveAll(staged)
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
