package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// retention_routes.go: the admin surface for per-organization retention — read
// the policy, replace it. The enforcement sweep itself is server-internal; an
// operator triggers it with the maintenance command rather than through a
// request path, because a sweep that an anonymous caller could provoke would
// be a deletion oracle.

func (server *Server) handleRetentionGet(writer http.ResponseWriter, request *http.Request) {
	// requireOrganization already established the caller's membership; the
	// getter answers "no policy yet" with the documented defaults rather than
	// an error — defaults the enforcement sweep actually runs for orgs with
	// no row, so the displayed policy is the enforced policy.
	policy, err := server.database.RetentionPolicy(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, retentionResponse(policy))
}

// minCheckpointRetentionDays is the smallest nonzero checkpoint retention
// window the API accepts. The checkpoint sweep is destructive to history, so
// a window of "one day" is a footgun, not a policy: zero (keep forever) or at
// least one week.
const minCheckpointRetentionDays = 7

func (server *Server) handleRetentionSet(writer http.ResponseWriter, request *http.Request) {
	var input SetRetentionPolicyRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	organizationID := request.PathValue("organizationID")
	current, err := server.database.RetentionPolicy(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err)
		return
	}
	// RetentionPolicy answers with the documented defaults and a zero
	// UpdatedAt when no row exists — that zero is how this handler tells
	// "first write" from "update", and the defaults it carries are a
	// read-side presentation only.
	creating := current.UpdatedAt.IsZero()
	// Merge for updates: omitted fields keep their stored value; zero is
	// keep-forever and passes through deliberately. For a first write there
	// are no stored values to keep, and silently substituting the displayed
	// defaults would persist a checkpoint window the caller never named —
	// arming checkpoint deletion off, say, a single audit_retention_days
	// update. Creating a policy therefore names every window explicitly.
	policy := database.RetentionPolicy{OrganizationID: organizationID}
	if creating {
		if input.AuditRetentionDays == nil || input.CheckpointRetentionDays == nil || input.DeletedStorageRetentionDays == nil {
			writeError(writer, http.StatusBadRequest, "RETENTION_INVALID", "creating a retention policy requires audit_retention_days, checkpoint_retention_days, and deleted_storage_retention_days to be named explicitly")
			return
		}
		policy.AuditRetentionDays = *input.AuditRetentionDays
		policy.CheckpointRetentionDays = *input.CheckpointRetentionDays
		policy.DeletedStorageRetentionDays = *input.DeletedStorageRetentionDays
	} else {
		policy.AuditRetentionDays = current.AuditRetentionDays
		policy.CheckpointRetentionDays = current.CheckpointRetentionDays
		policy.DeletedStorageRetentionDays = current.DeletedStorageRetentionDays
		if input.AuditRetentionDays != nil {
			policy.AuditRetentionDays = *input.AuditRetentionDays
		}
		if input.CheckpointRetentionDays != nil {
			policy.CheckpointRetentionDays = *input.CheckpointRetentionDays
		}
		if input.DeletedStorageRetentionDays != nil {
			policy.DeletedStorageRetentionDays = *input.DeletedStorageRetentionDays
		}
	}
	// Validation runs in a fixed field order so the field named in an error
	// is deterministic, and the plan cap is resolved once up front: a lookup
	// failure is an error, never a silent downgrade to the free-tier cap
	// that would 402 a Business admin's legitimate window.
	dayCap, err := server.retentionDayCap(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err)
		return
	}
	for _, field := range []struct {
		name string
		days int
	}{
		{"audit_retention_days", policy.AuditRetentionDays},
		{"checkpoint_retention_days", policy.CheckpointRetentionDays},
		{"deleted_storage_retention_days", policy.DeletedStorageRetentionDays},
	} {
		if field.days < 0 || field.days > 36500 {
			writeError(writer, http.StatusBadRequest, "RETENTION_INVALID", field.name+" must be between 0 (keep forever) and 36500 days")
			return
		}
		if field.name == "checkpoint_retention_days" && field.days != 0 && field.days < minCheckpointRetentionDays {
			writeError(writer, http.StatusBadRequest, "RETENTION_INVALID", field.name+" must be 0 (keep forever) or at least "+fmt.Sprint(minCheckpointRetentionDays)+" days")
			return
		}
		if field.days > dayCap {
			writeError(writer, http.StatusPaymentRequired, "RETENTION_PLAN_LIMIT", field.name+" exceeds the retention window this plan allows; upgrade the plan for longer retention")
			return
		}
	}
	if err := server.database.SetRetentionPolicy(request.Context(), policy, server.auditInput(request, "retention.update", "organization", organizationID, map[string]any{
		"audit_retention_days":           policy.AuditRetentionDays,
		"checkpoint_retention_days":      policy.CheckpointRetentionDays,
		"deleted_storage_retention_days": policy.DeletedStorageRetentionDays,
	})); err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "RETENTION_UPDATE_FAILED", err)
		return
	}
	// Re-read so the response carries the row's own updated_at, not a clock we
	// trust less than the database.
	saved, err := server.database.RetentionPolicy(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, retentionResponse(saved))
}

