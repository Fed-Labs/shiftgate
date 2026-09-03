package model

import (
	"path/filepath"
	"testing"
)

func TestWorkloadNormalizeRejectsRoot(t *testing.T) {
	spec := WorkloadSpec{Name: "bad", Command: []string{"true"}, RootPath: string(filepath.Separator)}
	if err := spec.Normalize(); err == nil {
		t.Fatal("expected filesystem root capture to be rejected")
	}
}

func TestMigrationTransitionSafety(t *testing.T) {
	migration := Migration{Stage: MigrationCreated}
	if err := migration.Transition(MigrationCommit, "skip validation", 1); err == nil {
		t.Fatal("unsafe transition was accepted")
	}
	if err := migration.Transition(MigrationDiscover, "inventory", 0.05); err != nil {
		t.Fatalf("valid transition rejected: %v", err)
	}
}
