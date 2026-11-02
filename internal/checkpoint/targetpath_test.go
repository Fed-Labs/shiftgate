package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTargetVerifyNoticesSameInodeParentSwap covers the detection gap the
// pinned-handle design promised to close: a parent renamed away and replaced
// by a symlink — even one pointing back at the same directory inode — changes
// what the path names, and verify() must refuse it as an identity change.
// The operations themselves run through the pinned handle and could not be
// redirected by the swap; this is the promised notice, not a new boundary.
func TestTargetVerifyNoticesSameInodeParentSwap(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(work, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(work, "root")
	dir, err := pinTargetDir(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.verify(); err != nil {
		t.Fatalf("verify must pass while the path is intact: %v", err)
	}
	if err := os.Rename(work, filepath.Join(base, "work.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "work.real"), work); err != nil {
		t.Fatal(err)
	}
	err = dir.verify()
	if err == nil {
		t.Fatal("BYPASS REGRESSED: verify followed a symlink swap onto the same directory inode")
	}
	if !errors.Is(err, ErrRestoreTargetDenied) || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("the refusal must be the promised identity-change denial, got: %v", err)
	}
	if err := dir.renameWithin("root", "moved"); err == nil {
		t.Fatal("a rename must not run through a swapped path")
	}
	if _, statErr := os.Stat(filepath.Join(base, "work.real", "root")); statErr != nil {
		t.Fatalf("nothing may be renamed through the swapped path: %v", statErr)
	}
}

// TestTargetVerifyPassesOnIntactPath is the positive control: a healthy pinned
// parent verifies and the switch runs.
func TestTargetVerifyPassesOnIntactPath(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(filepath.Join(work, "root"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := pinTargetDir(filepath.Join(work, "root"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.renameWithin("root", "moved"); err != nil {
		t.Fatalf("the switch must run through the pinned handle: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(work, "moved")); statErr != nil {
		t.Fatalf("the tree did not land in the pinned directory: %v", statErr)
	}
}
