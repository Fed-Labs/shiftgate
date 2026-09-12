package agent

// replicate.go is the sending half of warm-standby failover. Each pass
// replicates the newest root checkpoint of every workload carrying a
// failover policy to its standby: reserve, key, manifest, the chunks the
// standby is missing, verify, then hold — the same peer protocol a
// migration speaks, ending in the standby's held state instead of a
// restore. Incremental checkpoints (those with a parent) are deliberately
// not replicated: a delta is unrestorable without its ancestors, while each
// periodic root stands alone, so the standby's newest root is at most one
// checkpoint interval behind the source.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
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
}

// Entries lists the replication ledger, oldest update first.
func (r *failoverReplicator) Entries() []model.ReplicationEntry {
	entries := r.ledger.List()
	sort.Slice(entries, func(i, j int) bool { return entries[i].UpdatedAt.Before(entries[j].UpdatedAt) })
	return entries
}

// apply is one replication pass: first withdrawals, then due pushes. Both
// halves reconcile the ledger against the workload specs — the spec is the
// operator's intent, the ledger is what has actually been pushed, and the
// two must not drift apart silently.
func (r *failoverReplicator) apply(ctx context.Context) {
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
		if err := r.replicate(ctx, workload.Spec, policy, root); err != nil {
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
func (r *failoverReplicator) replicate(ctx context.Context, spec model.WorkloadSpec, policy *model.FailoverPolicySpec, root checkpoint.Summary) error {
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
	machine, err := peer.Machine(ctx)
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
	if _, err := peer.Reserve(ctx, transfer.ReserveRequest{
		ID: sessionID, SourceMachineID: r.identity.Machine.ID, WorkloadID: spec.ID,
		EstimatedBytes: manifest.Metrics.StoredBytes,
		ExpiresAt:      time.Now().Add(replicationSessionTTL),
		Purpose:        transfer.PurposeReplication,
	}); err != nil {
		return fmt.Errorf("reserve: %w", err)
	}
	dataKey, err := r.keys.ExportWorkloadKey(spec.ID, manifest.Security.KeyVersion)
	if err != nil {
		return fmt.Errorf("export workload key: %w", err)
	}
	if err := peer.ImportKey(ctx, sessionID, spec.ID, manifest.Security.KeyVersion, dataKey); err != nil {
		return fmt.Errorf("import key: %w", err)
	}
	missing, err := peer.ImportManifest(ctx, sessionID, manifest)
	if err != nil {
		return fmt.Errorf("import manifest: %w", err)
	}
	if err := r.uploadChunks(ctx, peer, sessionID, missing.Missing); err != nil {
		return fmt.Errorf("upload chunks: %w", err)
	}
	if _, err := peer.Verify(ctx, sessionID); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if _, err := peer.Hold(ctx, sessionID, policy.KeepLast, r.advertisedURL); err != nil {
		return fmt.Errorf("hold: %w", err)
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
					err = peer.UploadChunk(ctx, sessionID, ref, r.chunks)
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
