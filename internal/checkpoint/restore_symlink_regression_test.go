package checkpoint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"shift.dev/shift/internal/model"
)

// Regression cover for the restore-target symlink escape (ship finding 05
// blocker B2): a target like <allowed>/lnk/newdir with lnk -> /etc used to
// pass a lexical denylist check and land a root-extracted tree under /etc.
// The admission path must resolve without following a single symlink and
// refuse the planted-parent case.
func TestRestoreTargetRefusesPlantedParentSymlink(t *testing.T) {
	allowed := t.TempDir()
	workload := model.WorkloadSpec{UID: os.Geteuid(), GID: os.Getegid(), Command: []string{"/bin/true"}}

	// Positive control: a fresh target under the allowed prefix, reached
	// through real directories, is admitted.
	real := filepath.Join(allowed, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := RestorePolicy{AllowedRootPrefixes: []string{allowed}, AllowSystemTreeTargets: []string{allowed}}
	target := filepath.Join(real, "newdir")
	if err := policy.CheckRestoreTarget(target, workload, ""); err != nil {
		t.Fatalf("fresh target under the allowed prefix must be admitted: %v", err)
	}

	// The bypass: an intermediate component of the target is a symlink out
	// of the allowed tree. Lexically <allowed>/lnk/ssl/newdir; actually
	// /etc/ssl/newdir.
	plant := filepath.Join(allowed, "lnk")
	if err := os.Symlink("/etc", plant); err != nil {
		t.Skipf("cannot plant the escape symlink: %v", err)
	}
	escape := filepath.Join(plant, "ssl", "newdir")
	err := policy.CheckRestoreTarget(escape, workload, "")
	if err == nil {
		t.Fatal("BYPASS REGRESSED: a target escaping through a planted parent symlink is admitted")
	}
	if !errors.Is(err, ErrRestoreTargetDenied) {
		t.Fatalf("planted-parent escape error = %v, want ErrRestoreTargetDenied", err)
	}
}

// The same symlink-free resolution must guard brand-new fork/clone roots:
// a new root materializing through a planted parent symlink is refused.
func TestNewRootRefusesPlantedParentSymlink(t *testing.T) {
	allowed := t.TempDir()
	workload := model.WorkloadSpec{UID: os.Geteuid(), GID: os.Getegid(), Command: []string{"/bin/true"}}

	source := filepath.Join(allowed, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	plant := filepath.Join(allowed, "lnk")
	if err := os.Symlink("/etc", plant); err != nil {
		t.Skipf("cannot plant the escape symlink: %v", err)
	}
	policy := RestorePolicy{AllowSystemTreeTargets: []string{allowed}}
	escape := filepath.Join(plant, "share", "newroot")
	err := policy.CheckNewRoot(escape, source, workload)
	if err == nil {
		t.Fatal("BYPASS REGRESSED: a new root materializing through a planted parent symlink is admitted")
	}
	if !errors.Is(err, ErrRestoreTargetDenied) {
		t.Fatalf("planted-parent escape error = %v, want ErrRestoreTargetDenied", err)
	}
}
