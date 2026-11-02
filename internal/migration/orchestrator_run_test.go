package migration

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/transfer"
)

// scriptedPeer is a PeerClient whose behavior each test scripts: which
// capabilities to report, how many chunk uploads to fail before succeeding,
// whether rollback fails, and which session state to report. It records the
// calls tests assert on.
type scriptedPeer struct {
	profile model.MachineCapabilities

	uploadFailures   int32
	uploadCalls      int32
	rollbackFailures int32
	rollbackErr      error
	rollbackCalls    int32
	commitCalls      int32
	blockOnManifest  bool
	manifestMissing  []model.ChunkRef
	sessionState     transfer.SessionState
	machineErr       error
	reserveErr       error
	verifyErr        error
	restoreErr       error
	commitErr        error
}

func (p *scriptedPeer) Machine(context.Context) (model.MachineCapabilities, error) {
	return p.profile, p.machineErr
}

func (p *scriptedPeer) Reserve(context.Context, transfer.ReserveRequest) (transfer.Session, error) {
	return transfer.Session{}, p.reserveErr
}

func (p *scriptedPeer) ImportKey(context.Context, string, string, uint32, []byte) error {
	return nil
}

func (p *scriptedPeer) ImportManifest(context.Context, string, model.CheckpointManifest) (transfer.MissingResponse, error) {
	if p.blockOnManifest {
		return transfer.MissingResponse{}, errors.New("scriptedPeer: ImportManifest used without block handler")
	}
	return transfer.MissingResponse{Missing: p.manifestMissing}, nil
}

func (p *scriptedPeer) UploadChunk(ctx context.Context, _ string, _ model.ChunkRef, _ *chunkstore.Store) error {
	atomic.AddInt32(&p.uploadCalls, 1)
	if atomic.AddInt32(&p.uploadFailures, -1) >= 0 {
		return errors.New("scripted transient upload failure")
	}
	return nil
}

func (p *scriptedPeer) Verify(context.Context, string) (transfer.Session, error) {
	return transfer.Session{}, p.verifyErr
}

func (p *scriptedPeer) Restore(context.Context, string, time.Duration) (checkpoint.RestoreRecord, error) {
	return checkpoint.RestoreRecord{}, p.restoreErr
}

func (p *scriptedPeer) Commit(context.Context, string) (transfer.Session, error) {
	atomic.AddInt32(&p.commitCalls, 1)
	return transfer.Session{}, p.commitErr
}

func (p *scriptedPeer) Rollback(context.Context, string) (transfer.Session, error) {
	atomic.AddInt32(&p.rollbackCalls, 1)
	if atomic.AddInt32(&p.rollbackFailures, -1) >= 0 {
		return transfer.Session{}, errors.New("scripted transient rollback failure")
	}
	return transfer.Session{}, p.rollbackErr
}

func (p *scriptedPeer) Get(context.Context, string) (transfer.Session, error) {
	return transfer.Session{State: p.sessionState}, nil
}

func (p *scriptedPeer) GetForCleanup(context.Context, string) (transfer.Session, error) {
	return transfer.Session{State: p.sessionState}, nil
}

// waitingPeer wraps a scriptedPeer and makes ImportManifest block until the
// run context is cancelled — the seam a cancellation test needs, since
// Cancel cancels that context.
type waitingPeer struct {
	*scriptedPeer
	reached chan struct{}
}

func (p *waitingPeer) ImportManifest(ctx context.Context, id string, manifest model.CheckpointManifest) (transfer.MissingResponse, error) {
	select {
	case <-p.reached:
	default:
		close(p.reached)
	}
	<-ctx.Done()
	return transfer.MissingResponse{}, ctx.Err()
}

// waitForStage polls a migration record until predicate holds or the
// deadline expires. Migration bookkeeping runs on its own goroutine; the
// polling window is generous because CI runners are slow, but bounded so a
// regression fails instead of hanging.
func waitForStage(t *testing.T, orchestrator *Orchestrator, id string, predicate func(model.Migration) bool) model.Migration {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		migration, err := orchestrator.Get(id)
		if err == nil && predicate(migration) {
			return migration
		}
		time.Sleep(25 * time.Millisecond)
	}
	migration, err := orchestrator.Get(id)
	t.Fatalf("timed out waiting for migration stage; last=%+v err=%v", migration, err)
	return model.Migration{}
}

