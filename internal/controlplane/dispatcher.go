package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

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

	machines, err := server.database.Machines(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "AGENT_MACHINE_LOOKUP_FAILED", err.Error())
		return
	}
	var source *database.MachineRecord
	var destination *database.MachineRecord
	for index := range machines {
		switch machines[index].MachineID {
		case machineID:
			source = &machines[index]
		case input.DestinationID:
			destination = &machines[index]
		}
	}
	if source == nil {
		writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "machine was not found")
		return
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

	workloads, err := server.database.Workloads(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "WORKLOAD_LOOKUP_FAILED", err.Error())
		return
	}
	var workload *database.WorkloadRecord
	for index := range workloads {
		if workloads[index].ID == input.WorkloadID {
			workload = &workloads[index]
			break
		}
	}

	client, err := agentclient.New(source.AgentURL, timeout)
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
			writeAgentError(writer, err)
			return
		}
		status := map[string]any{"state": string(result.Status), "agent": result}
		record, err := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workload.ID, mustJSON(status), server.auditInput(request, "workload."+input.Action, "workload", workload.ID, map[string]any{"dispatched": true}))
		if err != nil {
			writeError(writer, http.StatusBadGateway, "WORKLOAD_RECONCILE_FAILED", err.Error())
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
			writeAgentError(writer, err)
			return
		}
		record, err := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workload.ID, mustJSON(map[string]any{"state": "deleted"}), server.auditInput(request, "workload.delete", "workload", workload.ID, map[string]any{"dispatched": true}))
		if err != nil {
			writeError(writer, http.StatusBadGateway, "WORKLOAD_RECONCILE_FAILED", err.Error())
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
			writeAgentError(writer, err)
			return
		}
		metadata, metadataErr := json.Marshal(manifest)
		if metadataErr != nil {
			writeError(writer, http.StatusInternalServerError, "CHECKPOINT_ENCODE_FAILED", metadataErr.Error())
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
			writeDatabaseErrorCheckpoint(writer, err)
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
			writeAgentError(writer, err)
			return
		}
		status := map[string]any{"state": string(result.State), "restore_id": result.ID}
		record, reconcileErr := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workload.ID, mustJSON(status), server.auditInput(request, "checkpoint.restore", "checkpoint", input.CheckpointID, map[string]any{"restore_id": result.ID}))
		if reconcileErr != nil {
			writeError(writer, http.StatusBadGateway, "RESTORE_RECONCILE_FAILED", reconcileErr.Error())
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
		agentRequest := agentclient.MigrationCreateRequest{
			WorkloadID: workload.ID,
			Destination: model.Destination{
				MachineID: destination.MachineID, AgentURL: destination.AgentURL,
				ServerName: serverNameFromAgentURL(destination.AgentURL),
			},
			Mode: mode, TimeoutSeconds: input.TimeoutSeconds,
		}
		result, err := client.CreateMigration(commandContext, agentRequest)
		if err != nil {
			writeAgentError(writer, err)
			return
		}
		id, idErr := model.NewID()
		if idErr != nil {
			writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", idErr.Error())
			return
		}
		principal := principalFrom(request.Context())
		_, jobErr := server.database.CreateMigrationForAgent(request.Context(), database.MigrationRecord{
			ID: id, OrganizationID: organizationID, WorkloadID: workload.ID,
			SourceMachineID: machineID, DestinationMachineID: destination.MachineID,
			Mode: input.Mode, CreatedBy: principal.UserID,
		}, server.auditInput(request, "migration.dispatch", "migration", id, map[string]any{"agent_migration_id": result.ID, "mode": input.Mode}))
		if jobErr != nil {
			writeError(writer, http.StatusBadGateway, "MIGRATION_RECONCILE_FAILED", jobErr.Error())
			return
		}
		progress, _ := json.Marshal(migrationProgress(result))
		sequence := int64(len(result.Events))
		if sequence == 0 {
			sequence = 1
		}
		eventErr := server.database.AddMigrationEvent(request.Context(), database.MigrationEventRecord{
			ID: result.ID + "-" + fmtInt(sequence), MigrationID: id, Sequence: sequence, Stage: string(result.Stage),
			Message: migrationStageMessage(result), Progress: lastEventProgress(result),
			BytesDone: lastBytesDone(result), BytesTotal: result.Metrics.TotalStateBytes,
		})
		if eventErr != nil {
			writeError(writer, http.StatusBadGateway, "MIGRATION_EVENT_RECONCILE_FAILED", eventErr.Error())
			return
		}
		jobRecord, jobErr := server.database.UpdateMigrationStatus(request.Context(), organizationID, id, migrationJobStatus(result.Stage), progress, result.FailureReason, server.auditInput(request, "migration.progress", "migration", id, nil))
		if jobErr != nil {
			writeError(writer, http.StatusBadGateway, "MIGRATION_STATUS_RECONCILE_FAILED", jobErr.Error())
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

func writeAgentError(writer http.ResponseWriter, err error) {
	var apiError *agentclient.APIError
	if errors.As(err, &apiError) {
		status := http.StatusBadGateway
		switch apiError.Status {
		case http.StatusBadRequest, http.StatusConflict, http.StatusForbidden, http.StatusNotFound:
			status = apiError.Status
		}
		writeError(writer, status, "AGENT_COMMAND_FAILED", apiError.Message)
		return
	}
	writeError(writer, http.StatusBadGateway, "AGENT_UNREACHABLE", err.Error())
}

func writeDatabaseErrorCheckpoint(writer http.ResponseWriter, err error) {
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
		writeError(writer, http.StatusBadGateway, "CHECKPOINT_RECONCILE_FAILED", err.Error())
	}
}

func fmtInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

func lastEventProgress(result model.Migration) float64 {
	if len(result.Events) == 0 {
		return 0
	}
	return result.Events[len(result.Events)-1].Progress
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage(`{"error":"encode failed"}`)
	}
	return encoded
}

func serverNameFromAgentURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return parsed.Hostname()
}

func migrationProgress(result model.Migration) map[string]any {
	progress := 0.0
	if len(result.Events) > 0 {
		progress = result.Events[len(result.Events)-1].Progress
	}
	if result.Stage == model.MigrationCompleted {
		progress = 1
	}
	return map[string]any{"stage": string(result.Stage), "progress": progress, "agent_id": result.ID}
}

func migrationStageMessage(result model.Migration) string {
	if len(result.Events) > 0 {
		return result.Events[len(result.Events)-1].Message
	}
	return "migration dispatched to source agent"
}

func lastBytesDone(result model.Migration) int64 {
	if len(result.Events) == 0 {
		return 0
	}
	return result.Events[len(result.Events)-1].BytesDone
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

func (server *Server) handleReconcile(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	machines, err := server.database.Machines(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", err.Error())
		return
	}
	reconciled := 0
	failed := make(map[string]string)
	for _, machine := range machines {
		if machine.AgentURL == "" || machine.Status == "disabled" {
			continue
		}
		client, err := agentclient.New(machine.AgentURL, 15*time.Second)
		if err != nil {
			failed[machine.MachineID] = err.Error()
			continue
		}
		workloads, err := client.Workloads(request.Context())
		if err != nil {
			failed[machine.MachineID] = err.Error()
			continue
		}
		for _, workload := range workloads {
			specEncoded, err := json.Marshal(workload.Spec)
			if err != nil {
				specEncoded = []byte(`{}`)
			}
			status := map[string]any{"state": string(workload.Status), "agent": workload}
			statusEncoded, _ := json.Marshal(status)
			record, err := server.database.UpsertWorkload(request.Context(), database.WorkloadRecord{
				ID: workload.Spec.ID, OrganizationID: organizationID, MachineID: machine.MachineID,
				Name: workload.Spec.Name, Spec: specEncoded, Status: statusEncoded,
			}, server.auditInput(request, "workload.reconcile", "workload", workload.Spec.ID, map[string]any{"machine_id": machine.MachineID}))
			if err != nil {
				failed[workload.Spec.ID] = err.Error()
				continue
			}
			_ = record
			reconciled++
			checkpoints, err := client.Checkpoints(request.Context(), workload.Spec.ID)
			if err != nil {
				failed["checkpoint:"+workload.Spec.ID] = err.Error()
				continue
			}
			for _, summary := range checkpoints {
				manifest, err := client.Checkpoint(request.Context(), summary.ID)
				if err != nil {
					failed["manifest:"+summary.ID] = err.Error()
					continue
				}
				manifestEncoded, _ := json.Marshal(manifest)
				if _, err := model.NewID(); err != nil {
					failed["id:"+summary.ID] = err.Error()
					continue
				}
				_, err = server.database.CreateCheckpointForAgent(request.Context(), database.CheckpointRecord{
					ID: manifest.ID, OrganizationID: organizationID, WorkloadID: manifest.Workload.ID,
					MachineID: manifest.SourceIdentity.ID, Kind: string(manifest.Kind), ParentID: manifest.ParentID,
					Manifest: manifestEncoded, PlainBytes: manifest.Metrics.PlainBytes,
					StoredBytes: manifest.Metrics.StoredBytes, ChunkCount: manifest.Metrics.ChunkCount,
				}, server.auditInput(request, "checkpoint.reconcile", "checkpoint", manifest.ID, nil))
				if err != nil && !database.IsConflict(err) {
					failed["checkpoint:"+summary.ID] = err.Error()
				}
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"reconciled_workloads": reconciled, "failed": failed})
}

func (server *Server) handleMigrationCancel(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	migrationID := request.PathValue("migrationID")
	migrations, err := server.database.Migrations(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MIGRATIONS_LOOKUP_FAILED", err.Error())
		return
	}
	var job *database.MigrationRecord
	for index := range migrations {
		if migrations[index].ID == migrationID {
			job = &migrations[index]
			break
		}
	}
	if job == nil {
		writeError(writer, http.StatusNotFound, "MIGRATION_NOT_FOUND", "migration was not found")
		return
	}
	if job.Status != "queued" && job.Status != "running" {
		writeError(writer, http.StatusConflict, "MIGRATION_NOT_ACTIVE", "migration is no longer active")
		return
	}
	var progress map[string]any
	if err := json.Unmarshal(job.Progress, &progress); err != nil {
		writeError(writer, http.StatusInternalServerError, "MIGRATION_PROGRESS_INVALID", err.Error())
		return
	}
	agentMigrationID, _ := progress["agent_id"].(string)
	if agentMigrationID == "" {
		writeError(writer, http.StatusConflict, "AGENT_MIGRATION_MISSING", "migration has not been assigned an agent identifier")
		return
	}
	sourceMachineID := job.SourceMachineID
	machines, err := server.database.Machines(request.Context(), organizationID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", err.Error())
		return
	}
	var source *database.MachineRecord
	for index := range machines {
		if machines[index].MachineID == sourceMachineID {
			source = &machines[index]
			break
		}
	}
	if source == nil || source.AgentURL == "" {
		writeError(writer, http.StatusConflict, "SOURCE_AGENT_UNAVAILABLE", "source machine or agent URL is unavailable")
		return
	}
	client, clientErr := agentclient.New(source.AgentURL, 30*time.Second)
	if clientErr != nil {
		writeError(writer, http.StatusBadRequest, "AGENT_ENDPOINT_INVALID", clientErr.Error())
		return
	}
	if err := client.CancelMigration(request.Context(), agentMigrationID); err != nil {
		writeAgentError(writer, err)
		return
	}
	record, err := server.database.UpdateMigrationStatus(request.Context(), organizationID, migrationID, "cancelled", mustJSON(map[string]any{"stage": "CANCELLED", "progress": 0}), "", server.auditInput(request, "migration.cancel", "migration", migrationID, nil)) //nolint:misspell // persisted status vocabulary
	if err != nil {
		writeError(writer, http.StatusBadGateway, "MIGRATION_RECONCILE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusAccepted, migrationResponse(record))
}
