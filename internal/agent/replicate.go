package agent

// replicate.go is the sending half of warm-standby failover. Each pass
// replicates the newest root checkpoint of every workload carrying a
// failover policy to its standby: reserve, key, manifest, the chunks the
// standby is missing, verify, then hold — the same peer protocol a
// migration speaks, ending in the standby's held state instead of a
// restore. Incremental checkpoints (those with a parent) are deliberately
// not replicated: a delta is unrestorable without its ancestors, while each
// periodic root stands alone, so the standby's newest root is at most one
// checkpoint interval behind the source. Every push runs on its own
// goroutine under a bounded per-peer budget: the checkpoint scheduler that
// drives these passes must never wait on a standby's network I/O, and no
// call may fall through to the peer client's day-long no-deadline fallback.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/transfer"
)

// replicationRetryDelay backs off a failing push so a dead standby is
// retried roughly every half minute, not every scheduler tick.
const replicationRetryDelay = 30 * time.Second

// replicationSessionTTL bounds a replication session on the standby: long
// enough for a large push, short enough that an abandoned one stops
// counting against the standby's reservation bookkeeping.
const replicationSessionTTL = 30 * time.Minute

const (
	// replicationPushTimeout bounds one push to one standby. It sits just
	// under the session TTL the push reserves: past that the standby reaps
	// the session anyway, and without a budget every call inside the push
	// falls through to the peer client's 24-hour no-deadline fallback — one
	// black-holed standby would pin its goroutine for a day.
	replicationPushTimeout = 25 * time.Minute
	// replicationCallTimeout bounds one control call inside a push: the
	// identity probe, reserve, key and manifest imports, verify, hold, and
	// the post-hold confirmation.
	replicationCallTimeout = 2 * time.Minute
	// replicationChunkTimeout bounds one chunk upload attempt; retries ride
	// the same budget per attempt and the push budget over all.
	replicationChunkTimeout = 10 * time.Minute
)

// failoverReplicator drives replication from the policy loop. It owns the
// ledger; everything else it borrows from the service.
type failoverReplicator struct {
	ledger      *securestore.EncryptedCollection[model.ReplicationEntry]
	runtime     *shiftruntime.Manager
	checkpoints *checkpoint.Service
	keys        *securestore.Manager
	chunks      *chunkstore.Store
	identity    *identity.Identity
	peerClient  func(endpoint string) (*transfer.Client, error)
	// advertisedURL is this agent's dialable peer listener — the URL a
	// standby should probe before believing this machine dead. It comes
	// from the control-plane agent_url setting; an agent that advertises
	// nothing sends nothing, and its standby can never confirm its death
	// automatically. That limitation is the operator's to see and accept.
	advertisedURL     string
	migrationInFlight func(workloadID string) bool
	logger            *slog.Logger
	mu                sync.Mutex
	// passing is the single-flight latch around one replication pass: the
	// scheduler may tick while a previous push is still in flight, and the
	// answer is to skip, never to queue a second pass behind a hung one.
	passing atomic.Bool
	// pushTimeout overrides replicationPushTimeout in tests.
	pushTimeout time.Duration
	// guard is the agent's shared write guard (Service.gcGuard). A push
	// holds it shared while it reads chunks, so an online garbage-collection
	// sweep cannot delete state out from under a long replication read.
	// Optional: a replicator without one behaves as before.
	guard *gcWriteGuard
}

// SetWriteGuard attaches the agent's shared checkpoint/transfer write guard.
func (r *failoverReplicator) SetWriteGuard(guard *gcWriteGuard) {
	r.guard = guard
}

// pushBudget is the wall-clock budget for one peer push.
func (r *failoverReplicator) pushBudget() time.Duration {
	if r.pushTimeout > 0 {
		return r.pushTimeout
	}
	return replicationPushTimeout
}

// Entries lists the replication ledger, oldest update first.
func (r *failoverReplicator) Entries() []model.ReplicationEntry {
	entries := r.ledger.List()
	sort.Slice(entries, func(i, j int) bool { return entries[i].UpdatedAt.Before(entries[j].UpdatedAt) })
	return entries
}

