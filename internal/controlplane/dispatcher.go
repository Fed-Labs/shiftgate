package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// workloadStatusForRestore maps the restore transaction state onto the shared
// workload status enum (model.WorkloadStatus — the constants live in
// internal/model, used as-is here). The workloads UI keys every action on
// those literals, so a restore must never publish the transaction vocabulary
// ("COMMITTED", "VALIDATED") into workloads.status.state.
func workloadStatusForRestore(state checkpoint.RestoreState) model.WorkloadStatus {
	switch state {
	case checkpoint.RestoreCommitted, checkpoint.RestoreValidated:
		return model.WorkloadRunning
	case checkpoint.RestoreProcessRunning:
		return model.WorkloadStarting
	case checkpoint.RestoreRolledBack, checkpoint.RestoreRollingBack:
		return model.WorkloadStopped
	case checkpoint.RestoreFailed:
		return model.WorkloadFailed
	default:
		return model.WorkloadRestoring
	}
}

func (server *Server) handleAgentCommand(writer http.ResponseWriter, request *http.Request) {
	var input AgentCommandRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Action = strings.ToLower(strings.TrimSpace(input.Action))
	if !validAgentCommand(input) {
		writeError(writer, http.StatusBadRequest, "AGENT_COMMAND_INVALID", "action or referenced resource is invalid")
		return
	}
	organizationID := request.PathValue("organizationID")
	machineID := request.PathValue("machineID")

	sourceRecord, err := server.database.Machine(request.Context(), organizationID, machineID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "machine was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "AGENT_MACHINE_LOOKUP_FAILED", err)
		return
	}
	source := &sourceRecord
	var destination *database.MachineRecord
	if input.DestinationID != "" {
		destinationRecord, destErr := server.database.Machine(request.Context(), organizationID, input.DestinationID)
		if destErr != nil && !database.IsNotFound(destErr) {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "AGENT_MACHINE_LOOKUP_FAILED", destErr)
			return
		}
		if destErr == nil {
			destination = &destinationRecord
		}
	}
	if source.AgentURL == "" {
		writeError(writer, http.StatusConflict, "AGENT_ENDPOINT_MISSING", "machine has no reachable agent URL")
		return
	}
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 10*time.Minute {
		timeout = 10 * time.Minute
	}
	// The dispatch runs inside this request's budget: advertising a longer
	// timeout than the request context can wait for only guarantees a
	// mid-flight cancellation. The agent still receives the caller's own
	// TimeoutSeconds as its work budget.
	if requestBudget := server.config.RequestTimeout; requestBudget > 0 && timeout > requestBudget {
		timeout = requestBudget
	}

	workloadRecord, err := server.database.Workload(request.Context(), organizationID, input.WorkloadID)
	var workload *database.WorkloadRecord
	if err == nil {
		workload = &workloadRecord
	} else if !database.IsNotFound(err) {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "WORKLOAD_LOOKUP_FAILED", err)
		return
	}

	client, err := server.newAgentClient(source.AgentURL, timeout)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "AGENT_ENDPOINT_INVALID", err.Error())
		return
	}
	commandContext := request.Context()

	response := AgentCommandResponse{Machine: machineID, Action: input.Action}
	switch input.Action {
	case "start", "pause", "resume", "stop":
		if workload == nil || (workload.MachineID != "" && workload.MachineID != machineID) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload was not found on this machine")
			return
		}
		actionInput := struct {
			TimeoutSeconds int `json:"timeout_seconds,omitempty"`
		}{TimeoutSeconds: input.TimeoutSeconds}
		result, err := client.WorkloadAction(commandContext, input.WorkloadID, input.Action, actionInput)
		if err != nil {
			server.writeAgentError(writer, request, err)
			return
		}
		status := map[string]any{"state": string(result.Status), "agent": redactedWorkloadView(result)}
		record, err := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workload.ID, mustJSON(status), server.auditInput(request, "workload."+input.Action, "workload", workload.ID, map[string]any{"dispatched": true}))
		if err != nil {
			server.writeInternalError(writer, request, http.StatusBadGateway, "WORKLOAD_RECONCILE_FAILED", err)
			return
		}
		updated := workloadResponse(record)
		response.Result = result
		response.Workload = &updated
	case "delete":
		if workload == nil || (workload.MachineID != "" && workload.MachineID != machineID) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload was not found on this machine")
			return
		}
		if err := client.DeleteWorkload(commandContext, input.WorkloadID); err != nil {
			server.writeAgentError(writer, request, err)
			return
		}
		record, err := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workload.ID, mustJSON(map[string]any{"state": string(model.WorkloadStopped)}), server.auditInput(request, "workload.delete", "workload", workload.ID, map[string]any{"dispatched": true}))
		if err != nil {
			server.writeInternalError(writer, request, http.StatusBadGateway, "WORKLOAD_RECONCILE_FAILED", err)
			return
		}
		updated := workloadResponse(record)
		response.Result = map[string]string{"status": "deleted"}
		response.Workload = &updated
	case "checkpoint":
		if workload == nil || (workload.MachineID != "" && workload.MachineID != machineID) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload was not found on this machine")
			return
		}
		createRequest := agentclient.CheckpointCreateRequest{
			WorkloadID:     workload.ID,
			Kind:           model.CheckpointFull,
			TimeoutSeconds: input.TimeoutSeconds,
		}
		manifest, err := client.CreateCheckpoint(commandContext, createRequest)
		if err != nil {
			server.writeAgentError(writer, request, err)
			return
		}
		// The stored projection is redacted: plaintext environment, command
		// arguments, and unkeyed digests never persist in control-plane
		// metadata (S-03). The response carries the same projection.
		manifest = manifest.Public()
		metadata, metadataErr := json.Marshal(manifest)
		if metadataErr != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "CHECKPOINT_ENCODE_FAILED", metadataErr)
			return
		}
		checkpointRecord := database.CheckpointRecord{
			ID: manifest.ID, OrganizationID: organizationID, WorkloadID: workload.ID,
			MachineID: machineID, Kind: string(manifest.Kind), ParentID: manifest.ParentID,
			Manifest: metadata, PlainBytes: manifest.Metrics.PlainBytes,
			StoredBytes: manifest.Metrics.StoredBytes, ChunkCount: manifest.Metrics.ChunkCount,
		}
		record, err := server.database.CreateCheckpointForAgent(request.Context(), checkpointRecord, server.auditInput(request, "checkpoint.create", "checkpoint", checkpointRecord.ID, map[string]any{"dispatched": true}))
		if err != nil {
			server.writeDatabaseErrorCheckpoint(writer, request, err)
			return
		}
		registered := checkpointResponse(record)
		response.Result = manifest
		response.Checkpoint = &registered
	case "restore":
		if workload == nil || (workload.MachineID != "" && workload.MachineID != machineID) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload target was not found on this machine")
			return
		}
		result, err := client.Restore(commandContext, input.CheckpointID, agentclient.RestoreRequest{TimeoutSeconds: input.TimeoutSeconds})
		if err != nil {
			server.writeAgentError(writer, request, err)
			return
		}
		status := map[string]any{"state": string(workloadStatusForRestore(result.State)), "restore_id": result.ID}
		record, reconcileErr := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workload.ID, mustJSON(status), server.auditInput(request, "checkpoint.restore", "checkpoint", input.CheckpointID, map[string]any{"restore_id": result.ID}))
		if reconcileErr != nil {
			server.writeInternalError(writer, request, http.StatusBadGateway, "RESTORE_RECONCILE_FAILED", reconcileErr)
			return
		}
		updated := workloadResponse(record)
		response.Result = result
		response.Workload = &updated
	case "migrate":
		if destination == nil {
			writeError(writer, http.StatusNotFound, "DESTINATION_NOT_FOUND", "destination machine was not found")
			return
		}
		if workload == nil || (workload.MachineID != "" && workload.MachineID != machineID) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload was not found on this machine")
			return
		}
		if !server.requireLiveMigrationPlan(writer, request, organizationID, input.Mode) {
			return
		}
		mode := model.MigrationCold
		if input.Mode == "live" {
			mode = model.MigrationLive
		}
		id, idErr := model.NewID()
		if idErr != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", idErr)
			return
		}
		principal := principalFrom(request.Context())
		// The job row lands before the agent is called: a crash, a restart, or
		// a lost response must never leave the agent running a migration the
		// control plane has no record of. The job id is also the dispatch
		// idempotency key, so the unmanaged settler can adopt this row and
		// reconcile the agent side even without a stored agent migration id.
		if _, jobErr := server.database.CreateMigrationForAgent(request.Context(), database.MigrationRecord{
			ID: id, OrganizationID: organizationID, WorkloadID: workload.ID,
			SourceMachineID: machineID, DestinationMachineID: destination.MachineID,
			Mode: input.Mode, CreatedBy: principal.UserID, TimeoutSeconds: input.TimeoutSeconds,
		}, server.auditInput(request, "migration.dispatch", "migration", id, map[string]any{"mode": input.Mode})); jobErr != nil {
			if errors.Is(jobErr, database.ErrMigrationAlreadyActive) {
				writeError(writer, http.StatusConflict, "MIGRATION_ALREADY_ACTIVE", jobErr.Error())
				return
			}
			server.writeInternalError(writer, request, http.StatusBadGateway, "MIGRATION_RECONCILE_FAILED", jobErr)
			return
		}
		server.watchUnmanagedMigration(organizationID, id)
		agentRequest := agentclient.MigrationCreateRequest{
			WorkloadID: workload.ID,
			// The job id is the dispatch idempotency key: a retried dispatch
			// replays on the agent instead of creating a second migration.
			IdempotencyKey: id,
			Destination: model.Destination{
				MachineID: destination.MachineID, AgentURL: destination.AgentURL,
				ServerName: serverNameFromAgentURL(destination.AgentURL),
			},
			Mode: mode, TimeoutSeconds: input.TimeoutSeconds,
		}
		result, err := client.CreateMigration(commandContext, agentRequest)
		if err != nil {
			// A definitive agent refusal settles the row failed. An ambiguous
			// failure (timeout, transport, 5xx) may still have been accepted
			// by the agent: leave the row active for the unmanaged settler to
			// resolve from the agent's own record instead of misreporting it.
			var apiError *agentclient.APIError
			if errors.As(err, &apiError) && apiError.Status < 500 {
				server.finishUnmanagedMigration(request.Context(), organizationID, id, "failed", migrationStageProgress("FAILED"), "agent refused the migration")
			}
			server.writeAgentError(writer, request, err)
			return
		}
		progress, _ := json.Marshal(migrationProgress(result))
		server.recordMigrationEvents(request.Context(), id, result)
		jobRecord, jobErr := server.database.UpdateMigrationStatus(request.Context(), organizationID, id, migrationJobStatus(result.Stage), progress, result.FailureReason, server.auditInput(request, "migration.progress", "migration", id, map[string]any{"agent_migration_id": result.ID, "mode": input.Mode}))
		if jobErr != nil {
			server.writeInternalError(writer, request, http.StatusBadGateway, "MIGRATION_STATUS_RECONCILE_FAILED", jobErr)
			return
		}
		migration := migrationResponse(jobRecord)
		response.Result = result
		response.Migration = &migration
	default:
		writeError(writer, http.StatusBadRequest, "AGENT_COMMAND_UNSUPPORTED", "unsupported command")
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func validAgentCommand(input AgentCommandRequest) bool {
	switch input.Action {
	case "start", "pause", "resume", "stop", "delete":
		return input.WorkloadID != ""
	case "checkpoint":
		return input.WorkloadID != "" && input.TimeoutSeconds >= 0
	case "restore":
		return input.WorkloadID != "" && input.CheckpointID != ""
	case "migrate":
		return input.WorkloadID != "" && input.DestinationID != "" && (input.Mode == "" || input.Mode == "cold" || input.Mode == "live")
	default:
		return false
	}
}

