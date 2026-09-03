package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/filesystem"
	"shift.dev/shift/internal/model"
)

func captureDirectory(ctx context.Context, store *chunkstore.Store, workloadID, assetName, path string, exclusions []string, restoreOrder int) (chunkstore.PutResult, error) {
	parent := filepath.Dir(path)
	base := filepath.Base(path)
	args := []string{
		"--create", "--format=pax", "--numeric-owner", "--acls", "--xattrs", "--xattrs-include=*", "--sparse",
		"--one-file-system", "--directory", parent,
	}
	for _, exclusion := range exclusions {
		args = append(args, "--exclude", filepath.ToSlash(filepath.Join(base, exclusion)))
	}
	args = append(args, "--", base)
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
		return chunkstore.PutResult{}, fmt.Errorf("archive %s: %w: %s", path, waitErr, strings.TrimSpace(stderr.String()))
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

// mergeDirectory copies files that are absent from destination. It is used to
// make a CRIU final dump self-contained after a pre-dump: files emitted by the
// final dump win, while unchanged image files are inherited from the pre-dump.
func mergeDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		// CRIU image files are written once and never modified, so cloning
		// them copy-on-write where the filesystem allows is safe and turns
		// the pre-dump merge into a metadata operation on btrfs and xfs.
		if _, err := filesystem.CloneFile(path, target); err != nil {
			return err
		}
		return nil
	})
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

func drainAndClose(reader io.ReadCloser) {
	_, _ = io.Copy(io.Discard, reader)
	_ = reader.Close()
}
