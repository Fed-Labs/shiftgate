package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// liveMigrationAllowed reports whether the organization is entitled to the
// live migration mode right now — the entitlement truth the heartbeat
// transports (machines_routes live_migration_allowed), the job route and the
// agent-command dispatcher gate on, and the CLI/agent mirror via
// billing.LiveMigrationAllowed. The decision is computed from billing state
// (billing.EntitlementState): the catalog or a negotiated tier's contract, an
// active subscription, and a running Business trial — never the cold-only
// free tier or a lapsed subscription. A lookup error fails closed.
func (server *Server) liveMigrationAllowed(ctx context.Context, organizationID string) (bool, error) {
	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil {
		return false, err
	}
	server.releaseExpiredTrial(ctx, entitlement)
	return entitlementStateFor(entitlement).LiveMigrationAllowed(), nil
}

// requireLiveMigrationPlan refuses a live-mode migration for a plan whose
// catalog tier is cold-only. Both platform-mediated migration paths — the
// migration job route and the agent command dispatcher — call it before any
// work starts. Peer-to-peer moves enforce the same contract in the CLI and
// the agent (see docs/limitations.md).
func (server *Server) requireLiveMigrationPlan(writer http.ResponseWriter, request *http.Request, organizationID, mode string) bool {
	if mode != string(model.MigrationLive) {
		return true
	}
	allowed, err := server.liveMigrationAllowed(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return false
	}
	if !allowed {
		writeError(writer, http.StatusPaymentRequired, billing.LiveMigrationPlanRequiredCode, billing.LiveMigrationPlanRequiredMessage)
		return false
	}
	return true
}

