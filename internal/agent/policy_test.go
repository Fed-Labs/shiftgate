package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

// policyEngine backs the policy loop with a fake CRIU whose dumps succeed and
// write one image file — enough for the loop's scheduling behavior to be
// observed without the real engine.
type policyEngine struct{}

func (policyEngine) Check(context.Context) error                                     { return nil }
func (policyEngine) Version(context.Context) (string, error)                         { return "CRIU policy test", nil }
func (policyEngine) PreDump(context.Context, checkpoint.DumpOptions) error           { return nil }
func (policyEngine) Restore(context.Context, checkpoint.RestoreOptions) (int, error) { return 0, nil }
func (policyEngine) Dump(_ context.Context, options checkpoint.DumpOptions) error {
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}

// policyHarness wires a minimal agent Service — runtime, checkpoint service,
// logger — around one running workload. The workload's spec is created with a
// CreatedAt far enough in the past that a policy interval is already due.
func policyHarness(t *testing.T, policy *model.CheckpointPolicySpec) (*Service, string) {
	t.Helper()
	stateRoot := t.TempDir()
	workloadRoot := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(stateRoot+"/runtime", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workloadRoot, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := model.WorkloadSpec{
		Name: "policed", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(), CheckpointPolicy: policy,
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	workload, err := runtimeManager.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeManager.Start(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = runtimeManager.Stop(workload.Spec.ID, time.Second) })
	machine, err := identity.Ensure(stateRoot + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(stateRoot + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(stateRoot+"/objects", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := checkpoint.OpenRepository(stateRoot+"/checkpoints", keys)
	if err != nil {
		t.Fatal(err)
	}
	checkpoints := checkpoint.NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, policyEngine{}, logger)
	service := &Service{runtime: runtimeManager, checkpoints: checkpoints, logger: logger}
	return service, workload.Spec.ID
}

func TestCheckpointPolicyLoopCreatesAndPrunes(t *testing.T) {
	service, workloadID := policyHarness(t, &model.CheckpointPolicySpec{IntervalSeconds: 10, KeepLast: 2})
	// Two existing checkpoints age past the retention count; once the interval
	// elapses since the newest one, the loop's own checkpoint becomes the
	// third and pushes the oldest out. A manual checkpoint resets the
	// schedule — the interval is measured from the newest protection, not
	// from the workload's creation.
	first, err := service.checkpoints.Create(context.Background(), workloadID, checkpoint.CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := service.checkpoints.Create(context.Background(), workloadID, checkpoint.CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if summaries := service.checkpoints.List(workloadID); len(summaries) != 2 {
		t.Fatalf("manual checkpoints should both exist, got %d", len(summaries))
	}
	time.Sleep(10 * time.Second)

	service.applyCheckpointPolicies(context.Background())

	summaries := service.checkpoints.List(workloadID)
	if len(summaries) != 2 {
		t.Fatalf("retention must leave exactly the kept checkpoints, got %d", len(summaries))
	}
	ids := map[string]bool{summaries[0].ID: true, summaries[1].ID: true}
	if ids[first.ID] {
		t.Fatal("the oldest checkpoint should have been pruned")
	}
	if !ids[second.ID] {
		t.Fatal("the recent manual checkpoint should have been retained")
	}
	// The loop's checkpoint is a full, leave-running checkpoint of a workload
	// that keeps running.
	if summaries[0].Kind != model.CheckpointFull {
		t.Fatalf("policy checkpoints are full checkpoints, got %s", summaries[0].Kind)
	}
	workload, err := service.runtime.Get(workloadID)
	if err != nil || workload.Status != model.WorkloadRunning {
		t.Fatalf("the workload must keep running through a policy checkpoint: %+v %v", workload, err)
	}
}

func TestCheckpointPolicyLoopSkipsAndReschedules(t *testing.T) {
	service, workloadID := policyHarness(t, &model.CheckpointPolicySpec{IntervalSeconds: 600})
	if _, err := service.runtime.Pause(workloadID); err != nil {
		t.Fatal(err)
	}
	service.applyCheckpointPolicies(context.Background())
	if summaries := service.checkpoints.List(workloadID); len(summaries) != 0 {
		t.Fatalf("a paused workload must not be checkpointed, got %d", len(summaries))
	}
	if _, err := service.runtime.Resume(workloadID); err != nil {
		t.Fatal(err)
	}

	service.applyCheckpointPolicies(context.Background())
	if summaries := service.checkpoints.List(workloadID); len(summaries) != 1 {
		t.Fatalf("a due policy should checkpoint once, got %d", len(summaries))
	}
	// The interval is measured from the checkpoint that just happened, so an
	// immediate second pass must not checkpoint again.
	service.applyCheckpointPolicies(context.Background())
	if summaries := service.checkpoints.List(workloadID); len(summaries) != 1 {
		t.Fatalf("checkpointing must wait for the interval to elapse, got %d", len(summaries))
	}
}

func TestCheckpointPolicyLoopIgnoresUnpolicedWorkloads(t *testing.T) {
	service, workloadID := policyHarness(t, nil)
	service.applyCheckpointPolicies(context.Background())
	if summaries := service.checkpoints.List(workloadID); len(summaries) != 0 {
		t.Fatalf("a workload without a policy must never be auto-checkpointed, got %d", len(summaries))
	}
}

func TestMigrationActiveFor(t *testing.T) {
	migrations := []model.Migration{
		{ID: "done", WorkloadID: "w1", Stage: model.MigrationCompleted},
		{ID: "failed", WorkloadID: "w1", Stage: model.MigrationFailed},
		{ID: "rolled-back", WorkloadID: "w1", Stage: model.MigrationRolledBack},
		{ID: "cancelled", WorkloadID: "w1", Stage: model.MigrationCancelled}, //nolint:misspell // mirrors the persisted stage vocabulary
	}
	if migrationActiveFor(migrations, "w1") {
		t.Fatal("terminal migrations must not hold the policy loop back")
	}
	if migrationActiveFor(migrations, "w2") {
		t.Fatal("migrations of other workloads must not block this one")
	}
	migrations = append(migrations, model.Migration{ID: "live", WorkloadID: "w1", Stage: model.MigrationTransfer})
	if !migrationActiveFor(migrations, "w1") {
		t.Fatal("a migration in flight must block the policy loop for its workload")
	}
}
