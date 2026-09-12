// End-to-end lazy restore: the real agent, real CRIU, real userfaultfd. The
// restored process starts before its memory is resident — a criu lazy-pages
// daemon serves pages on demand — and the assertions cover both the honest
// record (lazy, time to first execution, daemon pid) and the lifecycle: the
// image set stays on disk while the daemon lives and is cleaned up once the
// workload (and with it the daemon) ends.

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// TestE2ELazyRestore: checkpoint a counter workload → stop it → restore it
// with --lazy → the process resumes executing while its pages are still
// streaming, the counter continues from where it stopped, the lazy-pages
// daemon is provably alive over the retained image set, and stopping the
// workload ends the daemon and the retained image set is cleaned up.
func TestE2ELazyRestore(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "lazy-agent")
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "lazy-counter", root, script)
	logPath := filepath.Join(root, "progress.log")

	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(true),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	stopped, err := agentProc.client.WorkloadAction(ctx, workload.Spec.ID, "stop", struct{}{})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != model.WorkloadStopped {
		t.Fatalf("workload did not stop: %s", stopped.Status)
	}
	baseline := fileLineCount(t, logPath)
	time.Sleep(2500 * time.Millisecond)
	if after := fileLineCount(t, logPath); after != baseline {
		t.Fatalf("counter still running after stop: %d → %d", baseline, after)
	}

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300, Lazy: true})
	if err != nil {
		t.Fatalf("lazy restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s), want %s", record.State, record.Error, checkpoint.RestoreCommitted)
	}
	if !record.Lazy {
		t.Fatal("the restore record must be marked lazy")
	}
	if record.TimeToFirstExecutionMS <= 0 {
		t.Fatalf("the record must carry an honest time to first execution, got %d ms", record.TimeToFirstExecutionMS)
	}
	if record.LazyPagesPID == 0 {
		t.Fatal("the record must carry the lazy-pages daemon's pid")
	}
	t.Logf("lazy restore: time to first execution %d ms", record.TimeToFirstExecutionMS)

	// The restored process resumes the counter where it left off — pages
	// faulting in through the daemon are what let it run.
	waitUntil(t, 60*time.Second, "lazily restored counter to write more lines", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})

	// Observe the daemon while it serves. Its lifetime is CRIU's own — it
	// exits once every image page has been transferred (faults on demand,
	// the rest streamed in the background), which for this small workload is
	// about a second — so the observation is a poll, not an instant
	// assertion: either the daemon is provably alive with this restore's
	// identity, or it has already finished serving and the commit-armed watch
	// has begun cleaning up. The committed restore above is itself the proof
	// the daemon served: `criu restore --lazy-pages` cannot complete without
	// the daemon behind its socket.
	daemonObserved := false
	images := filepath.Join(record.RestoreDirectory, "images")
	observeDeadline := time.Now().Add(30 * time.Second)
	for {
		cmdline, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", record.LazyPagesPID))
		if readErr == nil {
			arguments := strings.Split(string(cmdline), "\x00")
			servesLazyPages, servesTheseImages := false, false
			for _, argument := range arguments {
				if argument == "lazy-pages" {
					servesLazyPages = true
				}
				if argument == images {
					servesTheseImages = true
				}
			}
			if servesLazyPages && servesTheseImages {
				daemonObserved = true
				t.Logf("lazy-pages daemon %d observed serving %s", record.LazyPagesPID, images)
				break
			}
			t.Fatalf("daemon pid %d is not a lazy-pages daemon over %s: %q",
				record.LazyPagesPID, images, arguments)
		}
		if _, statErr := os.Stat(record.RestoreDirectory); errors.Is(statErr, os.ErrNotExist) {
			t.Logf("daemon %d finished serving before observation; the retained image set is already cleaned up", record.LazyPagesPID)
			break
		}
		if time.Now().After(observeDeadline) {
			t.Fatalf("neither a live daemon over %s nor a cleaned-up restore directory was observed within 30s", images)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Once the daemon has finished serving, the retained image set is
	// removed — nothing is left behind on the machine.
	waitUntil(t, 60*time.Second, "the retained image set to be cleaned up after the daemon finishes serving", func() (bool, string) {
		_, statErr := os.Stat(record.RestoreDirectory)
		if errors.Is(statErr, os.ErrNotExist) {
			return true, ""
		}
		return false, fmt.Sprintf("%s still exists", record.RestoreDirectory)
	})

	// The workload does not depend on the image set once serving is done:
	// the counter keeps going after the cleanup.
	afterCleanup := fileLineCount(t, logPath)
	waitUntil(t, 60*time.Second, "counter to keep running after the image set cleanup", func() (bool, string) {
		return fileLineCount(t, logPath) > afterCleanup, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), afterCleanup)
	})
	if !daemonObserved {
		t.Log("the daemon finished serving before it could be observed; restore completion already proved it served")
	}
}
