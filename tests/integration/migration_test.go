// Real cross-agent migrations over real TLS: two full agents on this host,
// the source orchestrating a checkpoint → key transfer → chunk upload →
// destination restore → commit, exactly what runs between two machines.

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
		for _, event := range migration.Events {
			t.Logf("event %d %s %s %s", event.Sequence, event.Timestamp.Format("15:04:05.000"), event.Stage, event.Message)
		}
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

// TestE2EMigrationDryRun exercises `migrate --dry-run` over real peer TLS: a
// healthy destination comes back compatible with both machines and the
// network plan named, a wrong machine id is refused, the source itself is
// refused as a destination, a dead port is unreachable — and none of it
// creates a migration record or interrupts the workload.
func TestE2EMigrationDryRun(t *testing.T) {
	requireE2E(t)
	source := startAgent(t, "dryrun-source")
	destination := startAgent(t, "dryrun-destination")

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "stays-put", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := source.client.PreflightMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: destination.machineID(t),
			AgentURL:  destination.peerURL(),
		},
		Mode: model.MigrationCold,
	})
	if err != nil {
		t.Fatalf("dry run against the live destination: %v", err)
	}
	if !result.Report.Compatible {
		t.Fatalf("two agents on the same machine must be compatible, got %+v", result.Report.Issues)
	}
	if result.Destination.MachineID != destination.machineID(t) {
		t.Fatalf("dry run resolved destination %s", result.Destination.MachineID)
	}
	if result.SourceMachine.MachineID != source.machineID(t) {
		t.Fatalf("dry run reported source %s", result.SourceMachine.MachineID)
	}
	if result.Network.Summary == "" {
		t.Fatal("the dry run must include the network plan the migration would apply")
	}

	// A machine id that is not the destination's is refused before anything
	// else moves — the same refusal a real migration would get.
	var api *agentclient.APIError
	_, err = source.client.PreflightMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: "not-this-machine",
			AgentURL:  destination.peerURL(),
		},
	})
	if !errors.As(err, &api) || api.Code != "DESTINATION_IDENTITY_MISMATCH" {
		t.Fatalf("a mismatched machine id must fail with DESTINATION_IDENTITY_MISMATCH, got %v", err)
	}
	_, err = source.client.PreflightMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{AgentURL: source.peerURL()},
	})
	if !errors.As(err, &api) || api.Code != "DESTINATION_IS_SOURCE" {
		t.Fatalf("migrating to the source must fail with DESTINATION_IS_SOURCE, got %v", err)
	}
	deadPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.client.PreflightMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{AgentURL: fmt.Sprintf("https://127.0.0.1:%d", deadPort)},
	})
	if !errors.As(err, &api) || api.Code != "DESTINATION_UNREACHABLE" {
		t.Fatalf("a dead destination must fail with DESTINATION_UNREACHABLE, got %v", err)
	}

	// The dry run must leave nothing behind: no migration records, and the
	// workload never stopped counting.
	migrations, err := source.client.Migrations(ctx)
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(migrations) != 0 {
		t.Fatalf("a dry run must not create migration records, got %d", len(migrations))
	}
	waitForWorkload(t, source.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 30*time.Second, "counter to keep advancing through the dry run", func() (bool, string) {
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

// TestE2ELiveMigrationDestinationDiesDuringPreCopy: the destination agent is
// killed while a pre-copy pass is still uploading — before the freeze. The
// migration must fail and preserve a source that was never stopped: the
// passes run while the workload is live, so the failure costs it nothing,
// and the record states that plainly as zero downtime.
func TestE2ELiveMigrationDestinationDiesDuringPreCopy(t *testing.T) {
	requireE2E(t)
	if !interpreterAvailable("python3") {
		t.Skip("python3 is not on PATH")
	}
	source := startAgent(t, "live-die-source")
	destination := startAgent(t, "live-die-destination")

	// A random heap keeps pass 1's upload incompressible — a real transfer,
	// not a compression shortcut. The kill below is timed by watching for the
	// pass event, not by racing the upload's duration, so the heap stays
	// small enough that the migration's honest worst-case reservation (the
	// heap sized once per pass plus the final delta) fits the destination's
	// free storage even on a nearly full disk.
	root := t.TempDir()
	script := filepath.Join(root, "state.py")
	if err := os.WriteFile(script, []byte(`import os, time
blocks = [os.urandom(131072) for _ in range(384)]
log = open("progress.log", "a", 1)
n = 0
while True:
    n += 1
    log.write("%d\n" % n)
    time.sleep(1)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "live-die", root, "python3", script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 60*time.Second, "heap workload to make progress", func() (bool, string) {
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
		Mode:           model.MigrationLive,
		PreCopyPasses:  4,
		TimeoutSeconds: 240,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	// Kill the destination once a pre-copy pass is provably uploading.
	killDeadline := time.Now().Add(2 * time.Minute)
	killed := false
	for time.Now().Before(killDeadline) {
		migration, err := source.client.Migration(ctx, created.ID)
		if err != nil {
			t.Fatalf("read migration: %v", err)
		}
		if migrationTerminal(migration.Stage) {
			t.Fatalf("migration reached %s before a pre-copy pass could be caught; the heap uploaded too fast", migration.Stage)
		}
		for _, event := range migration.Events {
			if strings.Contains(event.Message, "transferring pre-copy pass") {
				destination.stop(t)
				killed = true
				break
			}
		}
		if killed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !killed {
		t.Fatal("never observed a pre-copy pass transfer")
	}

	migration := waitForMigrationTerminal(t, source.client, created.ID)
	if migration.Stage == model.MigrationCompleted {
		t.Fatal("migration completed despite the destination dying mid-pass")
	}
	if !migration.SourcePreserved {
		t.Fatal("failed migration did not preserve the source")
	}
	// The workload was never frozen — passes run while it is live and the
	// failure happened before Finalize — so the record's downtime is zero,
	// not the whole wall clock of the failed attempt.
	if migration.Metrics.Downtime != 0 {
		t.Fatalf("a migration that never froze its workload must report zero downtime, got %s", migration.Metrics.Downtime)
	}
	waitForWorkload(t, source.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 120*time.Second, "source counter to keep counting after the destination died mid-pass", func() (bool, string) {
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
	record, err := second.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
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

// TestE2ELiveMigrationIterativePreCopy: a live migration whose pre-copy loop
// runs up to three passes while the workload keeps running, each pass's
// images transferring to the destination the moment the pass completes — the
// interleaving spec §10 demands — so the freeze at the end carries only the
// final delta. The restore on the destination is the proof that the whole
// pass chain arrived complete; the metrics prove the passes moved while the
// workload was live and the frozen window carried only part of the bytes.
func TestE2ELiveMigrationIterativePreCopy(t *testing.T) {
	requireE2E(t)
	source := startAgent(t, "live-precopy-source")
	destination := startAgent(t, "live-precopy-destination")

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "live-precopy", root, script)
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
		Mode:           model.MigrationLive,
		PreCopyPasses:  3,
		TimeoutSeconds: 240,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	migration := waitForMigrationTerminal(t, source.client, created.ID)
	if migration.Stage != model.MigrationCompleted {
		t.Fatalf("live migration ended in %s (%s: %s)", migration.Stage, migration.FailureCode, migration.FailureReason)
	}
	// The checkpoint that carried the workload across must record the
	// pre-copy loop — the passes that ran, not the cap that was asked for.
	manifest, err := source.client.Checkpoint(ctx, migration.CheckpointID)
	if err != nil {
		t.Fatalf("load migration checkpoint: %v", err)
	}
	if !manifest.Engine.PreCopy || manifest.Engine.PreCopyPasses < 1 || manifest.Engine.PreCopyPasses > 3 {
		t.Fatalf("live checkpoint did not record its pre-copy passes: %+v", manifest.Engine)
	}
	// The passes ran as their own packaged assets, beside the final image set
	// and the filesystem root.
	passAssets := 0
	for _, asset := range manifest.Assets {
		if strings.HasPrefix(asset.Name, "process-state-precopy-") {
			passAssets++
		}
	}
	if passAssets != manifest.Engine.PreCopyPasses {
		t.Fatalf("manifest carries %d pass assets but the engine recorded %d passes", passAssets, manifest.Engine.PreCopyPasses)
	}
	// The interleaving itself, in the migration's own metrics: bytes moved
	// during the passes — while the workload kept running — and more bytes
	// still had to travel inside the frozen window for the final delta and
	// the filesystem root.
	if migration.Metrics.PreCopyTransferredBytes <= 0 {
		t.Fatalf("pre-copy passes transferred nothing before the freeze: %+v", migration.Metrics)
	}
	if migration.Metrics.TransferredBytes <= migration.Metrics.PreCopyTransferredBytes {
		t.Fatalf("the frozen window carried nothing beyond the passes: %+v", migration.Metrics)
	}
	if migration.Metrics.TotalStateBytes <= 0 {
		t.Fatalf("live migration recorded no state bytes: %+v", migration.Metrics)
	}

	// The destination runs the workload and the counter continued — the
	// merged multi-pass image set restored complete state.
	destinationWorkload := waitForWorkload(t, destination.client, workload.Spec.ID, model.WorkloadRunning)
	if destinationWorkload.Process == nil {
		t.Fatal("destination workload has no live process")
	}
	waitUntil(t, 90*time.Second, "migrated counter to keep counting on the destination", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})

	// The source is stopped and clean.
	sourceWorkload, err := source.client.Workload(ctx, workload.Spec.ID)
	if err != nil {
		t.Fatalf("read source workload: %v", err)
	}
	if sourceWorkload.Status != model.WorkloadStopped {
		t.Fatalf("source workload is %s after live migration, want stopped", sourceWorkload.Status)
	}
}
