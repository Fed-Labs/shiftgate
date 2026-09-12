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

func TestCheckpointPolicyValidation(t *testing.T) {
	var absent *CheckpointPolicySpec
	if err := absent.Validate(); err != nil {
		t.Fatalf("a nil policy means no scheduling and must be valid: %v", err)
	}
	for name, policy := range map[string]CheckpointPolicySpec{
		"interval below the floor": {IntervalSeconds: 9},
		"negative interval":        {IntervalSeconds: -60},
		"negative keep":            {IntervalSeconds: 600, KeepLast: -1},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	policy := CheckpointPolicySpec{IntervalSeconds: 600, KeepLast: 5}
	if err := policy.Validate(); err != nil {
		t.Fatalf("a sane policy must be valid: %v", err)
	}
	spec := WorkloadSpec{Name: "policed", Command: []string{"true"}, RootPath: t.TempDir(), CheckpointPolicy: &CheckpointPolicySpec{IntervalSeconds: 5}}
	if err := spec.Normalize(); err == nil {
		t.Fatal("Normalize must reject a policy the scheduler could not honor")
	}
}

// TestFailoverPolicyValidation pins the standby designation's own rules and
// its pairing with a checkpoint schedule: without one, nothing would ever be
// replicated, so a failover policy alone must never normalize.
func TestFailoverPolicyValidation(t *testing.T) {
	var absent *FailoverPolicySpec
	if err := absent.Validate(); err != nil {
		t.Fatalf("a nil policy means no standby and must be valid: %v", err)
	}
	for name, policy := range map[string]FailoverPolicySpec{
		"missing url":        {},
		"plain http":         {AgentURL: "http://standby.example:9443"},
		"query string":       {AgentURL: "https://standby.example:9443/?peer=1"},
		"fragment":           {AgentURL: "https://standby.example:9443/#main"},
		"hostless":           {AgentURL: "https://"},
		"negative keep":      {AgentURL: "https://standby.example:9443", KeepLast: -1},
		"not a URL at all":   {AgentURL: "standby.example"},
		"unix socket scheme": {AgentURL: "unix:///run/shift/agent.sock"},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	policy := FailoverPolicySpec{AgentURL: "https://standby.example:9443", MachineID: "machine-standby", KeepLast: 3}
	if err := policy.Validate(); err != nil {
		t.Fatalf("a pinned https standby must be valid: %v", err)
	}
	// The pair: a standby without a schedule replicates nothing, forever.
	orphan := WorkloadSpec{Name: "stranded", Command: []string{"true"}, RootPath: t.TempDir(),
		FailoverPolicy: &FailoverPolicySpec{AgentURL: "https://standby.example:9443"}}
	if err := orphan.Normalize(); err == nil {
		t.Fatal("a failover policy without a checkpoint policy must be refused")
	}
	paired := orphan
	paired.CheckpointPolicy = &CheckpointPolicySpec{IntervalSeconds: 600}
	if err := paired.Normalize(); err != nil {
		t.Fatalf("a policy pair must normalize: %v", err)
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

// TestMigrationFailureRollsBackDirectly pins the stage machine's
// rollback-reachability invariant: a failure at any pre-commit stage must be
// able to enter ROLLING_BACK without passing through FAILED. FAILED is a
// terminal stage for every observer — the CLI, the dashboard, the policy
// scheduler — so routing a rolling-back migration through it makes a
// preserved source look like an abandoned one.
func TestMigrationFailureRollsBackDirectly(t *testing.T) {
	preCommit := []MigrationStage{
		MigrationDiscover, MigrationValidate, MigrationSnapshot, MigrationPrepare,
		MigrationTransfer, MigrationVerify, MigrationRestore, MigrationPostValidate,
		MigrationSwitch, MigrationCommit,
	}
	for _, stage := range preCommit {
		if !CanTransition(stage, MigrationRollingBack) {
			t.Fatalf("stage %s cannot roll back directly — a failure there would be observable as terminal FAILED", stage)
		}
	}
	if CanTransition(MigrationCompleted, MigrationRollingBack) {
		t.Fatal("a completed migration must never roll back")
	}
}