func retentionResponse(policy database.RetentionPolicy) RetentionPolicyResponse {
	return RetentionPolicyResponse{
		OrganizationID:              policy.OrganizationID,
		AuditRetentionDays:          policy.AuditRetentionDays,
		CheckpointRetentionDays:     policy.CheckpointRetentionDays,
		DeletedStorageRetentionDays: policy.DeletedStorageRetentionDays,
		UpdatedAt:                   policy.UpdatedAt,
	}
}

// retentionDayCap is the longest retention window the organization's tier
// allows. Free and Pro are capped at one year (365 days); Business and
// Enterprise — and negotiated plan keys the catalog does not know — run to
// the hard 36500-day ceiling. A subscription that is not active has fallen
// back to free-tier capacity and gets the free-tier cap with it, and an
// organization with no entitlement row is free-tier as well. Any other
// lookup failure is returned as an error: a transient database problem must
// never masquerade as a plan limit and refuse a paid admin's window.
func (server *Server) retentionDayCap(ctx context.Context, organizationID string) (int, error) {
	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil {
		if database.IsNotFound(err) {
			return 365, nil
		}
		return 0, err
	}
	if !entitlement.Active() {
		return 365, nil
	}
	if plan, ok := billing.FindPlan(entitlement.Plan); ok && (plan.Key == "free" || plan.Key == "pro") {
		return 365, nil
	}
	return 36500, nil
}

// sweepRetention is the periodic enforcement pass started by Run when a sweep
// interval is configured. One pass covers every organization; deletes are
// batched so a first run against a long backlog costs a chain of bounded
// transactions instead of one giant one, and the database-side advisory lock
// makes the pass single-flight across replicas. A failed pass logs and waits
// for the next tick — retention is monotone, so a skipped pass only delays
// deletion.
func (server *Server) sweepRetention(ctx context.Context) {
	server.sweepMu.Lock()
	defer server.sweepMu.Unlock()
	sweep, err := server.database.EnforceRetention(ctx)
	if err != nil {
		server.logger.Warn("retention sweep failed", "error", err)
		return
	}
	// GDPR minimization rides the same pass: accounts deactivated (SCIM
	// delete) more than 30 days ago lose their personal data while the
	// pseudonymous rows stay for audit integrity.
	anonymized, err := server.database.AnonymizeDeactivatedUsers(ctx, 30*24*time.Hour)
	if err != nil {
		server.logger.Warn("deactivated-user anonymization failed", "error", err)
	} else if anonymized > 0 {
		server.logger.Info("deactivated users anonymized", "count", anonymized)
	}
	if sweep.AuditEventsDeleted > 0 || sweep.CheckpointsMarked > 0 || sweep.StorageObjectsPurged > 0 ||
		sweep.SessionsPurged > 0 || sweep.APIKeysPurged > 0 || sweep.MigrationJobsPurged > 0 ||
		sweep.AgentCommandsPurged > 0 || sweep.RelayTicketsPurged > 0 || sweep.RelayNoncesPurged > 0 ||
		sweep.StripeEventsPurged > 0 || sweep.UsageRecordsPurged > 0 ||
		sweep.ReservationsExpired > 0 || sweep.ReservationsPurged > 0 || sweep.MachinesMarkedOffline > 0 {
		server.logger.Info("retention sweep",
			"audit_events_deleted", sweep.AuditEventsDeleted,
			"checkpoints_marked", sweep.CheckpointsMarked,
			"storage_objects_purged", sweep.StorageObjectsPurged,
			"sessions_purged", sweep.SessionsPurged,
			"api_keys_purged", sweep.APIKeysPurged,
			"migration_jobs_purged", sweep.MigrationJobsPurged,
			"agent_commands_purged", sweep.AgentCommandsPurged,
			"relay_tickets_purged", sweep.RelayTicketsPurged,
			"relay_nonces_purged", sweep.RelayNoncesPurged,
			"stripe_events_purged", sweep.StripeEventsPurged,
			"usage_records_purged", sweep.UsageRecordsPurged,
			"reservations_expired", sweep.ReservationsExpired,
			"reservations_purged", sweep.ReservationsPurged,
			"machines_marked_offline", sweep.MachinesMarkedOffline)
	}
}

// startRetentionSweeper runs the retention sweep on the configured interval
// until ctx ends. Interval zero means disabled and returns immediately.
func (server *Server) startRetentionSweeper(ctx context.Context) {
	if server.config.RetentionSweepInterval <= 0 {
		return
	}
	interval := server.config.RetentionSweepInterval
	server.backgroundWg.Add(1)
	go func() {
		defer server.backgroundWg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// The sweep runs on a detached, bounded context so a batch can
				// finish its in-flight statements when shutdown arrives; the
				// shutdown drain waits for it briefly (bounded) and each batch
				// is short enough to finish or leave the next tick to resume
				// the backlog.
				sweepCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				server.sweepRetention(sweepCtx)
				cancel()
			}
		}
	}()
}
