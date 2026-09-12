// Integrity: a corrupted chunk store must be detected, never silently
// restored. The spec's rule is absolute — never claim success when state
// cannot be restored — so the assertion here is that restore FAILS with the
// real corruption error and leaves nothing running.

package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/model"
)

// TestE2ECheckpointCorruptionDetected: checkpoint a workload, flip bytes in
// an encrypted chunk on disk, and verify restore refuses to proceed.
func TestE2ECheckpointCorruptionDetected(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "corruption-agent")

	root := t.TempDir()
	// A random payload guarantees each chunk is distinct ciphertext; a
	// deduplicated or repeating store could otherwise make corruption
	// invisible by restoring an intact duplicate.
	payload := filepath.Join(root, "payload.bin")
	if err := writeRandomFile(t, payload, 16<<20); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, agentProc.client, "corruptible", root, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)

	// The agent's chunk store lives under its state directory: chunks of
	// encrypted checkpoint data, addressed by content. Corrupting one on disk
	// is the on-disk failure this test exists for.
	corrupted, err := corruptFirstChunk(t, filepath.Join(agentProc.stateDir, "objects", "chunks"))
	if err != nil {
		t.Fatalf("corrupt a chunk: %v", err)
	}
	t.Logf("corrupted chunk %s", corrupted)

	// Restore over corrupted state: it must fail, with the failure visible
	// to the caller — not a hang, not a silent success.
	_, restoreErr := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 120})
	if restoreErr == nil {
		t.Fatal("restore succeeded over corrupted state; corruption went undetected")
	}
	t.Logf("restore failed as required: %v", restoreErr)

	// And nothing is running afterward: a failed restore must not leave a
	// half-restored process behind.
	time.Sleep(500 * time.Millisecond)
	after, err := agentProc.client.Workload(ctx, workload.Spec.ID)
	if err != nil {
		t.Fatalf("read workload after failed restore: %v", err)
	}
	if after.Status == model.WorkloadRunning {
		t.Fatalf("workload is running after a failed restore (pid %d)", after.Process.PID)
	}
}
