package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression suite for the restore-target admission bypasses the ship
// verifier proved against the old lexical checks: an intermediate symlink
// redirecting the target into a system tree, and a parent directory swapped
// for a symlink in the extraction window. Both were admitted or executed
// before; every assertion here fails against that logic.

// TestPolicyBypassRestoreTargetSymlinkedParentEscapeRefused reproduces the verifier's
// bypass (a): target <allowed>/lnk/ssl/newdir with lnk -> /etc. The lexical
// check saw a target under the operator allowlist and checkTargetOwnership's
// Lstat followed the link into root-owned /etc/ssl — admission passed and a
// fresh root landed under /etc. The symlink-free resolution must refuse.
func TestPolicyBypassRestoreTargetSymlinkedParentEscapeRefused(t *testing.T) {
	base := t.TempDir()
	policy := RestorePolicy{AllowedRootPrefixes: []string{base}, AllowSystemTreeTargets: []string{os.TempDir()}}
	workload := testWorkload(os.Geteuid())

	lnk := filepath.Join(base, "lnk")
	if err := os.Symlink("/etc", lnk); err != nil {
		t.Fatal(err)
	}
	// The exact shape the verifier used: a target that lexically sits under
	// the allowed prefix but whose parent chain resolves into /etc.
	target := filepath.Join(base, "lnk", "ssl", "newdir")
	err := policy.CheckRestoreTarget(target, workload, "")
	if err == nil {
		t.Fatal("BYPASS REGRESSED: a target resolving through a symlink into /etc was admitted")
	}
	if !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "without symlinks") {
		t.Fatalf("the refusal must name the symlink, got %v", err)
	}

	// The same chain through a relative link must fail the same way.
	rel := filepath.Join(base, "rellink")
	if err := os.Symlink("../../etc", rel); err != nil {
		t.Fatal(err)
	}
	if err := policy.CheckRestoreTarget(filepath.Join(base, "rellink", "newdir"), workload, ""); err == nil {
		t.Fatal("BYPASS REGRESSED: a relative parent symlink into /etc was admitted")
	}
}

