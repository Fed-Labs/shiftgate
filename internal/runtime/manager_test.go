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