func (server *Server) handleMigrationList(writer http.ResponseWriter, request *http.Request) {
	page := pageFromCappedRequest(request)
	records, err := server.database.Migrations(request.Context(), request.PathValue("organizationID"), page)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATIONS_LOOKUP_FAILED", err)
		return
	}
	if page.Offset == 0 {
		server.settleCompletedMigrationPage(request.Context(), request.PathValue("organizationID"), records)
	}
	result := make([]MigrationJob, 0, len(records))
	for _, record := range records {
		result = append(result, migrationJobResponse(record, server.config.RelayEndpoint))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleMigrationCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateMigrationRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	idempotency, validKey := idempotencyKey(request)
	if input.IdempotencyKey != "" {
		bodyKey := strings.TrimSpace(input.IdempotencyKey)
		if !validIdempotencyKey(bodyKey) || (idempotency != "" && idempotency != bodyKey) {
			writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be at most 255 characters and match the request body when both are supplied")
			return
		}
		idempotency = bodyKey
		validKey = true
	}
	if !validKey {
		writeError(writer, http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "Idempotency-Key must be at most 255 characters and contain no line breaks")
		return
	}
	input.Mode = strings.ToLower(strings.TrimSpace(input.Mode))
	if input.Mode == "" {
		input.Mode = "cold"
	}
	if input.Route != "" && input.Route != "direct" && input.Route != "relay" {
		writeError(writer, http.StatusBadRequest, "MIGRATION_ROUTE_INVALID", "route must be direct or relay")
		return
	}
	if input.WorkloadID == "" || input.SourceMachineID == "" || input.DestinationMachineID == "" || (input.Mode != "cold" && input.Mode != "live") {
		writeError(writer, http.StatusBadRequest, "MIGRATION_INVALID", "workload, source, destination, and a cold/live mode are required")
		return
	}
	if err := validateMigrationTimeout(input.TimeoutSeconds); err != nil {
		writeError(writer, http.StatusBadRequest, "MIGRATION_TIMEOUT_INVALID", err.Error())
		return
	}
	if !server.requireLiveMigrationPlan(writer, request, request.PathValue("organizationID"), input.Mode) {
		return
	}
	if !server.checkMigrationRate(writer, request, request.PathValue("organizationID")) {
		server.recordRefusalEvent(request.Context(), request.PathValue("organizationID"), "migration_create", billing.RateLimitUpgradeCode)
		return
	}
	organizationID := request.PathValue("organizationID")
	workloadRecord, workloadErr := server.database.Workload(request.Context(), organizationID, input.WorkloadID)
	if workloadErr != nil {
		if database.IsNotFound(workloadErr) {
			writeError(writer, http.StatusNotFound, "MIGRATION_RESOURCE_NOT_FOUND", "workload or machine was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATION_CREATE_FAILED", workloadErr)
		return
	}
	if workloadStatusDeleted(workloadRecord.Status) {
		writeError(writer, http.StatusConflict, "WORKLOAD_DELETED", "workload is deleted")
		return
	}
	id, err := model.NewID()
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", err)
		return
	}
	principal := principalFrom(request.Context())
	audit := server.auditInput(request, "migration.create", "migration", id, map[string]any{"mode": input.Mode})
	migrationInput := database.MigrationRecord{ID: id, OrganizationID: organizationID, WorkloadID: input.WorkloadID, SourceMachineID: input.SourceMachineID, DestinationMachineID: input.DestinationMachineID, Mode: input.Mode, CreatedBy: principal.UserID, IdempotencyKey: idempotency, Managed: input.Route != "relay", TimeoutSeconds: input.TimeoutSeconds}
	record, created, err := server.database.CreateMigration(request.Context(), migrationInput, audit)
	if errors.Is(err, database.ErrMigrationSourceMismatch) && server.settleWorkloadAssignment(request.Context(), organizationID, input.WorkloadID) {
		record, created, err = server.database.CreateMigration(request.Context(), migrationInput, audit)
	}
	if err != nil {
		switch {
		case database.IsNotFound(err):
			writeError(writer, http.StatusNotFound, "MIGRATION_RESOURCE_NOT_FOUND", "workload or machine was not found")
		case errors.Is(err, database.ErrIdempotencyConflict):
			writeError(writer, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", err.Error())
		case errors.Is(err, database.ErrMigrationSourceMismatch):
			writeError(writer, http.StatusBadRequest, "MIGRATION_SOURCE_MISMATCH", "workload is not assigned to the requested source machine")
		case errors.Is(err, database.ErrMigrationAlreadyActive):
			writeError(writer, http.StatusConflict, "MIGRATION_ALREADY_ACTIVE", "the workload already has a queued or running migration")
		default:
			// Anything else here is storage trouble, not caller input: a
			// database outage must read as one, not as a validation error.
			server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATION_CREATE_FAILED", err)
		}
		return
	}
	if !created {
		writer.Header().Set("Idempotency-Replayed", "true")
	}
	response := migrationJobResponse(record, server.config.RelayEndpoint)
	if input.Route == "relay" && created {
		endpoint, ticket, relayErr := server.setupRelayForMigration(request, record, input)
		if relayErr != nil {
			// The job row exists but its relay leg never armed. Settle it
			// failed so no worker or operator ever sees a half-initialized
			// job with a missing ticket.
			server.settleFailedRelayJob(request, record, relayErr)
			// The mapped message is the response for every status, 5xx
			// included (the cause is logged by settleFailedRelayJob): an
			// unconfigured relay is a deployment state, never "internal
			// error". This mirrors handleRelayTicket.
			status, code, message := relayFailureResponse(relayErr)
			writeError(writer, status, code, message)
			return
		}
		response.Route = "relay"
		response.RelayEndpoint = endpoint
		response.RelayTicket = ticket
	}
	writeJSON(writer, http.StatusAccepted, response)
}

// setupRelayForMigration arms a created relay migration: it issues the pair's
// relay ticket and enqueues the source-side start command. Every step's
// marshal and id errors are real errors here — an empty payload or audit id
// must never reach an agent.
func (server *Server) setupRelayForMigration(request *http.Request, record database.MigrationRecord, input CreateMigrationRequest) (endpoint, ticket string, err error) {
	ctx := request.Context()
	organizationID := request.PathValue("organizationID")
	issued, err := server.issueRelayForMigration(ctx, server.auditInput(request, "relay.ticket.issue", "migration", record.ID, map[string]any{"source_machine_id": input.SourceMachineID, "destination_machine_id": input.DestinationMachineID}), organizationID, input.SourceMachineID, input.DestinationMachineID, record.ID, 120)
	if err != nil {
		return "", "", err
	}
	destination, err := server.database.Machine(ctx, organizationID, input.DestinationMachineID)
	if err != nil {
		return "", "", fmt.Errorf("load relay destination machine: %w", err)
	}
	destinationURL := destination.AgentURL
	if destinationURL == "" {
		destinationURL = "https://relay.invalid"
	}
	payload, err := json.Marshal(map[string]any{"workload_id": input.WorkloadID, "idempotency_key": record.ID, "destination": model.Destination{MachineID: input.DestinationMachineID, AgentURL: destinationURL, ServerName: serverNameFromAgentURL(destination.AgentURL), Route: "relay", RelayEndpoint: issued.Endpoint, RelayTicket: issued.Token}, "mode": input.Mode, "timeout_seconds": input.TimeoutSeconds})
	if err != nil {
		return "", "", fmt.Errorf("encode relay start command: %w", err)
	}
	commandID, err := model.NewID()
	if err != nil {
		return "", "", fmt.Errorf("relay command id: %w", err)
	}
	commandAudit := server.auditInput(request, "relay.command.enqueue", "migration", record.ID, map[string]any{"source_machine_id": input.SourceMachineID})
	if commandAudit.ID, err = model.NewID(); err != nil {
		return "", "", fmt.Errorf("relay command audit id: %w", err)
	}
	if err := server.database.EnqueueAgentCommand(ctx, database.AgentCommandRecord{ID: commandID, OrganizationID: organizationID, MachineID: input.SourceMachineID, Action: "relay.start", Payload: payload}, commandAudit); err != nil {
		return "", "", fmt.Errorf("enqueue relay start command: %w", err)
	}
	return issued.Endpoint, issued.Token, nil
}

// settleFailedRelayJob marks a relay migration whose setup failed as failed,
// so the queue and the fleet view never hold a job that nothing will drive.
// Ticket reconciliation runs with it: the job's issued tickets expire, and
// any relayed bytes recorded for it are metered. The settlement failure is
// logged: the caller is already answering an error.
func (server *Server) settleFailedRelayJob(request *http.Request, record database.MigrationRecord, cause error) {
	server.reconcileRelayTicketUsage(request.Context(), record.OrganizationID, record.ID, 0)
	server.logger.Warn("relay migration setup failed", "migration_id", record.ID, "error", cause)
	// The stored reason is stable text: error_message is client-visible via
	// GET /migrations, so the wrapped cause (SQL detail, endpoint addresses)
	// stays in the server log only.
	if _, err := server.database.UpdateMigrationStatus(request.Context(), record.OrganizationID, record.ID, "failed", record.Progress, "relay setup failed", server.auditInput(request, "migration.relay_setup_failed", "migration", record.ID, nil)); err != nil { //nolint:misspell // persisted status vocabulary
		server.logger.Warn("could not settle relay migration after setup failure", "migration_id", record.ID, "error", err)
	}
}

// relayFailureResponse maps a relay setup failure to the status, code, and
// client-visible message it deserves: configuration and mismatch problems are
// the caller's or operator's to fix; everything else is infrastructure. The
// messages are stable strings — the wrapped cause is logged, never returned.
func relayFailureResponse(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, errRelayNotConfigured):
		return http.StatusServiceUnavailable, "RELAY_NOT_CONFIGURED", "relay transport is not configured on this control plane"
	case errors.Is(err, errRelayPairMismatch):
		return http.StatusConflict, "RELAY_TICKET_FAILED", "relay pair does not match an active migration"
	case errors.Is(err, errRelayTTLInvalid):
		return http.StatusBadRequest, "RELAY_TICKET_FAILED", "ttl_seconds must be between 10 and 600"
	case database.IsNotFound(err):
		return http.StatusNotFound, "MIGRATION_NOT_FOUND", "migration was not found"
	default:
		return http.StatusBadGateway, "RELAY_SETUP_FAILED", "relay setup failed; the migration was settled as failed"
	}
}