// apply schedules one replication pass on its own goroutine and returns
// immediately. The checkpoint-policy scheduler calls it from its ticker, and
// a hung or slow standby must never delay periodic checkpoints for any
// workload on the machine — the pass runs behind a single-flight latch and
// every peer call it makes carries a deadline.
func (r *failoverReplicator) apply(ctx context.Context) {
	if !r.passing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer r.passing.Store(false)
		r.pass(ctx)
	}()
}

// pass runs one replication pass synchronously: first withdrawals, then due
// pushes. Both halves reconcile the ledger against the workload specs — the
// spec is the operator's intent, the ledger is what has actually been
// pushed, and the two must not drift apart silently.
func (r *failoverReplicator) pass(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconcileWithdrawals(ctx)
	r.replicateDue(ctx)
}

// reconcileWithdrawals releases duties the source no longer owes: the
// workload is gone, its failover policy was removed, or it now names a
// different standby. A withdrawal failure keeps the entry so the next pass
// retries — overwriting it with a push to the new standby would strand the
// old standby's duty with no one left to withdraw it.
func (r *failoverReplicator) reconcileWithdrawals(ctx context.Context) {
	for _, entry := range r.ledger.List() {
		workload, err := r.runtime.Get(entry.WorkloadID)
		policy := workload.Spec.FailoverPolicy
		stale := errors.Is(err, persistence.ErrNotFound) || err == nil && (policy == nil || policy.AgentURL != entry.StandbyURL)
		if !stale {
			continue
		}
		if entry.LastError != "" && !time.Now().UTC().After(entry.LastErrorAt.Add(replicationRetryDelay)) {
			continue
		}
		if err := r.withdraw(ctx, entry); err != nil {
			r.recordError(entry, fmt.Errorf("withdraw from %s: %w", entry.StandbyURL, err))
			continue
		}
		if err := r.ledger.Delete(entry.WorkloadID); err != nil {
			r.logger.Error("replication ledger delete failed", "workload_id", entry.WorkloadID, "error", err)
		}
	}
}

// withdraw tells one standby the source no longer owes it a duty.
func (r *failoverReplicator) withdraw(ctx context.Context, entry model.ReplicationEntry) error {
	peer, err := r.peerClient(entry.StandbyURL)
	if err != nil {
		return err
	}
	withdrawCtx, cancel := context.WithTimeout(ctx, standbyProbeTimeout)
	defer cancel()
	return peer.Withdraw(withdrawCtx, entry.WorkloadID)
}

// replicateDue pushes the newest root checkpoint of every workload with a
// failover policy that has not been pushed yet. A failed push is recorded
// on the ledger and backed off; the workload keeps running and its local
// checkpoints keep coming regardless — replication lag never blocks the
// source's own protection.
func (r *failoverReplicator) replicateDue(ctx context.Context) {
	for _, workload := range r.runtime.List() {
		policy := workload.Spec.FailoverPolicy
		if policy == nil {
			continue
		}
		if r.migrationInFlight(workload.Spec.ID) {
			continue
		}
		root, ok := newestRootCheckpoint(r.checkpoints.List(workload.Spec.ID))
		if !ok {
			continue
		}
		entry, err := r.ledger.Get(workload.Spec.ID)
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			r.logger.Error("replication ledger lookup failed", "workload_id", workload.Spec.ID, "error", err)
			continue
		}
		if err == nil {
			if entry.StandbyURL == policy.AgentURL && entry.LastCheckpointID == root.ID {
				continue
			}
			// A standby change leaves the old standby holding a duty only
			// this machine can withdraw; reconcileWithdrawals owns that,
			// and until it succeeds the push to the new standby waits.
			if entry.StandbyURL != policy.AgentURL {
				continue
			}
			if entry.LastError != "" && !time.Now().UTC().After(entry.LastErrorAt.Add(replicationRetryDelay)) {
				continue
			}
		}
		pushCtx, cancel := context.WithTimeout(ctx, r.pushBudget())
		err = r.replicate(pushCtx, workload.Spec, policy, root)
		cancel()
		if err != nil {
			r.logger.Warn("failover replication failed",
				"workload_id", workload.Spec.ID, "workload_name", workload.Spec.Name,
				"checkpoint_id", root.ID, "standby", policy.AgentURL, "error", err)
			entry.WorkloadID = workload.Spec.ID
			entry.WorkloadName = workload.Spec.Name
			entry.StandbyURL = policy.AgentURL
			r.recordError(entry, err)
			continue
		}
		r.logger.Info("checkpoint replicated to standby",
			"workload_id", workload.Spec.ID, "workload_name", workload.Spec.Name,
			"checkpoint_id", root.ID, "standby", policy.AgentURL, "bytes", root.StoredBytes)
	}
}

