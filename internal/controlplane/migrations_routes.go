package controlplane

import (
	"context"
	"net/http"
	"strings"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// liveMigrationAllowed reports whether the organization's plan includes the
// live migration mode. The catalog is the single source of truth — the same
// value the pricing page renders — and the free tier is cold-only. A plan key
// the catalog does not know (a negotiated Stripe metadata override) fails
// open: those subscriptions are paid tiers, and this gate differentiates
// tiers rather than protecting a metered resource.
func (server *Server) liveMigrationAllowed(ctx context.Context, organizationID string) (bool, error) {
	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil {
		return false, err
	}
	plan, known := billing.FindPlan(entitlement.Plan)
	if !known {
		return true, nil
	}
	return plan.LiveMigration, nil
}

// requireLiveMigrationPlan refuses a live-mode migration for a plan whose
// catalog tier is cold-only. Both platform-mediated migration paths — the
// migration job route and the agent command dispatcher — call it before any
// work starts.
func (server *Server) requireLiveMigrationPlan(writer http.ResponseWriter, request *http.Request, organizationID, mode string) bool {
	if mode != string(model.MigrationLive) {
		return true
	}
	allowed, err := server.liveMigrationAllowed(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err.Error())
		return false
	}
	if !allowed {
		writeError(writer, http.StatusPaymentRequired, "LIVE_MIGRATION_PLAN_REQUIRED", "live migration requires a paid plan; the free tier is cold-only")
		return false
	}
	return true
}

func (server *Server) handleMigrationList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.Migrations(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MIGRATIONS_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]MigrationJob, 0, len(records))
	for _, record := range records {
		result = append(result, migrationResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleMigrationCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateMigrationRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Mode = strings.ToLower(strings.TrimSpace(input.Mode))
	if input.Mode == "" {
		input.Mode = "cold"
	}
	if input.WorkloadID == "" || input.SourceMachineID == "" || input.DestinationMachineID == "" || (input.Mode != "cold" && input.Mode != "live") {
		writeError(writer, http.StatusBadRequest, "MIGRATION_INVALID", "workload, source, destination, and a cold/live mode are required")
		return
	}
	if !server.requireLiveMigrationPlan(writer, request, request.PathValue("organizationID"), input.Mode) {
		return
	}
	id, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	principal := principalFrom(request.Context())
	record, err := server.database.CreateMigration(request.Context(), database.MigrationRecord{ID: id, OrganizationID: request.PathValue("organizationID"), WorkloadID: input.WorkloadID, SourceMachineID: input.SourceMachineID, DestinationMachineID: input.DestinationMachineID, Mode: input.Mode, CreatedBy: principal.UserID}, server.auditInput(request, "migration.create", "migration", id, map[string]any{"mode": input.Mode}))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MIGRATION_RESOURCE_NOT_FOUND", "workload or machine was not found")
			return
		}
		writeError(writer, http.StatusBadRequest, "MIGRATION_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, migrationResponse(record))
}

func (server *Server) handleMigrationEvents(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.MigrationEvents(request.Context(), request.PathValue("organizationID"), request.PathValue("migrationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MIGRATION_EVENTS_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]MigrationEvent, 0, len(records))
	for _, record := range records {
		result = append(result, MigrationEvent{ID: record.ID, Sequence: record.Sequence, Stage: record.Stage, Message: record.Message, Progress: record.Progress, BytesDone: record.BytesDone, BytesTotal: record.BytesTotal, CreatedAt: record.CreatedAt})
	}
	writeJSON(writer, http.StatusOK, result)
}
