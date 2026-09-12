package integration

// Cgroup removal semantics the migration cleanup depends on: a directory
// that still holds a live process is another tenant's — a second agent's
// tree in a shared root — and is left standing with ErrCgroupOccupied,
// promptly rather than after a retry deadline burns; a directory whose last
// process is gone is removed outright.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"shift.dev/shift/internal/runtime"
)

func TestCgroupRemoveOccupiedAndEmpty(t *testing.T) {
	requireE2E(t)
	root := filepath.Join("/sys/fs/cgroup", "shift-e2e-cgroup-test")
	manager, err := runtime.NewCgroupManager(true, root)
	if err != nil {
		t.Fatalf("open cgroup manager under %s: %v", root, err)
	}
	workloadPath := filepath.Join(root, "occupied")
	if err := os.MkdirAll(workloadPath, 0o755); err != nil {
		t.Fatalf("create workload cgroup: %v", err)
	}
	sleep := exec.Command("sleep", "300")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleep.Process.Kill()
		_ = sleep.Wait()
		_ = os.RemoveAll(root)
	})
	if err := os.WriteFile(filepath.Join(workloadPath, "cgroup.procs"),
		[]byte(strconv.Itoa(sleep.Process.Pid)), 0o644); err != nil {
		t.Fatalf("attach process to cgroup: %v", err)
	}

	removedAt := time.Now()
	err = manager.Remove("occupied")
	elapsed := time.Since(removedAt)
	if !errors.Is(err, runtime.ErrCgroupOccupied) {
		t.Fatalf("removing an occupied cgroup: got %v, want ErrCgroupOccupied", err)
	}
	// The occupied verdict must be prompt — a deadline burn here is exactly
	// the four-second cleanup window this behavior exists to eliminate.
	if elapsed > 700*time.Millisecond {
		t.Fatalf("occupied verdict took %s; the retry deadline burned", elapsed)
	}
	if _, statErr := os.Stat(workloadPath); statErr != nil {
		t.Fatalf("occupied cgroup was removed anyway: %v", statErr)
	}
	// The occupant is untouched — removal must never take another tenant's
	// process down with it.
	if err := syscall.Kill(sleep.Process.Pid, 0); err != nil {
		t.Fatalf("the occupying process was killed by the removal attempt: %v", err)
	}

	// Once the process is dead and reaped the directory is empty, and
	// removal succeeds outright.
	if err := sleep.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Wait only reaps; its error is the kill we just asked for.
	_ = sleep.Wait()
	if err := manager.Remove("occupied"); err != nil {
		t.Fatalf("removing an emptied cgroup: %v", err)
	}
	if _, statErr := os.Stat(workloadPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("emptied cgroup still exists: %v", statErr)
	}
	if err := manager.Remove("occupied"); err != nil {
		t.Fatalf("removing a missing cgroup should be a no-op: %v", err)
	}
}