func (server *Server) handleMigrationEvents(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	migrationID := request.PathValue("migrationID")
	// A foreign or unknown migration is a 404, not an empty event list:
	// callers must be able to tell "no events yet" from "no such job".
	jobRecord, err := server.database.Migration(request.Context(), organizationID, migrationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MIGRATION_NOT_FOUND", "migration was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATIONS_LOOKUP_FAILED", err)
		return
	}
	if jobRecord.Status == "completed" {
		server.settleWorkloadAssignment(request.Context(), organizationID, jobRecord.WorkloadID)
	}
	records, err := server.database.MigrationEvents(request.Context(), organizationID, migrationID, pageFromCappedRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATION_EVENTS_LOOKUP_FAILED", err)
		return
	}
	result := make([]MigrationEvent, 0, len(records))
	for _, record := range records {
		result = append(result, MigrationEvent{ID: record.ID, Sequence: record.Sequence, Stage: record.Stage, Message: record.Message, Progress: record.Progress, BytesDone: record.BytesDone, BytesTotal: record.BytesTotal, CreatedAt: record.CreatedAt})
	}
	writeJSON(writer, http.StatusOK, result)
}

func validateMigrationTimeout(seconds int) error {
	if seconds == 0 {
		return nil
	}
	if seconds < 30 || seconds > 86400 {
		return errors.New("timeout_seconds must be 0 for the default or between 30 and 86400")
	}
	return nil
}