func (server *Server) writeAgentError(writer http.ResponseWriter, request *http.Request, err error) {
	var apiError *agentclient.APIError
	if errors.As(err, &apiError) {
		status := http.StatusBadGateway
		switch apiError.Status {
		case http.StatusBadRequest, http.StatusConflict, http.StatusForbidden, http.StatusNotFound:
			status = apiError.Status
		}
		if status < 500 {
			writeError(writer, status, "AGENT_COMMAND_FAILED", apiError.Message)
			return
		}
		server.writeInternalError(writer, request, status, "AGENT_COMMAND_FAILED", err)
		return
	}
	server.writeInternalError(writer, request, http.StatusBadGateway, "AGENT_UNREACHABLE", err)
}

func (server *Server) writeDatabaseErrorCheckpoint(writer http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, database.ErrStorageEntitlementExceeded):
		writeError(writer, http.StatusPaymentRequired, "CHECKPOINT_STORAGE_ENTITLEMENT_EXCEEDED", err.Error())
	case errors.Is(err, database.ErrCheckpointMachineMismatch):
		writeError(writer, http.StatusConflict, "CHECKPOINT_MACHINE_MISMATCH", err.Error())
	case database.IsConflict(err):
		writeError(writer, http.StatusConflict, "CHECKPOINT_EXISTS", "checkpoint metadata already exists")
	case database.IsNotFound(err):
		writeError(writer, http.StatusNotFound, "CHECKPOINT_RESOURCE_NOT_FOUND", "workload or machine was not found")
	default:
		server.writeInternalError(writer, request, http.StatusBadGateway, "CHECKPOINT_RECONCILE_FAILED", err)
	}
}

func fmtInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"error":"encode failed"}`)
	}
	return encoded
}

func (server *Server) newAgentClient(agentURL string, timeout time.Duration) (*agentclient.Client, error) {
	return agentclient.NewWithTLS(agentURL, timeout, agentclient.TLSOptions{
		CertificateFile: server.config.AgentTLS.CertificateFile,
		PrivateKeyFile:  server.config.AgentTLS.PrivateKeyFile,
		PeerCAFile:      server.config.AgentTLS.PeerCAFile,
		ServerName:      server.config.AgentTLS.ServerName,
	})
}

func serverNameFromAgentURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return parsed.Hostname()
}

// migrationProgress renders the persisted progress document. The contract
// keys — current_stage, percent (0-100), downtime_ms, transfer_speed
// (bytes/sec), deduplicated_bytes, transferred_bytes, total_state_bytes — are
// what the dashboard's stage tracker, progress bar, and SPEED/DOWNTIME/
// DEDUPLICATED readouts consume. The legacy stage/progress (0..1)/agent_id
// keys stay for the cancel path and older clients; agent_id is the agent's
// migration id and must keep its name.
func migrationProgress(result model.Migration) map[string]any {
	fraction := 0.0
	if len(result.Events) > 0 {
		fraction = result.Events[len(result.Events)-1].Progress
	}
	if result.Stage == model.MigrationCompleted {
		fraction = 1
	}
	return map[string]any{
		"stage":              string(result.Stage),
		"current_stage":      string(result.Stage),
		"progress":           fraction,
		"percent":            fraction * 100,
		"agent_id":           result.ID,
		"downtime_ms":        result.Metrics.Downtime.Milliseconds(),
		"transfer_speed":     result.Metrics.TransferBytesPerSec,
		"deduplicated_bytes": result.Metrics.DeduplicatedBytes,
		"transferred_bytes":  result.Metrics.TransferredBytes,
		"total_state_bytes":  result.Metrics.TotalStateBytes,
	}
}

// migrationStageProgress renders the progress document for a job that reached
// a stage without live agent metrics (queued, cancelled, refused).
func migrationStageProgress(stage string) json.RawMessage {
	return mustJSON(map[string]any{
		"stage": stage, "current_stage": stage,
		"progress": 0.0, "percent": 0.0, "agent_id": "",
		"downtime_ms": 0, "transfer_speed": 0.0,
		"deduplicated_bytes": 0, "transferred_bytes": 0, "total_state_bytes": 0,
	})
}

// redactedWorkloadView strips plaintext workload material before a workload
// projection is persisted in control-plane metadata (S-03).
func redactedWorkloadView(workload model.Workload) model.Workload {
	workload.Spec = workload.Spec.Public()
	return workload
}

func migrationJobStatus(stage model.MigrationStage) string {
	switch stage {
	case model.MigrationCompleted:
		return "completed"
	case model.MigrationFailed, model.MigrationRolledBack:
		return "failed"
	case model.MigrationCancelled:
		// The two-L spelling is the job-status value the dashboard's MigrationStatus union carries.
		return "cancelled" //nolint:misspell // wire value: matches the persisted migration status
	default:
		return "running"
	}
}

// workloadReconcileGrace is how long a freshly written workload row is
// protected from the fleet sweep: a user action moments earlier (a status
// set, a delete) must not be clobbered by the agent's stale view.
const workloadReconcileGrace = 5 * time.Second

func (server *Server) handleReconcile(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	// The sweep walks N×M remote calls. It gets its own deadline derived from
	// the request budget so slow agents cannot stall other requests, and it
	// reports honestly: when the budget runs out mid-pass, the body says the
	// pass is partial instead of quietly counting what happened to finish.
	sweepContext, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), server.config.RequestTimeout)
	defer cancel()
	machines, err := server.allMachines(sweepContext, organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", err)
		return
	}
	reconciled := 0
	failed := make(map[string]string)
	partial := 0
	// Reconcile failures are reported by stable reason: the 200 body is
	// client-visible, so raw error text (SQLSTATE detail, endpoint addresses)
	// goes to the log and only the reason names the failure class.
	fail := func(key, reason string, cause error) {
		if cause != nil {
			server.logger.Warn("fleet reconcile failure", "resource", key, "reason", reason, "error", cause)
		}
		failed[key] = reason
	}
	for _, machine := range machines {
		if machine.AgentURL == "" || machine.Status == "disabled" {
			continue
		}
		if sweepContext.Err() != nil {
			partial++
			continue
		}
		client, err := server.newAgentClient(machine.AgentURL, 15*time.Second)
		if err != nil {
			fail(machine.MachineID, "agent URL invalid", err)
			continue
		}
		workloads, err := client.Workloads(sweepContext)
		if err != nil {
			fail(machine.MachineID, "agent unreachable", err)
			continue
		}
		for _, workload := range workloads {
			if sweepContext.Err() != nil {
				partial++
				continue
			}
			if server.skipWorkloadReconcile(sweepContext, organizationID, workload.Spec.ID) {
				continue
			}
			// The stored projection is redacted: plaintext environment, path,
			// health-check, and argv material never persist in control-plane
			// metadata (S-03).
			public := workload
			public.Spec = workload.Spec.Public()
			specEncoded, err := json.Marshal(public.Spec)
			if err != nil {
				specEncoded = []byte(`{}`)
			}
			status := map[string]any{"state": string(workload.Status), "agent": public}
			statusEncoded, _ := json.Marshal(status)
			record, err := server.database.UpsertWorkload(sweepContext, database.WorkloadRecord{
				ID: workload.Spec.ID, OrganizationID: organizationID, MachineID: machine.MachineID,
				Name: workload.Spec.Name, Spec: specEncoded, Status: statusEncoded,
			}, server.auditInput(request, "workload.reconcile", "workload", workload.Spec.ID, map[string]any{"machine_id": machine.MachineID}))
			if err != nil {
				fail(workload.Spec.ID, "workload reconcile failed", err)
				continue
			}
			_ = record
			reconciled++
			checkpoints, err := client.Checkpoints(sweepContext, workload.Spec.ID)
			if err != nil {
				fail("checkpoint:"+workload.Spec.ID, "checkpoint list failed", err)
				continue
			}
			for _, summary := range checkpoints {
				manifest, err := client.Checkpoint(sweepContext, summary.ID)
				if err != nil {
					fail("manifest:"+summary.ID, "checkpoint manifest fetch failed", err)
					continue
				}
				manifestEncoded, encodeErr := json.Marshal(manifest.Public())
				if encodeErr != nil {
					fail("manifest:"+summary.ID, "checkpoint manifest invalid", encodeErr)
					continue
				}
				_, err = server.database.CreateCheckpointForAgent(sweepContext, database.CheckpointRecord{
					ID: manifest.ID, OrganizationID: organizationID, WorkloadID: manifest.Workload.ID,
					MachineID: manifest.SourceIdentity.ID, Kind: string(manifest.Kind), ParentID: manifest.ParentID,
					Manifest: manifestEncoded, PlainBytes: manifest.Metrics.PlainBytes,
					StoredBytes: manifest.Metrics.StoredBytes, ChunkCount: manifest.Metrics.ChunkCount,
				}, server.auditInput(request, "checkpoint.reconcile", "checkpoint", manifest.ID, nil))
				if err != nil && !database.IsConflict(err) {
					fail("checkpoint:"+summary.ID, "checkpoint reconcile failed", err)
				}
			}
		}
	}
	body := map[string]any{"reconciled_workloads": reconciled, "failed": failed}
	if partial > 0 {
		// The pass ran out of budget: say so instead of implying the fleet is
		// fully reconciled.
		body["complete"] = false
		body["skipped_machines"] = partial
	}
	writeJSON(writer, http.StatusOK, body)
}

// skipWorkloadReconcile reports whether the sweep must leave a workload row
// alone: rows the user just touched are given a grace window, and rows the
// user deleted are never resurrected from the agent's stale inventory.
func (server *Server) skipWorkloadReconcile(ctx context.Context, organizationID, workloadID string) bool {
	existing, err := server.database.Workload(ctx, organizationID, workloadID)
	if err != nil {
		// Unknown (or unreadable) rows are reconciled in as usual.
		return false
	}
	var stored struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(existing.Status, &stored)
	if stored.State == "deleted" {
		return true
	}
	return time.Since(existing.UpdatedAt) < workloadReconcileGrace
}

func (server *Server) handleMigrationCancel(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	migrationID := request.PathValue("migrationID")
	jobRecord, err := server.database.Migration(request.Context(), organizationID, migrationID)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MIGRATION_NOT_FOUND", "migration was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATIONS_LOOKUP_FAILED", err)
		return
	}
	job := &jobRecord
	if job.Status != "queued" && job.Status != "running" {
		writeError(writer, http.StatusConflict, "MIGRATION_NOT_ACTIVE", "migration is no longer active")
		return
	}
	if job.Managed {
		if job.CancelRequested {
			// A repeated cancel is not idempotent success: the documented
			// contract is 409 once cancellation has already been requested.
			writeError(writer, http.StatusConflict, "MIGRATION_NOT_ACTIVE", "migration cancellation is already in progress")
			return
		}
		record, requestErr := server.database.RequestManagedMigrationCancel(request.Context(), organizationID, migrationID)
		if requestErr != nil {
			if database.IsNotFound(requestErr) {
				writeError(writer, http.StatusConflict, "MIGRATION_NOT_ACTIVE", "migration is no longer active")
				return
			}
			server.writeInternalError(writer, request, http.StatusBadGateway, "MIGRATION_CANCEL_REQUEST_FAILED", requestErr)
			return
		}
		writeJSON(writer, http.StatusAccepted, migrationResponse(record))
		return
	}
	// Touch the settlement watch so the job keeps being followed even if this
	// cancel cannot reach the agent right now.
	server.watchUnmanagedMigration(organizationID, migrationID)
	var progress map[string]any
	if err := json.Unmarshal(job.Progress, &progress); err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "MIGRATION_PROGRESS_INVALID", err)
		return
	}
	agentMigrationID, _ := progress["agent_id"].(string)
	if agentMigrationID == "" {
		writeError(writer, http.StatusConflict, "AGENT_MIGRATION_MISSING", "migration has not been assigned an agent identifier")
		return
	}
	sourceRecord, err := server.database.Machine(request.Context(), organizationID, job.SourceMachineID)
	if err != nil || sourceRecord.AgentURL == "" {
		if err != nil && !database.IsNotFound(err) {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", err)
			return
		}
		writeError(writer, http.StatusConflict, "SOURCE_AGENT_UNAVAILABLE", "source machine or agent URL is unavailable")
		return
	}
	source := &sourceRecord
	client, clientErr := server.newAgentClient(source.AgentURL, 30*time.Second)
	if clientErr != nil {
		writeError(writer, http.StatusBadRequest, "AGENT_ENDPOINT_INVALID", clientErr.Error())
		return
	}
	if err := client.CancelMigration(request.Context(), agentMigrationID); err != nil {
		server.writeAgentError(writer, request, err)
		return
	}
	record, err := server.database.UpdateMigrationStatus(request.Context(), organizationID, migrationID, "cancelled", migrationStageProgress("CANCELLED"), "", server.auditInput(request, "migration.cancel", "migration", migrationID, nil)) //nolint:misspell // persisted status vocabulary
	if err != nil {
		server.writeInternalError(writer, request, http.StatusBadGateway, "MIGRATION_RECONCILE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusAccepted, migrationResponse(record))
}
