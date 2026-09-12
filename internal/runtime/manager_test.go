package runtime

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

func TestWorkloadLifecycle(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name:       "test",
		Command:    []string{script},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	workload, err = manager.Start(workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workload.Process == nil {
		t.Fatal("started workload has no process")
	}
	if _, err := manager.Pause(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resume(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Stop(workload.Spec.ID, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != model.WorkloadStopped || stopped.Process != nil {
		t.Fatalf("unexpected stopped state: %+v", stopped)
	}
}

// TestStopCheckpointedWorkloadSkipsGracePeriod: a checkpointed tree sits
// stopped where CRIU left it — a SIGTERM would queue behind the stop and burn
// the whole grace period, and resuming it with SIGCONT first would let the
// frozen rollback copy execute. Stop must kill it outright and return
// promptly; the checkpoint's data is already durable in the chunk store. The
// workload traps and ignores SIGTERM so only the immediate SIGKILL can end it.
func TestStopCheckpointedWorkloadSkipsGracePeriod(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name:       "checkpointed",
		Command:    []string{script},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	// Mark the workload checkpointed the way a stopped checkpoint leaves it.
	if _, err := manager.MarkCheckpoint(workload.Spec.ID, "checkpoint-test", false); err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Now()
	stopped, err := manager.Stop(workload.Spec.ID, 5*time.Second)
	elapsed := time.Since(stoppedAt)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != model.WorkloadStopped || stopped.Process != nil {
		t.Fatalf("unexpected stopped state: %+v", stopped)
	}
	// The TERM-immune process dies only by SIGKILL, which Stop must send
	// immediately for a checkpointed tree — never after a grace period.
	if elapsed > 2*time.Second {
		t.Fatalf("stopping a checkpointed workload burned %s; a grace period was waited out", elapsed)
	}
}

// waitForStatus polls the manager's record until the workload reaches the
// wanted status — the exit is recorded by the asynchronous waiter, not by
// the call that started the process.
func waitForStatus(t *testing.T, manager *Manager, id string, want model.WorkloadStatus) model.Workload {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		workload, err := manager.Get(id)
		if err != nil {
			t.Fatalf("read workload: %v", err)
		}
		if workload.Status == want {
			return workload
		}
		time.Sleep(50 * time.Millisecond)
	}
	workload, _ := manager.Get(id)
	t.Fatalf("workload %s never reached %s (last %s, error %q)", id, want, workload.Status, workload.LastError)
	return model.Workload{}
}

// TestWorkloadDeathIsFailure: a process that ends without the agent asking
// for it — a cgroup OOM kill, a crash, a signal from outside — is a failed
// workload, never a stopped one, and the record carries the kernel's reason.
// A retirement the agent itself ordered, the way a committed restore reaps a
// superseded source, stays an ordinary stop.
func TestWorkloadDeathIsFailure(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	crashScript := filepath.Join(root, "crash.sh")
	if err := os.WriteFile(crashScript, []byte("#!/bin/sh\nkill -9 $$\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	crashed, err := manager.Create(model.WorkloadSpec{
		Name:       "crash",
		Command:    []string{crashScript},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(crashed.Spec.ID); err != nil {
		t.Fatal(err)
	}
	failed := waitForStatus(t, manager, crashed.Spec.ID, model.WorkloadFailed)
	if failed.Process != nil {
		t.Fatal("failed workload still reports a process")
	}
	if failed.LastError == "" {
		t.Fatal("failed workload records no reason for its death")
	}

	loopScript := filepath.Join(root, "loop.sh")
	if err := os.WriteFile(loopScript, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	retired, err := manager.Create(model.WorkloadSpec{
		Name:       "retire",
		Command:    []string{loopScript},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(retired.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager.Retire(retired.Spec.ID)
	if err := syscall.Kill(-started.Process.PGID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	stopped := waitForStatus(t, manager, retired.Spec.ID, model.WorkloadStopped)
	if stopped.Process != nil {
		t.Fatal("stopped workload still reports a process")
	}
	if stopped.LastError != "" {
		t.Fatalf("retired workload records an error: %q", stopped.LastError)
	}
}

func TestSetCheckpointPolicy(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name: "policed", Command: []string{script}, RootPath: root, WorkingDir: root,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetCheckpointPolicy(workload.Spec.ID, &model.CheckpointPolicySpec{IntervalSeconds: 5}); err == nil {
		t.Fatal("a policy below the interval floor must be rejected")
	}
	updated, err := manager.SetCheckpointPolicy(workload.Spec.ID, &model.CheckpointPolicySpec{IntervalSeconds: 600, KeepLast: 4})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Spec.CheckpointPolicy == nil || updated.Spec.CheckpointPolicy.IntervalSeconds != 600 || updated.Spec.CheckpointPolicy.KeepLast != 4 {
		t.Fatalf("policy was not applied: %+v", updated.Spec.CheckpointPolicy)
	}
	// The persisted record carries the policy, so the schedule survives an
	// agent restart.
	reopened, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Get(workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Spec.CheckpointPolicy == nil || persisted.Spec.CheckpointPolicy.KeepLast != 4 {
		t.Fatalf("policy did not survive reopening the manager: %+v", persisted.Spec.CheckpointPolicy)
	}
	cleared, err := reopened.SetCheckpointPolicy(workload.Spec.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Spec.CheckpointPolicy != nil {
		t.Fatalf("a nil policy must disable scheduling: %+v", cleared.Spec.CheckpointPolicy)
	}
}

// TestSetFailoverPolicy pins the setter guards around the policy pair: a
// standby designation cannot arrive before a checkpoint schedule, a schedule
// cannot be removed while a standby waits for replicas, and clearing the
// standby is always allowed.
func TestSetFailoverPolicy(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name: "protected", Command: []string{script}, RootPath: root, WorkingDir: root,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	standby := &model.FailoverPolicySpec{AgentURL: "https://standby.example:9443", KeepLast: 2}
	if _, err := manager.SetFailoverPolicy(workload.Spec.ID, standby); err == nil {
		t.Fatal("a failover policy without a checkpoint policy must be refused")
	}
	if _, err := manager.SetFailoverPolicy(workload.Spec.ID, &model.FailoverPolicySpec{AgentURL: "http://standby.example:9443"}); err == nil {
		t.Fatal("a failover policy the replicator could not dial must be refused")
	}
	if _, err := manager.SetCheckpointPolicy(workload.Spec.ID, &model.CheckpointPolicySpec{IntervalSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	updated, err := manager.SetFailoverPolicy(workload.Spec.ID, standby)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Spec.FailoverPolicy == nil || updated.Spec.FailoverPolicy.AgentURL != "https://standby.example:9443" {
		t.Fatalf("failover policy was not applied: %+v", updated.Spec.FailoverPolicy)
	}
	// Removing the schedule while a standby waits would strand it at the
	// checkpoint it already holds; the removal is refused until the standby
	// designation goes first.
	if _, err := manager.SetCheckpointPolicy(workload.Spec.ID, nil); err == nil {
		t.Fatal("removing the schedule under a live standby designation must be refused")
	}
	cleared, err := manager.SetFailoverPolicy(workload.Spec.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Spec.FailoverPolicy != nil || cleared.Spec.CheckpointPolicy == nil {
		t.Fatalf("clearing the standby must keep the schedule: %+v", cleared.Spec)
	}
	// With the standby gone, the schedule can be removed again.
	unscheduled, err := manager.SetCheckpointPolicy(workload.Spec.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unscheduled.Spec.CheckpointPolicy != nil {
		t.Fatalf("the schedule must be removable once no standby waits: %+v", unscheduled.Spec.CheckpointPolicy)
	}
	// The persisted record carries the designation across a restart.
	reopened, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.SetCheckpointPolicy(workload.Spec.ID, &model.CheckpointPolicySpec{IntervalSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.SetFailoverPolicy(workload.Spec.ID, standby); err != nil {
		t.Fatal(err)
	}
	persisted, err := reopened.Get(workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Spec.FailoverPolicy == nil || persisted.Spec.FailoverPolicy.KeepLast != 2 {
		t.Fatalf("failover policy did not survive reopening the manager: %+v", persisted.Spec.FailoverPolicy)
	}
}