func migrationJobResponse(record database.MigrationRecord, relayEndpoint string) MigrationJob {
	response := migrationResponse(record)
	if record.Managed {
		response.Route = "direct"
	} else {
		response.Route = "relay"
		response.RelayEndpoint = strings.TrimSpace(relayEndpoint)
	}
	return response
}

const migrationSettleScanPages = 20

func (server *Server) latestWorkloadMigration(ctx context.Context, organizationID, workloadID string, completedOnly bool) (database.MigrationRecord, bool, error) {
	for index := 0; index < migrationSettleScanPages; index++ {
		records, err := server.database.Migrations(ctx, organizationID, database.NormalizePage(database.MaxPageLimit, index*database.MaxPageLimit))
		if err != nil {
			return database.MigrationRecord{}, false, err
		}
		for _, record := range records {
			if record.WorkloadID != workloadID {
				continue
			}
			if completedOnly && record.Status != "completed" {
				continue
			}
			return record, true, nil
		}
		if len(records) < database.MaxPageLimit {
			break
		}
	}
	return database.MigrationRecord{}, false, nil
}

func (server *Server) hasActiveMigration(ctx context.Context, organizationID, workloadID string) (bool, error) {
	job, found, err := server.latestWorkloadMigration(ctx, organizationID, workloadID, false)
	if err != nil || !found {
		return false, err
	}
	return job.Status == "queued" || job.Status == "running", nil
}

func (server *Server) settleWorkloadAssignment(ctx context.Context, organizationID, workloadID string) bool {
	job, found, err := server.latestWorkloadMigration(ctx, organizationID, workloadID, true)
	if err != nil || !found {
		return false
	}
	return server.applyMigrationAssignment(ctx, organizationID, workloadID, job.DestinationMachineID, job.ID)
}

func (server *Server) applyMigrationAssignment(ctx context.Context, organizationID, workloadID, machineID, migrationID string) bool {
	if machineID == "" {
		return false
	}
	workload, err := server.database.Workload(ctx, organizationID, workloadID)
	if err != nil {
		return false
	}
	if workload.MachineID == machineID {
		return false
	}
	record := workload
	record.MachineID = machineID
	auditID, err := model.NewID()
	if err != nil {
		return false
	}
	if _, err := server.database.UpsertWorkload(ctx, record, database.AuditInput{ID: auditID, OrganizationID: organizationID, Action: "workload.assignment_settled", ResourceType: "workload", ResourceID: workloadID, Metadata: map[string]any{"migration_id": migrationID, "machine_id": machineID}}); err != nil {
		server.logger.Warn("could not settle workload assignment after migration", "workload_id", workloadID, "migration_id", migrationID, "error", err)
		return false
	}
	return true
}

func (server *Server) settleCompletedMigrationPage(ctx context.Context, organizationID string, records []database.MigrationRecord) {
	settled := make(map[string]bool)
	for _, record := range records {
		if record.Status != "completed" || settled[record.WorkloadID] {
			continue
		}
		settled[record.WorkloadID] = true
		server.applyMigrationAssignment(ctx, organizationID, record.WorkloadID, record.DestinationMachineID, record.ID)
	}
}