// recordError attributes a failed push (or withdrawal) to the ledger so
// `failover status` shows it. The entry is written on failure too — a first
// push that never succeeded must not be invisible.
func (r *failoverReplicator) recordError(entry model.ReplicationEntry, cause error) {
	if entry.WorkloadID == "" {
		return
	}
	entry.LastError = cause.Error()
	entry.LastErrorAt = time.Now().UTC()
	entry.UpdatedAt = entry.LastErrorAt
	if err := r.ledger.Put(entry.WorkloadID, entry); err != nil {
		r.logger.Error("replication ledger persist failed", "workload_id", entry.WorkloadID, "error", err)
	}
}

// replicate pushes one root checkpoint to the standby through the same peer
// protocol a migration speaks. Each attempt opens a fresh session: an
// attempt interrupted by a crash or a network cut simply re-pushes — the
// manifest import overwrites the same record, and the chunks the standby
// already holds are negotiated away, so a retry costs only what is missing.
func (r *failoverReplicator) replicate(ctx context.Context, spec model.WorkloadSpec, policy *model.FailoverPolicySpec, root checkpoint.Summary) (err error) {
	if r.guard != nil {
		r.guard.mu.RLock()
		defer r.guard.mu.RUnlock()
	}
	peer, err := r.peerClient(policy.AgentURL)
	if err != nil {
		return err
	}
	manifest, err := r.checkpoints.Load(root.ID)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}
	// The identity pin runs before anything leaves: a mistyped URL must
	// fail here, not deposit a workload's encrypted state on the wrong
	// host — the key would follow the chunks on the next line.
	machine, err := peerCall(ctx, func(callCtx context.Context) (model.MachineCapabilities, error) {
		return peer.Machine(callCtx)
	})
	if err != nil {
		return fmt.Errorf("reach standby: %w", err)
	}
	if machine.MachineID == r.identity.Machine.ID {
		return errors.New("the failover standby is this machine")
	}
	if policy.MachineID != "" && machine.MachineID != policy.MachineID {
		return fmt.Errorf("standby identity mismatch: policy pins %s, standby answers as %s", policy.MachineID, machine.MachineID)
	}
	sessionID, err := model.NewID()
	if err != nil {
		return err
	}
	if _, err := peerCall(ctx, func(callCtx context.Context) (transfer.Session, error) {
		return peer.Reserve(callCtx, transfer.ReserveRequest{
			ID: sessionID, SourceMachineID: r.identity.Machine.ID, WorkloadID: spec.ID,
			EstimatedBytes: manifest.Metrics.StoredBytes,
			ExpiresAt:      time.Now().Add(replicationSessionTTL),
			Purpose:        transfer.PurposeReplication,
		})
	}); err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	dataKey, err := r.keys.ExportWorkloadKey(spec.ID, manifest.Security.KeyVersion)
	if err != nil {
		return fmt.Errorf("export workload key: %w", err)
	}
	if err := peerCallVoid(ctx, func(callCtx context.Context) error {
		return peer.ImportKey(callCtx, sessionID, spec.ID, manifest.Security.KeyVersion, dataKey)
	}); err != nil {
		return fmt.Errorf("import key: %w", err)
	}
	missing, err := peerCall(ctx, func(callCtx context.Context) (transfer.MissingResponse, error) {
		return peer.ImportManifest(callCtx, sessionID, manifest)
	})
	if err != nil {
		return fmt.Errorf("import manifest: %w", err)
	}
	if err := r.uploadChunks(ctx, peer, sessionID, missing.Missing); err != nil {
		return fmt.Errorf("upload chunks: %w", err)
	}
	if _, err := peerCall(ctx, func(callCtx context.Context) (transfer.Session, error) {
		return peer.Verify(callCtx, sessionID)
	}); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if _, err := peerCall(ctx, func(callCtx context.Context) (transfer.Session, error) {
		return peer.HoldWithFencing(callCtx, sessionID, policy.KeepLast, r.advertisedURL, policy.FencingProvider)
	}); err != nil {
		return fmt.Errorf("hold: %w", err)
	}
	// The hold's answer is the standby's in-flight copy; re-read the session
	// it actually stored. A standby that could not persist its duty has
	// degraded the hold — the ledger must record that failure and retry
	// rather than claim a protection that does not exist.
	if err := confirmHold(ctx, peer, sessionID); err != nil {
		return err
	}
	// The ledger is written after the hold, not before: a crash between the
	// two re-pushes the same checkpoint next pass, and the standby's
	// negotiation makes that cheap. Writing it earlier would risk claiming
	// a push that never landed.
	now := time.Now().UTC()
	return r.ledger.Put(spec.ID, model.ReplicationEntry{
		WorkloadID: spec.ID, WorkloadName: spec.Name,
		StandbyURL: policy.AgentURL, StandbyMachineID: machine.MachineID,
		KeepLast: policy.KeepLast, LastCheckpointID: root.ID, LastPushAt: now, UpdatedAt: now,
	})
}

