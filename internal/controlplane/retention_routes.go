package controlplane

import (
	"context"
	"net/http"
	"time"

	"shift.dev/shift/internal/database"
)

// retention_routes.go: the admin surface for per-organization retention — read
// the policy, replace it. The enforcement sweep itself is server-internal; an
// operator triggers it with the maintenance command rather than through a
// request path, because a sweep that an anonymous caller could provoke would
// be a deletion oracle.

func (server *Server) handleRetentionGet(writer http.ResponseWriter, request *http.Request) {
	// requireOrganization already established the caller's membership; the
	// getter answers "no policy yet" with the defaults rather than an error.
	policy, err := server.database.RetentionPolicy(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, retentionResponse(policy))
}

func (server *Server) handleRetentionSet(writer http.ResponseWriter, request *http.Request) {
	var input SetRetentionPolicyRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	organizationID := request.PathValue("organizationID")
	current, err := server.database.RetentionPolicy(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err.Error())
		return
	}
	// Merge: omitted fields keep their current value; zero is keep-forever and
	// passes through deliberately.
	policy := database.RetentionPolicy{
		OrganizationID:              organizationID,
		AuditRetentionDays:          current.AuditRetentionDays,
		CheckpointRetentionDays:     current.CheckpointRetentionDays,
		DeletedStorageRetentionDays: current.DeletedStorageRetentionDays,
	}
	if input.AuditRetentionDays != nil {
		policy.AuditRetentionDays = *input.AuditRetentionDays
	}
	if input.CheckpointRetentionDays != nil {
		policy.CheckpointRetentionDays = *input.CheckpointRetentionDays
	}
	if input.DeletedStorageRetentionDays != nil {
		policy.DeletedStorageRetentionDays = *input.DeletedStorageRetentionDays
	}
	for field, days := range map[string]int{
		"audit_retention_days":           policy.AuditRetentionDays,
		"checkpoint_retention_days":      policy.CheckpointRetentionDays,
		"deleted_storage_retention_days": policy.DeletedStorageRetentionDays,
	} {
		if days < 0 || days > 36500 {
			writeError(writer, http.StatusBadRequest, "RETENTION_INVALID", field+" must be between 0 (keep forever) and 36500 days")
			return
		}
	}
	if err := server.database.SetRetentionPolicy(request.Context(), policy, server.auditInput(request, "retention.update", "organization", organizationID, map[string]any{
		"audit_retention_days":           policy.AuditRetentionDays,
		"checkpoint_retention_days":      policy.CheckpointRetentionDays,
		"deleted_storage_retention_days": policy.DeletedStorageRetentionDays,
	})); err != nil {
		writeError(writer, http.StatusInternalServerError, "RETENTION_UPDATE_FAILED", err.Error())
		return
	}
	// Re-read so the response carries the row's own updated_at, not a clock we
	// trust less than the database.
	saved, err := server.database.RetentionPolicy(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "RETENTION_LOOKUP_FAILED", err.Error())
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

// sweepRetention is the periodic enforcement pass started by Run when a sweep
// interval is configured. One pass covers every organization; the statements
// are set-based, so the cost scales with rows past their window, not with
// fleet size. A failed pass logs and waits for the next tick — retention is
// monotone, so a skipped pass only delays deletion.
func (server *Server) sweepRetention(ctx context.Context) {
	sweep, err := server.database.EnforceRetention(ctx)
	if err != nil {
		server.logger.Warn("retention sweep failed", "error", err)
		return
	}
	if sweep.AuditEventsDeleted > 0 || sweep.CheckpointsMarked > 0 || sweep.StorageObjectsPurged > 0 {
		server.logger.Info("retention sweep",
			"audit_events_deleted", sweep.AuditEventsDeleted,
			"checkpoints_marked", sweep.CheckpointsMarked,
			"storage_objects_purged", sweep.StorageObjectsPurged)
	}
}

// startRetentionSweeper runs the retention sweep on the configured interval
// until ctx ends. Interval zero means disabled and returns immediately.
func (server *Server) startRetentionSweeper(ctx context.Context) {
	if server.config.RetentionSweepInterval <= 0 {
		return
	}
	interval := server.config.RetentionSweepInterval
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// The sweep runs on a detached context: it must finish its
				// statements even while shutdown drains in-flight requests,
				// because a half-applied sweep is exactly what the single
				// set-based statements exist to avoid.
				sweepCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				server.sweepRetention(sweepCtx)
				cancel()
			}
		}
	}()
}
