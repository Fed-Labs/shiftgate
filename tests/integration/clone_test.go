package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// TestE2ECheckpointClone proves a clone set: one checkpoint, several
// independent running workloads, every one of them a live continuation of the
// checkpointed process. Each clone's counter must continue from the baseline
// the source left — the same evidence a restore test uses, replicated per
// member — and every member must hold its own root, its own workload record,
// and its own process.
func TestE2ECheckpointClone(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "clone-agent")
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "clone-source", root, script)
	logPath := filepath.Join(root, "progress.log")

	waitUntil(t, 30*time.Second, "source counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID: workload.Spec.ID,
		// LeaveRunning false is explicit: the API defaults an omitted field
		// to leave-running, and clones continue the frozen process's state —
		// a live source would keep writing to the source log, making
		// per-clone counters ambiguous evidence.
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	stopped := waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
	baseline := fileLineCount(t, logPath)
	_ = stopped

	const count = 3
	record, err := agentProc.client.CloneCheckpoint(ctx, manifest.ID, agentclient.CloneRequest{
		Count: count, Parallel: 2, TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if record.State != checkpoint.CloneCommitted {
		t.Fatalf("clone ended in %s (error: %s), want %s", record.State, record.Error, checkpoint.CloneCommitted)
	}
	if record.Count != count || len(record.Members) != count {
		t.Fatalf("clone set has %d members (count=%d), want %d", len(record.Members), record.Count, count)
	}
	if record.DurationMS <= 0 {
		t.Fatal("committed clone set reports no duration")
	}

	// Every member is its own workload, running its own process, with its own
	// root directory — none of them is the source.
	byName := make(map[string]bool, count)
	for _, member := range record.Members {
		if member.PID == 0 {
			t.Fatalf("member %s reports no pid", member.Name)
		}
		if member.RootPath == root {
			t.Fatalf("member %s shares the source root", member.Name)
		}
		if member.RootPath == "" || member.WorkloadID == workload.Spec.ID {
			t.Fatalf("member %s has no own identity", member.Name)
		}
		if info, statErr := os.Stat(member.RootPath); statErr != nil || !info.IsDir() {
			t.Fatalf("member %s root %s missing: %v", member.Name, member.RootPath, statErr)
		}
		byName[member.Name] = true
		running := waitForWorkload(t, agentProc.client, member.WorkloadID, model.WorkloadRunning)
		if running.Spec.RootPath != member.RootPath {
			t.Fatalf("member %s workload root %s does not match the record %s", member.Name, running.Spec.RootPath, member.RootPath)
		}
		// The clone's own counter continues the checkpointed sequence —
		// evidence that each member is a real continuation, not a restart.
		cloneLog := filepath.Join(member.RootPath, "progress.log")
		waitUntil(t, 60*time.Second, fmt.Sprintf("clone %s counter to continue past the baseline", member.Name), func() (bool, string) {
			return fileLineCount(t, cloneLog) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, cloneLog), baseline)
		})
	}
	if len(byName) != count {
		t.Fatalf("clone member names are not unique: %d distinct of %d", len(byName), count)
	}

	// The clone list carries the set; the inspect view agrees.
	sets, err := agentProc.client.Clones(ctx)
	if err != nil {
		t.Fatalf("clone list: %v", err)
	}
	found := false
	for _, set := range sets {
		if set.ID == record.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("clone list does not contain the committed set")
	}
	inspected, err := agentProc.client.Clone(ctx, record.ID)
	if err != nil {
		t.Fatalf("clone inspect: %v", err)
	}
	if inspected.State != checkpoint.CloneCommitted || len(inspected.Members) != count {
		t.Fatalf("clone inspect disagrees: state=%s members=%d", inspected.State, len(inspected.Members))
	}
	t.Logf("clone set %s: %d members in %dms (cloned files %d, copied %d)",
		record.ID, count, record.DurationMS, record.ClonedFiles, record.CopiedFiles)

	// A committed set is a fleet of ordinary workloads: rollback is refused
	// so teardown always goes through the workload API.
	if _, err := agentProc.client.CloneRollback(ctx, record.ID); err == nil {
		t.Fatal("rollback of a committed clone set was accepted")
	}

	// Teardown through the workload API, and the roots go with the workloads
	// being deleted only if the operator removes them; the test cleans its
	// own tree.
	for _, member := range record.Members {
		if _, err := agentProc.client.WorkloadAction(ctx, member.WorkloadID, "stop", struct{}{}); err != nil {
			t.Fatalf("stop member %s: %v", member.Name, err)
		}
		if err := agentProc.client.DeleteWorkload(ctx, member.WorkloadID); err != nil {
			t.Fatalf("delete member %s: %v", member.Name, err)
		}
	}
}
