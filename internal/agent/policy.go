package agent

import (
	"context"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// checkpointPolicyTick is how often the scheduler reconsiders workload
// policies. An interval is measured from the last checkpoint, so a tick
// coarser than the smallest allowed interval (10 seconds) would silently
// stretch every schedule.
const checkpointPolicyTick = 5 * time.Second

// runCheckpointPolicies is the periodic-checkpoint loop: workloads whose spec
// carries a policy are checkpointed every interval while they run. The
// schedule is derived from persisted facts — the spec and the existing
// checkpoints — never from an in-memory timer, so it survives agent restarts:
// after a restart a workload whose newest checkpoint is already older than
// its interval is protected promptly instead of waiting a fresh interval.
func (s *Service) runCheckpointPolicies(ctx context.Context) {
	ticker := time.NewTicker(checkpointPolicyTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.applyCheckpointPolicies(ctx)
			s.replicator.apply(ctx)
		}
	}
}

// applyCheckpointPolicies runs one scheduling pass. A workload is skipped
// unless it is running, has no migration in flight, and its interval has
// elapsed since the last checkpoint (or since the workload was created, when
// none exists yet).
func (s *Service) applyCheckpointPolicies(ctx context.Context) {
	for _, workload := range s.runtime.List() {
		policy := workload.Spec.CheckpointPolicy
		if policy == nil || policy.IntervalSeconds <= 0 {
			continue
		}
		if workload.Status != model.WorkloadRunning {
			continue
		}
		if s.migrationInFlight(workload.Spec.ID) {
			continue
		}
		if !s.policyCheckpointDue(workload, policy) {
			continue
		}
		interval := time.Duration(policy.IntervalSeconds) * time.Second
		checkpointCtx, cancel := context.WithTimeout(ctx, interval)
		manifest, err := s.checkpoints.Create(checkpointCtx, workload.Spec.ID, checkpoint.CreateOptions{
			LeaveRunning: true,
			Timeout:      interval,
		})
		cancel()
		if err != nil {
			// The next pass retries; a workload that keeps failing is visible
			// through its status and the checkpoint diagnostics, not silenced.
			s.logger.Warn("periodic checkpoint failed",
				"workload_id", workload.Spec.ID, "workload_name", workload.Spec.Name, "error", err)
			continue
		}
		s.logger.Info("periodic checkpoint created",
			"checkpoint_id", manifest.ID, "workload_id", workload.Spec.ID,
			"workload_name", workload.Spec.Name, "plain_bytes", manifest.Metrics.PlainBytes)
		if policy.KeepLast > 0 {
			s.pruneCheckpoints(workload, policy.KeepLast)
		}
	}
}

// policyCheckpointDue reports whether the workload's interval has elapsed.
// The reference is the newest existing checkpoint, or the workload's creation
// when none exists — both persisted facts, so a restart cannot reset the
// schedule.
func (s *Service) policyCheckpointDue(workload model.Workload, policy *model.CheckpointPolicySpec) bool {
	reference := workload.Spec.CreatedAt
	if summaries := s.checkpoints.List(workload.Spec.ID); len(summaries) > 0 {
		if newest := summaries[0].CreatedAt; newest.After(reference) {
			reference = newest
		}
	}
	return !time.Now().UTC().Before(reference.Add(time.Duration(policy.IntervalSeconds) * time.Second))
}

// migrationInFlight reports whether a migration is still working on the
// workload. Checkpointing mid-migration would fight the migration's own
// checkpoint and transfer machinery on the same frozen process.
func (s *Service) migrationInFlight(workloadID string) bool {
	if s.migrations == nil {
		return false
	}
	return migrationActiveFor(s.migrations.List(), workloadID)
}

// migrationActiveFor reports whether any non-terminal migration in the list
// still targets the workload.
func migrationActiveFor(migrations []model.Migration, workloadID string) bool {
	for _, migration := range migrations {
		if migration.WorkloadID != workloadID {
			continue
		}
		switch migration.Stage {
		case model.MigrationCompleted, model.MigrationFailed, model.MigrationRolledBack, model.MigrationCancelled:
			continue
		default:
			return true
		}
	}
	return false
}

// pruneCheckpoints enforces the policy's retention count after a successful
// checkpoint. Ancestors of the survivors are never deleted; PruneWorkload
// already refuses those, and the failure is logged rather than propagated —
// retention running behind must not fail the checkpoint that just succeeded.
func (s *Service) pruneCheckpoints(workload model.Workload, keepLast int) {
	deleted, err := s.checkpoints.PruneWorkload(workload.Spec.ID, keepLast)
	if err != nil {
		s.logger.Warn("checkpoint retention could not complete",
			"workload_id", workload.Spec.ID, "workload_name", workload.Spec.Name,
			"kept", len(deleted), "error", err)
		return
	}
	if len(deleted) > 0 {
		s.logger.Info("checkpoint retention applied",
			"workload_id", workload.Spec.ID, "workload_name", workload.Spec.Name,
			"deleted", len(deleted), "keep_last", keepLast)
	}
}
