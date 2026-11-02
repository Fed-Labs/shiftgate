package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

// writeVerifiedState builds a minimal but complete encrypted state directory:
// keyring, chunk store, signed checkpoint manifest, and index entry — the
// shape every real agent state has.
func writeVerifiedState(t *testing.T) string {
	t.Helper()
	stateDir := t.TempDir()
	machine, err := identity.Ensure(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(filepath.Join(stateDir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chunkstore.Open(filepath.Join(stateDir, "objects"), 4<<20, keys); err != nil {
		t.Fatal(err)
	}
	repository, err := checkpoint.OpenRepository(filepath.Join(stateDir, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-1", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{ID: "workload-1", Name: "workload"},
	}
	if err := checkpoint.SignManifest(&manifest, machine); err != nil {
		t.Fatal(err)
	}
	if err := repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

func TestVerifyStateDirRejectsMissingDirectory(t *testing.T) {
	if err := verifyStateDir(context.Background(), filepath.Join(t.TempDir(), "no-such-dir")); err == nil {
		t.Fatal("nonexistent state directory verified as passed")
	}
}

// TestVerifyStateDirRejectsEmptyDirectoryWithoutWritingState is the regression
// guard for both halves of the false-pass: an empty directory must fail, and
// the verifier must not fabricate keys/chunks inside the directory it is
// certifying.
func TestVerifyStateDirRejectsEmptyDirectoryWithoutWritingState(t *testing.T) {
	stateDir := t.TempDir()
	if err := verifyStateDir(context.Background(), stateDir); err == nil {
		t.Fatal("empty state directory verified as passed")
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("verifier wrote into the directory it was asked to certify: %v", entries)
	}
}

func TestVerifyStateDirRejectsPartialState(t *testing.T) {
	stateDir := t.TempDir()
	if _, err := securestore.Open(filepath.Join(stateDir, "keys")); err != nil {
		t.Fatal(err)
	}
	if err := verifyStateDir(context.Background(), stateDir); err == nil {
		t.Fatal("state missing its checkpoint index verified as passed")
	}
}

func TestVerifyStateDirRejectsUnindexedManifest(t *testing.T) {
	stateDir := writeVerifiedState(t)
	orphan := filepath.Join(stateDir, "checkpoints", "orphan-1")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "manifest.enc.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyStateDir(context.Background(), stateDir)
	if err == nil || !strings.Contains(err.Error(), "unindexed") {
		t.Fatalf("unindexed manifest not reported: %v", err)
	}
}

func TestVerifyStateDirRejectsNothingToVerify(t *testing.T) {
	stateDir := writeVerifiedState(t)
	keys, err := securestore.Open(filepath.Join(stateDir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := checkpoint.OpenRepository(filepath.Join(stateDir, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Delete("checkpoint-1"); err != nil {
		t.Fatal(err)
	}
	err = verifyStateDir(context.Background(), stateDir)
	if err == nil || !(strings.Contains(err.Error(), "no checkpoints") || strings.Contains(err.Error(), "nothing was verified")) {
		t.Fatalf("verification of nothing did not fail: %v", err)
	}
}

func TestVerifyStateDirAcceptsCompleteState(t *testing.T) {
	stateDir := writeVerifiedState(t)
	if err := verifyStateDir(context.Background(), stateDir); err != nil {
		t.Fatalf("complete state rejected: %v", err)
	}
}