// seedMigration inserts a migration record at a chosen stage directly into
// the orchestrator's store — the unit seam for handleFailure /
// handleCancellation / Recover, which take the record as their input.
func seedMigration(t *testing.T, orchestrator *Orchestrator, id, workloadID string, stage model.MigrationStage) {
	t.Helper()
	seedMigrationRecord(t, orchestrator, id, workloadID, stage, model.MigrationCold)
}

// seedMigrationRecord seeds a migration record in a chosen mode: recovery
// rules differ between a cold migration (freeze during its snapshot) and a
// live one (freeze at the final delta).
func seedMigrationRecord(t *testing.T, orchestrator *Orchestrator, id, workloadID string, stage model.MigrationStage, mode model.MigrationMode) {
	t.Helper()
	now := time.Now().UTC()
	err := orchestrator.records.Put(id, model.Migration{
		ID: id, WorkloadID: workloadID, SourceMachineID: "src-machine",
		Destination: model.Destination{AgentURL: "https://peer.example:8443", MachineID: "dst-machine"},
		Mode:        mode, Stage: stage, SourcePreserved: true,
		CreatedAt: now, UpdatedAt: now, Revision: 1,
		Events: []model.MigrationEvent{{Sequence: 1, Stage: stage, Message: "seeded", Timestamp: now}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// pauseWorkload freezes the fixture workload the way a checkpoint does —
// SIGSTOP with the status recorded — so recovery can be observed deciding
// whether to thaw it.
func pauseWorkload(t *testing.T, orchestrator *Orchestrator, workloadID string) {
	t.Helper()
	if _, err := orchestrator.runtime.Pause(workloadID); err != nil {
		t.Fatal(err)
	}
}

// TestUploadChunksRetriesTransientFailures encodes the transfer loop's
// contract: transient per-chunk failures are retried with backoff, every
// chunk's stored size counts toward transferred bytes, and the progress
// callback fires.
func TestUploadChunksRetriesTransientFailures(t *testing.T) {
	peer := &scriptedPeer{uploadFailures: 2}
	orchestrator, _, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-upload"
	seedMigration(t, orchestrator, id, "wl-none", model.MigrationTransfer)
	refs := []model.ChunkRef{{StoredSize: 100}, {StoredSize: 200}}
	transferred, err := orchestrator.uploadChunks(context.Background(), id, peer, refs, 0.3, 0.4, "transferring")
	if err != nil {
		t.Fatal(err)
	}
	if transferred != 300 {
		t.Fatalf("transferred = %d, want 300", transferred)
	}
	if calls := atomic.LoadInt32(&peer.uploadCalls); calls < 4 {
		t.Fatalf("upload calls = %d, want at least 4 (2 chunks + 2 retries)", calls)
	}
	migration, err := orchestrator.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(migration.Events) == 1 {
		t.Fatal("upload progress did not append events")
	}
}

// TestUploadChunksPermanentFailureFailsFast: a chunk that keeps failing
// fails the whole call promptly rather than hanging or reporting success.
func TestUploadChunksPermanentFailureFailsFast(t *testing.T) {
	peer := &scriptedPeer{uploadFailures: 1 << 20}
	orchestrator, _, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-upload-fail"
	seedMigration(t, orchestrator, id, "wl-none", model.MigrationTransfer)
	done := make(chan error, 1)
	go func() {
		_, err := orchestrator.uploadChunks(context.Background(), id, peer, []model.ChunkRef{{StoredSize: 1}}, 0, 0, "transferring")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("permanent upload failure reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("uploadChunks did not fail fast")
	}
	// An empty chunk set is a clean no-op.
	transferred, err := orchestrator.uploadChunks(context.Background(), id, peer, nil, 0, 0, "noop")
	if err != nil || transferred != 0 {
		t.Fatalf("empty upload = (%d, %v), want (0, nil)", transferred, err)
	}
}

// TestHandleFailureRollsBackAndPreservesSource is the state machine's core
// invariant: a pre-commit failure rolls the destination back and leaves the
// migration in ROLLED_BACK with the source preserved — never observable in
// FAILED while rollback can still succeed.
func TestHandleFailureRollsBackAndPreservesSource(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionReserved}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-fail-rollback"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)

	orchestrator.handleFailure(context.Background(), id, peer, true, false, true, "DESTINATION_VERIFY_FAILED", errors.New("verify broke"), 2*time.Second)

	migration, err := orchestrator.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if migration.Stage != model.MigrationRolledBack {
		t.Fatalf("stage = %s, want ROLLED_BACK (%+v)", migration.Stage, migration)
	}
	if migration.FailureCode != "DESTINATION_VERIFY_FAILED" {
		t.Fatalf("failure code = %q", migration.FailureCode)
	}
	if !migration.SourcePreserved {
		t.Fatal("source was not preserved")
	}
	if atomic.LoadInt32(&peer.rollbackCalls) != 1 {
		t.Fatalf("rollback calls = %d, want 1", peer.rollbackCalls)
	}
	if migration.CompletedAt == nil {
		t.Fatal("rolled-back migration must be completed and terminal")
	}
}

// TestHandleFailureRetriesTransientRollbackAndResumesSource: a transient
// rollback failure — the report's "one-second network blip" — is retried with
// backoff until it succeeds, after which the frozen source is thawed and the
// migration settles ROLLED_BACK. A blip must never escalate into permanent
// operator intervention.
func TestHandleFailureRetriesTransientRollbackAndResumesSource(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionReserved, rollbackFailures: 1}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-fail-retry-rollback"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)
	pauseWorkload(t, orchestrator, workload.Spec.ID)

	orchestrator.handleFailure(context.Background(), id, peer, true, false, true, "DESTINATION_VERIFY_FAILED", errors.New("verify broke"), 2*time.Second)

	migration, _ := orchestrator.Get(id)
	if migration.Stage != model.MigrationRolledBack {
		t.Fatalf("stage = %s, want ROLLED_BACK (%+v)", migration.Stage, migration)
	}
	if calls := atomic.LoadInt32(&peer.rollbackCalls); calls < 2 {
		t.Fatalf("rollback calls = %d, want at least 2 (one retry)", calls)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadRunning {
		t.Fatalf("source workload left %s after a confirmed rollback; it must be thawed", status)
	}
}

// TestHandleFailureRollbackFailureKeepsRecordRecoverable: when the rollback
// cannot be confirmed even after retries and the destination may hold a
// restored copy, the source must NOT be resumed (no second live copy) and the
// record must NOT settle in terminal FAILED — it stays ROLLING_BACK, loudly,
// so recovery re-attempts the rollback and the thaw on every restart. Once
// the destination answers again, the same recovery path completes the
// rollback and thaws the source.
func TestHandleFailureRollbackFailureKeepsRecordRecoverable(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionRestored, rollbackErr: errors.New("destination unreachable")}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-fail-stuck"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)
	pauseWorkload(t, orchestrator, workload.Spec.ID)

	orchestrator.handleFailure(context.Background(), id, peer, true, false, true, "DESTINATION_VERIFY_FAILED", errors.New("verify broke"), 0)

	migration, _ := orchestrator.Get(id)
	if terminal(migration.Stage) {
		t.Fatalf("rollback-incomplete migration settled terminal in %s; it must stay recoverable (%+v)", migration.Stage, migration)
	}
	if migration.Stage != model.MigrationRollingBack {
		t.Fatalf("stage = %s, want ROLLING_BACK (%+v)", migration.Stage, migration)
	}
	if migration.CompletedAt != nil {
		t.Fatal("a recoverable rollback must not carry a completion timestamp")
	}
	if !strings.Contains(migration.FailureReason, "rollback incomplete") {
		t.Fatalf("failure reason %q must name the incomplete rollback loudly", migration.FailureReason)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadPaused {
		t.Fatalf("source workload was thaws to %s while the destination may still run a restored copy", status)
	}
	if migration.SourcePreserved != true {
		t.Fatal("source must stay preserved even when rollback needs an operator")
	}

	// The destination comes back: recovery re-drives the record, completes
	// the rollback, and thaws the source.
	peer.rollbackErr = nil
	orchestrator.Recover(context.Background())
	final := waitForStage(t, orchestrator, id, func(m model.Migration) bool { return terminal(m.Stage) })
	if final.Stage != model.MigrationRolledBack {
		t.Fatalf("recovered rollback ended %s, want ROLLED_BACK (%+v)", final.Stage, final)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadRunning {
		t.Fatalf("source workload left %s after recovery confirmed the rollback", status)
	}
}

// TestHandleFailureResumesSourceWhenDestinationHoldsNoCopy: when the rollback
// is unconfirmed but the destination provably holds no restored workload, the
// source is thawed anyway — the invariant is "a failed move never strands the
// source" — and the unconfirmed cleanup is named loudly on the settled record.
func TestHandleFailureResumesSourceWhenDestinationHoldsNoCopy(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionReserved, rollbackErr: errors.New("destination unreachable")}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-fail-safe-thaw"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)
	pauseWorkload(t, orchestrator, workload.Spec.ID)

	orchestrator.handleFailure(context.Background(), id, peer, true, false, true, "CHUNK_TRANSFER_FAILED", errors.New("chunks gone"), 0)

	migration, _ := orchestrator.Get(id)
	if migration.Stage != model.MigrationRolledBack {
		t.Fatalf("stage = %s, want ROLLED_BACK (%+v)", migration.Stage, migration)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadRunning {
		t.Fatalf("source workload left %s; nothing runs on the destination, so it must be thawed", status)
	}
	if !strings.Contains(migration.FailureReason, "rollback unconfirmed") {
		t.Fatalf("failure reason %q must name the unconfirmed rollback loudly", migration.FailureReason)
	}
}

// TestRecoverLiveMigrationFrozenBeforeTransferResumesSource is the HIGH-2
// regression: a live migration freezes the workload inside its final
// checkpoint while the record is still in SNAPSHOT/PREPARE — before TRANSFER
// is persisted. A crash in that window must not strand the frozen source:
// recovery consults the persisted freeze ownership and thaws the workload.
func TestRecoverLiveMigrationFrozenBeforeTransferResumesSource(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionReserved}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-live-frozen"
	seedMigrationRecord(t, orchestrator, id, workload.Spec.ID, model.MigrationPrepare, model.MigrationLive)
	orchestrator.noteFreezeOwnership(id, workload.Spec.ID, true)
	orchestrator.noteDowntimeStart(id, time.Now().Add(-5*time.Minute))
	pauseWorkload(t, orchestrator, workload.Spec.ID)

	orchestrator.Recover(context.Background())

	final := waitForStage(t, orchestrator, id, func(m model.Migration) bool { return terminal(m.Stage) })
	if final.Stage != model.MigrationRolledBack {
		t.Fatalf("recovered migration ended %s, want ROLLED_BACK (%+v)", final.Stage, final)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadRunning {
		t.Fatalf("source workload left %s; a live migration that froze it must resume it on recovery", status)
	}
	if final.Metrics.Downtime <= 0 {
		t.Fatalf("recovery reported downtime %s; the frozen outage must be measured from the persisted bound", final.Metrics.Downtime)
	}
}

// TestRecoverLeavesOperatorPausedWorkloadStopped: freeze ownership is the
// workload's pre-migration status. A workload the operator had paused is
// never resumed by crash recovery — the run path and recovery must agree.
func TestRecoverLeavesOperatorPausedWorkloadStopped(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionReserved}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-recover-paused"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)
	orchestrator.noteFreezeOwnership(id, workload.Spec.ID, false)
	pauseWorkload(t, orchestrator, workload.Spec.ID)

	orchestrator.Recover(context.Background())

	final := waitForStage(t, orchestrator, id, func(m model.Migration) bool { return terminal(m.Stage) })
	if final.Stage != model.MigrationRolledBack {
		t.Fatalf("recovered migration ended %s, want ROLLED_BACK (%+v)", final.Stage, final)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadPaused {
		t.Fatalf("operator-paused workload was changed to %s by recovery; it must stay paused", status)
	}
}

// TestRecoverClientFailureDoesNotResumeSource: a destination client that
// cannot be constructed says nothing about destination state. Recovery must
// treat it like a failed probe — assume state exists, attempt rollback, and
// never resume the source on an unconfirmed rollback.
func TestRecoverClientFailureDoesNotResumeSource(t *testing.T) {
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) {
		return nil, errors.New("tls material unreadable")
	})
	id := "mig-recover-noclient"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)
	orchestrator.noteFreezeOwnership(id, workload.Spec.ID, true)
	pauseWorkload(t, orchestrator, workload.Spec.ID)

	orchestrator.Recover(context.Background())

	// The record must not settle while the destination state is unknown.
	time.Sleep(2 * time.Second)
	migration, _ := orchestrator.Get(id)
	if terminal(migration.Stage) {
		t.Fatalf("migration settled %s although destination state was never rolled back (%+v)", migration.Stage, migration)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadPaused {
		t.Fatalf("source workload was changed to %s with the destination unprobed; it must stay frozen", status)
	}
}

// workloadStatus reads the fixture workload's current runtime status.
func workloadStatus(t *testing.T, orchestrator *Orchestrator, workloadID string) model.WorkloadStatus {
	t.Helper()
	workload, err := orchestrator.runtime.Get(workloadID)
	if err != nil {
		t.Fatal(err)
	}
	return workload.Status
}

// TestHandleFailureAfterCommitIsSourceCleanupFailure: once the destination
// committed, the migration is a success whose source cleanup is the only
// outstanding problem — it must not be rolled back or counted as failed.
func TestHandleFailureAfterCommitIsSourceCleanupFailure(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-fail-committed"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationCommit)

	orchestrator.handleFailure(context.Background(), id, peer, true, true, true, "SOURCE_CLEANUP_FAILED", errors.New("source stop failed"), 1*time.Second)

	migration, _ := orchestrator.Get(id)
	if atomic.LoadInt32(&peer.rollbackCalls) != 0 {
		t.Fatal("committed migration was rolled back")
	}
	if migration.FailureCode != "SOURCE_CLEANUP_FAILED" {
		t.Fatalf("failure code = %q", migration.FailureCode)
	}
}

// TestHandleFailureIgnoresTerminalMigrations: a settled migration is never
// re-processed — its root cause must survive later activity.
func TestHandleFailureIgnoresTerminalMigrations(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-fail-terminal"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationRolledBack)
	_ = orchestrator.update(id, func(value *model.Migration) error {
		value.FailureCode = "ORIGINAL_CAUSE"
		return nil
	})

	orchestrator.handleFailure(context.Background(), id, peer, true, false, true, "AGENT_RESTARTED", errors.New("agent restarted"), 0)

	migration, _ := orchestrator.Get(id)
	if migration.FailureCode != "ORIGINAL_CAUSE" {
		t.Fatalf("terminal migration's root cause was overwritten: %q", migration.FailureCode)
	}
}

// TestHandleCancellationCancelsAndPreservesSource: cancellation is not
// failure — the stage is CANCELLED, the source survives, and the destination
// is rolled back if a session was opened.
func TestHandleCancellationCancelsAndPreservesSource(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-cancel"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)

	orchestrator.handleCancellation(context.Background(), id, peer, true, false, true, context.Canceled, 0)

	migration, _ := orchestrator.Get(id)
	if migration.Stage != model.MigrationCancelled {
		t.Fatalf("stage = %s, want CANCELLED", migration.Stage)
	}
	if migration.FailureCode != "MIGRATION_CANCELLED" {
		t.Fatalf("failure code = %q", migration.FailureCode)
	}
	if !migration.SourcePreserved {
		t.Fatal("cancellation must preserve the source workload")
	}
	if atomic.LoadInt32(&peer.rollbackCalls) != 1 {
		t.Fatalf("rollback calls = %d, want 1", peer.rollbackCalls)
	}
}

// TestHandleCancellationCommittedKeepsDestination: a committed destination
// cannot be safely undone by cancellation — the migration settles COMPLETED
// through the run path's source-cleanup bookkeeping, the source is stopped,
// and no cancellation failure code rides on the completed record.
func TestHandleCancellationCommittedKeepsDestination(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-cancel-committed"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationCleanup)

	orchestrator.handleCancellation(context.Background(), id, peer, true, true, true, context.Canceled, 0)

	if atomic.LoadInt32(&peer.rollbackCalls) != 0 {
		t.Fatal("cancellation rolled back a committed destination")
	}
	migration, _ := orchestrator.Get(id)
	if migration.Stage != model.MigrationCompleted {
		t.Fatalf("stage = %s, want COMPLETED (%+v)", migration.Stage, migration)
	}
	if migration.FailureCode != "" {
		t.Fatalf("failure code %q must not ride on a completed record", migration.FailureCode)
	}
	if status := workloadStatus(t, orchestrator, workload.Spec.ID); status != model.WorkloadStopped {
		t.Fatalf("committed migration left the source %s; the committed destination is authoritative and the source is stopped", status)
	}
}

// TestCreateIdempotentReplayAfterMachineIDResolution: run() fills the record's
// Destination.MachineID with the resolved peer identity, so a retry of a
// request that left machine_id empty must still match the stored record —
// the documented "coordinator can safely retry after a lost response"
// contract. A request that pinned a different machine id is still a
// conflict.
func TestCreateIdempotentReplayAfterMachineIDResolution(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	now := time.Now().UTC()
	if err := orchestrator.records.Put("mig-replay", model.Migration{
		ID: "mig-replay", IdempotencyKey: "retry-key", WorkloadID: workload.Spec.ID,
		SourceMachineID: "src-machine",
		Destination:     model.Destination{AgentURL: "https://peer.example:8443", ServerName: "peer.example", MachineID: "dst-resolved"},
		Mode:            model.MigrationCold, Stage: model.MigrationCompleted,
		CreatedAt: now, UpdatedAt: now, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}

	replayed, err := orchestrator.Create(context.Background(), CreateRequest{
		WorkloadID:     workload.Spec.ID,
		IdempotencyKey: "retry-key",
		Destination:    model.Destination{AgentURL: "https://peer.example:8443"},
		Mode:           model.MigrationCold,
	})
	if err != nil {
		t.Fatalf("idempotent replay after machine-id resolution was rejected: %v", err)
	}
	if replayed.ID != "mig-replay" {
		t.Fatalf("replay returned %s, want the original record", replayed.ID)
	}

	_, err = orchestrator.Create(context.Background(), CreateRequest{
		WorkloadID:     workload.Spec.ID,
		IdempotencyKey: "retry-key",
		Destination:    model.Destination{AgentURL: "https://peer.example:8443", MachineID: "someone-else"},
		Mode:           model.MigrationCold,
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("a pinned machine id that disagrees with the resolved one must conflict, got %v", err)
	}
}

// TestRunColdMigrationCompletes drives the whole run() state machine with a
// scripted peer and a real (CRIU-less) checkpoint engine: the migration must
// traverse discover→…→completed, move the missing chunks, and record its
// metrics.
func TestRunColdMigrationCompletes(t *testing.T) {
	peer := &scriptedPeer{sessionState: transfer.SessionRestored, manifestMissing: []model.ChunkRef{{StoredSize: 64}}}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	peer.profile = destinationLike(takeInventory(t, orchestrator), "dst-machine-run")

	migration, err := orchestrator.Create(context.Background(), CreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{AgentURL: "https://peer.example:8443", MachineID: "dst-machine-run"},
		Mode:        model.MigrationCold,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForStage(t, orchestrator, migration.ID, func(m model.Migration) bool {
		return m.Stage == model.MigrationCompleted || m.Stage == model.MigrationFailed || m.Stage == model.MigrationRolledBack
	})
	if final.Stage != model.MigrationCompleted {
		t.Fatalf("cold migration ended %s: code=%s reason=%s", final.Stage, final.FailureCode, final.FailureReason)
	}
	if atomic.LoadInt32(&peer.commitCalls) != 1 {
		t.Fatalf("commit calls = %d, want 1", peer.commitCalls)
	}
	if final.Metrics.TransferredBytes == 0 {
		t.Fatal("completed migration recorded no transferred bytes")
	}
}

// TestRunDestinationIncompatibleFailsWithRollback: the compatibility gate is
// enforced inside run(), and a rejection follows the rollback path with the
// checker's code.
func TestRunDestinationIncompatibleFailsWithRollback(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	peer.profile = destinationLike(takeInventory(t, orchestrator), "dst-machine-bad")
	peer.profile.Architecture = "arm64" // deterministic ARCH_MISMATCH against this amd64 host

	migration, err := orchestrator.Create(context.Background(), CreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{AgentURL: "https://peer.example:8443", MachineID: "dst-machine-bad"},
		Mode:        model.MigrationCold,
	})
	if err != nil {
		t.Fatal(err)
	}
	final := waitForStage(t, orchestrator, migration.ID, func(m model.Migration) bool {
		return m.Stage == model.MigrationRolledBack || m.Stage == model.MigrationFailed || m.Stage == model.MigrationCompleted
	})
	if final.Stage == model.MigrationCompleted {
		t.Fatal("incompatible destination completed a migration")
	}
	if final.FailureCode != "DESTINATION_INCOMPATIBLE" {
		t.Fatalf("failure code = %q, want DESTINATION_INCOMPATIBLE", final.FailureCode)
	}
	if !final.SourcePreserved {
		t.Fatal("rejected migration must preserve the source")
	}
}

// TestCancelActivelyRunningMigration: Cancel cancels the run context, the
// blocked transfer fails with context.Canceled, and the state machine routes
// that through handleCancellation to a clean CANCELLED outcome.
func TestCancelActivelyRunningMigration(t *testing.T) {
	scripted := &scriptedPeer{sessionState: transfer.SessionReserved}
	waiting := &waitingPeer{scriptedPeer: scripted, reached: make(chan struct{})}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return waiting, nil })
	scripted.profile = destinationLike(takeInventory(t, orchestrator), "dst-machine-cancel")

	migration, err := orchestrator.Create(context.Background(), CreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{AgentURL: "https://peer.example:8443", MachineID: "dst-machine-cancel"},
		Mode:        model.MigrationCold,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting.reached:
	case <-time.After(60 * time.Second):
		t.Fatal("run never reached the transfer stage")
	}
	if err := orchestrator.Cancel(migration.ID); err != nil {
		t.Fatal(err)
	}
	final := waitForStage(t, orchestrator, migration.ID, func(m model.Migration) bool {
		return m.Stage == model.MigrationCancelled || m.Stage == model.MigrationFailed || m.Stage == model.MigrationRolledBack
	})
	if final.Stage != model.MigrationCancelled {
		t.Fatalf("cancelled migration ended %s: code=%s reason=%s", final.Stage, final.FailureCode, final.FailureReason)
	}
	if !final.SourcePreserved {
		t.Fatal("cancelled migration must preserve the source")
	}
}

// TestRecoverSettledFailuresIsTerminal: after a restart, Recover must not
// reprocess a migration that already settled in FAILED — its root cause is
// the record.
func TestRecoverSettledFailuresIsTerminal(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-recover-settled"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationFailed)
	_ = orchestrator.update(id, func(value *model.Migration) error {
		value.FailureCode = "ORIGINAL_CAUSE"
		value.FailureReason = "the original root cause"
		return nil
	})

	orchestrator.Recover(context.Background())
	time.Sleep(2 * time.Second)

	migration, _ := orchestrator.Get(id)
	if migration.FailureCode != "ORIGINAL_CAUSE" {
		t.Fatalf("Recover overwrote a settled failure: code=%q reason=%q", migration.FailureCode, migration.FailureReason)
	}
	if atomic.LoadInt32(&peer.rollbackCalls) != 0 {
		t.Fatal("Recover rolled back a settled failure")
	}
}

// TestRecoverInterruptedMigrationsRollsBack: a migration left mid-flight by
// an agent restart is rolled back and preserved — AGENT_RESTARTED is its
// honest failure code.
func TestRecoverInterruptedMigrationsRollsBack(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	id := "mig-recover-interrupted"
	seedMigration(t, orchestrator, id, workload.Spec.ID, model.MigrationTransfer)

	orchestrator.Recover(context.Background())

	final := waitForStage(t, orchestrator, id, func(m model.Migration) bool {
		return terminal(m.Stage) || m.Stage == model.MigrationFailed
	})
	if final.Stage != model.MigrationRolledBack {
		t.Fatalf("recovered migration ended %s, want ROLLED_BACK", final.Stage)
	}
	if final.FailureCode != "AGENT_RESTARTED" {
		t.Fatalf("failure code = %q, want AGENT_RESTARTED", final.FailureCode)
	}
	if !final.SourcePreserved {
		t.Fatal("recovery must preserve the source")
	}
}

// TestRecoverIgnoresSettledStages: completed and cancelled migrations are
// history, not work.
func TestRecoverIgnoresSettledStages(t *testing.T) {
	peer := &scriptedPeer{}
	orchestrator, workload, _ := preflightOrchestrator(t, func(model.Destination) (PeerClient, error) { return peer, nil })
	for _, stage := range []model.MigrationStage{model.MigrationCompleted, model.MigrationRolledBack, model.MigrationCancelled} {
		id := "mig-recover-" + string(stage)
		seedMigration(t, orchestrator, id, workload.Spec.ID, stage)
	}
	orchestrator.Recover(context.Background())
	time.Sleep(2 * time.Second)
	if atomic.LoadInt32(&peer.rollbackCalls) != 0 {
		t.Fatal("Recover touched a settled migration")
	}
}

// takeInventory returns the machine capabilities the compatibility check saw
// in preflight tests' fixtures — the source profile destinationLike mirrors.
func takeInventory(t *testing.T, orchestrator *Orchestrator) model.MachineCapabilities {
	t.Helper()
	profile, err := orchestrator.inventory.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