// peerCall runs one peer round trip under the control-call deadline, nested
// inside the push budget the caller already carries. Without a per-call
// deadline the peer client falls back to a 24-hour timeout for callers
// without one — the hang this file must never reproduce.
func peerCall[T any](ctx context.Context, call func(context.Context) (T, error)) (T, error) {
	callCtx, cancel := context.WithTimeout(ctx, replicationCallTimeout)
	defer cancel()
	return call(callCtx)
}

// peerCallVoid is peerCall for round trips with no response body of note.
func peerCallVoid(ctx context.Context, call func(context.Context) error) error {
	callCtx, cancel := context.WithTimeout(ctx, replicationCallTimeout)
	defer cancel()
	return call(callCtx)
}

// confirmHold re-reads the session the hold produced. The hold response is
// the standby's in-flight copy, taken before its duty record landed; the
// stored session is where a duty-persist failure surfaces as a degraded
// state. Anything but HELD means the push did not take — the source's
// ledger records it and the next pass retries.
func confirmHold(ctx context.Context, peer *transfer.Client, sessionID string) error {
	session, err := peerCall(ctx, func(callCtx context.Context) (transfer.Session, error) {
		return peer.Get(callCtx, sessionID)
	})
	if err != nil {
		return fmt.Errorf("confirm hold: %w", err)
	}
	if session.State != transfer.SessionHeld {
		return fmt.Errorf("standby did not keep the hold: session is %s", session.State)
	}
	return nil
}

// uploadChunks pushes the chunks the standby is missing, four at a time
// with per-chunk retries — the same shape the migration orchestrator uses,
// without its progress accounting (the ledger records the push, not each
// chunk).
func (r *failoverReplicator) uploadChunks(ctx context.Context, peer *transfer.Client, sessionID string, refs []model.ChunkRef) error {
	if len(refs) == 0 {
		return nil
	}
	workerCount := 4
	if len(refs) < workerCount {
		workerCount = len(refs)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan model.ChunkRef)
	type result struct {
		err error
	}
	results := make(chan result, workerCount)
	var workers sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for ref := range jobs {
				var err error
				for attempt := 0; attempt < 3; attempt++ {
					attemptCtx, attemptCancel := context.WithTimeout(ctx, replicationChunkTimeout)
					err = peer.UploadChunk(attemptCtx, sessionID, ref, r.chunks)
					attemptCancel()
					if err == nil || ctx.Err() != nil {
						break
					}
					select {
					case <-ctx.Done():
					case <-time.After(time.Duration(1<<attempt) * 250 * time.Millisecond):
					}
				}
				if err != nil {
					results <- result{err: err}
					return
				}
				select {
				case results <- result{}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, ref := range refs {
			select {
			case jobs <- ref:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	completed := 0
	for res := range results {
		if res.err != nil {
			cancel()
			return res.err
		}
		completed++
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if completed != len(refs) {
		return errors.New("chunk upload stopped before all chunks completed")
	}
	return nil
}

// newestRootCheckpoint picks the newest checkpoint without a parent — the
// newest state that restores on its own. Incrementals are skipped by
// design, not by oversight: their deltas are unrestorable without ancestor
// images this push does not carry.
func newestRootCheckpoint(summaries []checkpoint.Summary) (checkpoint.Summary, bool) {
	for _, summary := range summaries {
		if summary.ParentID == "" {
			return summary, true
		}
	}
	return checkpoint.Summary{}, false
}