// TestPolicyBypassRestoreTargetParentSwapTOCTOU reproduces the verifier's bypass (b):
// admission runs at t0, the parent is swapped for a symlink in the window,
// and the switch at t1 must refuse rather than follow the link. The old
// single Lstat before os.Rename only looked at the final component, so the
// rename followed the swapped parent and materialized the root-extracted
// tree wherever the symlink pointed.
func TestPolicyBypassRestoreTargetParentSwapTOCTOU(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(base, "attacker")
	if err := os.MkdirAll(attacker, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(work, "root")
	policy := RestorePolicy{AllowedRootPrefixes: []string{base}, AllowSystemTreeTargets: []string{os.TempDir()}}
	if err := policy.CheckRestoreTarget(target, testWorkload(os.Geteuid()), target); err != nil {
		t.Fatalf("admission must pass before the swap: %v", err)
	}

	staging := filepath.Join(base, "staging")
	staged := filepath.Join(staging, "root")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staged, "payload"), []byte("new tree"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The extraction window: the parent is renamed away and a symlink takes
	// its place. The verifier demonstrated exactly this rename semantics.
	if err := os.Rename(work, filepath.Join(base, "work.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, work); err != nil {
		t.Fatal(err)
	}

	err := materializeRoot(staged, target)
	if err == nil {
		t.Fatal("BYPASS REGRESSED: the switch followed a swapped parent symlink")
	}
	if _, statErr := os.Stat(filepath.Join(attacker, "root")); statErr == nil {
		t.Fatal("BYPASS REGRESSED: the staged tree materialized under the swapped-in location")
	}
	if _, statErr := os.Stat(filepath.Join(base, "work.real", "root")); statErr == nil {
		t.Fatal("the pre-swap location must not have received the tree either")
	}
	if !errors.Is(err, ErrRestoreTargetDenied) {
		t.Fatalf("the refusal must be a destination-policy denial, got %v", err)
	}
}

// TestPolicyBypassPinnedTargetDetectsParentSwap covers the pin the restore switch holds
// across the operation: once a target's parent is pinned, a swap of the
// mounted path is detected on the re-check immediately before the rename.
func TestPolicyBypassPinnedTargetDetectsParentSwap(t *testing.T) {
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
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
	attacker := filepath.Join(base, "attacker")
	if err := os.MkdirAll(attacker, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(work, filepath.Join(base, "work.real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(attacker, work); err != nil {
		t.Fatal(err)
	}
	if err := dir.verify(); err == nil {
		t.Fatal("BYPASS REGRESSED: the pinned target did not notice its parent was replaced")
	}
	if err := dir.renameWithin("root", "moved"); err == nil {
		t.Fatal("BYPASS REGRESSED: a rename ran after the parent was replaced")
	}
	if _, statErr := os.Stat(filepath.Join(attacker, "root")); statErr == nil {
		t.Fatal("nothing may be renamed through the swapped path")
	}
}

// TestPolicyBypassRecoverRestoresBackupWhenRecordStillPreparing is the crash-window
// regression: a crash between the backup rename and the FILESYSTEM_SWITCHED
// write leaves the record at PREPARING with the backup on disk. Recovery must
// restore the original root — the old rollback keyed off the state alone and
// stranded the original in the backup while the unvalidated tree stayed live.
func TestPolicyBypassRecoverRestoresBackupWhenRecordStillPreparing(t *testing.T) {
	fixture := newLazyFixture(t, fakeEngine{})
	target := fixture.workload.Spec.RootPath
	parent := filepath.Dir(target)
	backup := filepath.Join(parent, ".shift-backup-crashsim")
	staging := filepath.Join(parent, ".shift-restore-crashsim")
	restoreDir := filepath.Join(t.TempDir(), "restore-crashsim")

	// The crash state: the original root is already renamed to the backup
	// and an unvalidated tree occupies the target.
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "marker"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "marker"), []byte("unvalidated"), 0o600); err != nil {
		t.Fatal(err)
	}

	record := RestoreRecord{
		ID: "crashsim", CheckpointID: "ckpt", WorkloadID: fixture.workload.Spec.ID,
		State: RestorePreparing, TargetRoot: target, BackupRoot: backup,
		StagingRoot: staging, RestoreDirectory: restoreDir,
	}
	if err := fixture.restorer.records.Put(record.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.restorer.Recover(context.Background()); err != nil {
		t.Fatalf("recovery must succeed from the crash state: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(target, "marker"))
	if err != nil || string(content) != "original" {
		t.Fatalf("the original root must come back from the backup, got %q %v", content, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatal("the backup must be consumed by the restore")
	}
	stored, err := fixture.restorer.Get(record.ID)
	if err != nil || stored.State != RestoreRolledBack {
		t.Fatalf("the record must end rolled back, got %+v %v", stored, err)
	}
}

// TestPolicyBypassRecheckPinnedTargetDetectsTargetSwap covers the admission-
// to-switch window: CheckRestoreTarget verifies ownership at admission and
// closes its handle, and the extraction window that follows lets the target
// (or its entries) be swapped by whoever owns the parent. The switch must
// re-run the ownership verdict on its own pinned handle immediately before
// the backup rename — a target that is no longer the admitted real directory
// is refused.
func TestPolicyBypassRecheckPinnedTargetDetectsTargetSwap(t *testing.T) {
	base := t.TempDir()
	policy := RestorePolicy{AllowedRootPrefixes: []string{base}, AllowSystemTreeTargets: []string{os.TempDir()}}
	workload := testWorkload(os.Geteuid())
	target := filepath.Join(base, "app")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := policy.CheckRestoreTarget(target, workload, target); err != nil {
		t.Fatal(err)
	}
	switcher, err := pinTargetDir(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = switcher.Close() }()
	if err := policy.RecheckPinnedTarget(switcher, workload.UID); err != nil {
		t.Fatalf("a consistent pinned target must pass the switch-time recheck: %v", err)
	}
	saved := filepath.Join(base, "app.saved")
	if err := os.Rename(target, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", target); err != nil {
		t.Fatal(err)
	}
	if err := policy.RecheckPinnedTarget(switcher, workload.UID); err == nil {
		t.Fatal("BYPASS REGRESSED: the switch-time recheck admitted a swapped target")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, target); err != nil {
		t.Fatal(err)
	}
	if err := policy.RecheckPinnedTarget(switcher, workload.UID); err != nil {
		t.Fatalf("the restored real directory must pass again: %v", err)
	}
}
