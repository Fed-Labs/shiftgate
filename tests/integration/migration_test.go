// Real cross-agent migrations over real TLS: two full agents on this host,
// the source orchestrating a checkpoint → key transfer → chunk upload →
// destination restore → commit, exactly what runs between two machines.

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

// TestE2EMigrationCompletes: start a workload on one agent, migrate it to a
// second agent, and verify the process resumes on the destination with its
// counter intact while the source is stopped and cleaned up.
func TestE2EMigrationCompletes(t *testing.T) {
	requireE2E(t)
	source := startAgent(t, "migration-source")
	destination := startAgent(t, "migration-destination")
	if source.machineID(t) == destination.machineID(t) {
		t.Fatal("the two agents report the same machine identity")
	}

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "migrate-me", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	created, err := source.client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: destination.machineID(t),
			AgentURL:  destination.peerURL(),
		},
		Mode:           model.MigrationCold,
		TimeoutSeconds: 240,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	migration := waitForMigrationTerminal(t, source.client, created.ID)
	if migration.Stage != model.MigrationCompleted {
		t.Fatalf("migration ended in %s (%s: %s)", migration.Stage, migration.FailureCode, migration.FailureReason)
	}
	if migration.SourcePreserved {
		t.Fatal("completed migration still reports the source as preserved")
	}
	if migration.Metrics.TransferredBytes <= 0 {
		t.Fatalf("migration transferred no bytes: %+v", migration.Metrics)
	}

	// Destination: the same workload ID, running, with the counter resumed.
	destinationWorkload := waitForWorkload(t, destination.client, workload.Spec.ID, model.WorkloadRunning)
	if destinationWorkload.Spec.ID != workload.Spec.ID {
		t.Fatalf("destination runs workload %s, want %s", destinationWorkload.Spec.ID, workload.Spec.ID)
	}
	waitUntil(t, 90*time.Second, "migrated counter to keep counting on the destination", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})

	// Source: stopped, no live process, no reclamation of the root.
	sourceWorkload, err := source.client.Workload(ctx, workload.Spec.ID)
	if err != nil {
		t.Fatalf("read source workload: %v", err)
	}
	if sourceWorkload.Status != model.WorkloadStopped {
		t.Fatalf("source workload is %s after migration, want stopped", sourceWorkload.Status)
	}
	if sourceWorkload.Process != nil {
		t.Fatalf("source still reports a live process (pid %d)", sourceWorkload.Process.PID)
	}
}

// TestE2EMigrationUnreachableDestination: the destination address points at a
// port with nothing listening. The migration must fail cleanly at discovery,
// roll back, and — the invariant that matters most — resume the source
// workload so the application never disappears.
func TestE2EMigrationUnreachableDestination(t *testing.T) {
	requireE2E(t)
	source := startAgent(t, "unreachable-source")

	deadPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	// freePort binds, reads the port, and closes: the port is currently
	// free, so a connection to it must be refused — not hang, not reach a
	// stray listener.

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "stays-here", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	created, err := source.client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			AgentURL: fmt.Sprintf("https://127.0.0.1:%d", deadPort),
		},
		Mode:           model.MigrationCold,
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	migration := waitForMigrationTerminal(t, source.client, created.ID)
	if migration.Stage != model.MigrationRolledBack {
		t.Fatalf("migration to a dead destination ended in %s (%s: %s), want ROLLED_BACK",
			migration.Stage, migration.FailureCode, migration.FailureReason)
	}
	if !migration.SourcePreserved {
		t.Fatal("failed migration did not preserve the source")
	}
	if migration.FailureCode != "DESTINATION_UNREACHABLE" {
		t.Fatalf("failure code was %s, want DESTINATION_UNREACHABLE", migration.FailureCode)
	}

	// The source workload survived: running again, and its counter never
	// stopped for long — the lines keep accumulating past the baseline.
	resumed := waitForWorkload(t, source.client, workload.Spec.ID, model.WorkloadRunning)
	if resumed.Process == nil {
		t.Fatal("resumed workload has no live process")
	}
	waitUntil(t, 60*time.Second, "source counter to keep counting after rollback", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
}

// TestE2EMigrationDestinationDiesMidTransfer: the destination agent is killed
// while the migration is in flight. The migration must fail, roll back what
// it can, and resume the source workload — a mid-flight destination death
// can never strand the application.
func TestE2EMigrationDestinationDiesMidTransfer(t *testing.T) {
	requireE2E(t)
	source := startAgent(t, "die-source")
	destination := startAgent(t, "die-destination")

	// A large incompressible payload keeps the transfer stage alive long
	// enough to kill inside it, rather than after it.
	root := t.TempDir()
	payload := filepath.Join(root, "payload.bin")
	if err := writeRandomFile(t, payload, 256<<20); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "mid-flight", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	created, err := source.client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: destination.machineID(t),
			AgentURL:  destination.peerURL(),
		},
		Mode:           model.MigrationCold,
		TimeoutSeconds: 240,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	// Kill the destination the moment the migration reaches the transfer
	// stage — after reservation, so the rollback path must also handle a
	// dead peer.
	killDeadline := time.Now().Add(2 * time.Minute)
	killed := false
	for time.Now().Before(killDeadline) {
		migration, err := source.client.Migration(ctx, created.ID)
		if err != nil {
			t.Fatalf("read migration: %v", err)
		}
		if migration.Stage == model.MigrationTransfer {
			destination.stop(t)
			killed = true
			break
		}
		if migrationTerminal(migration.Stage) {
			if migration.Stage == model.MigrationCompleted {
				t.Fatal("migration finished before the destination could be killed; payload too small to catch the transfer stage")
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !killed {
		t.Fatalf("never observed the transfer stage (last stage visible before terminal: see migration %s)", created.ID)
	}

	migration := waitForMigrationTerminal(t, source.client, created.ID)
	if migration.Stage == model.MigrationCompleted {
		t.Fatal("migration completed despite the destination dying mid-transfer")
	}
	if !migration.SourcePreserved {
		t.Fatal("failed migration did not preserve the source")
	}

	// The source workload must be back and still counting.
	waitForWorkload(t, source.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 120*time.Second, "source counter to keep counting after the destination died", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
}

// TestE2EAgentRestartResume: machine disconnect, machine restart. A workload
// is checkpointed, the whole agent dies, a new agent process boots over the
// same state directory, and the checkpoint restores — the application
// resumes from where the old machine left off, with its counter intact.
func TestE2EAgentRestartResume(t *testing.T) {
	requireE2E(t)
	stateDir := t.TempDir()
	socketDir := t.TempDir()
	first := startAgentAt(t, "restart-first", stateDir, socketDir)

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, first.client, "across-restart", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := first.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, first.client, workload.Spec.ID, model.WorkloadCheckpointed)
	baseline := fileLineCount(t, logPath)
	identity := first.machineID(t)

	// The machine dies: agent killed, socket gone. The state directory is
	// all that survives.
	first.stop(t)

	second := startAgentAt(t, "restart-second", stateDir, t.TempDir())
	if second.machineID(t) != identity {
		t.Fatal("restarted agent has a different machine identity; state was not recovered")
	}

	// The checkpoint survived the restart and is visible through the new
	// process; restoring it resumes the counter past the baseline.
	record, err := second.client.Restore(ctx, manifest.ID, 300)
	if err != nil {
		t.Fatalf("restore after restart: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore after restart ended in %s (error: %s)", record.State, record.Error)
	}
	waitForWorkload(t, second.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 90*time.Second, "resumed counter to pass the pre-restart baseline", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
}
