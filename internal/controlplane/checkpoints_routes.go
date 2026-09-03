package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"shift.dev/shift/internal/database"
)

func (server *Server) handleCheckpointList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.Checkpoints(request.Context(), request.PathValue("organizationID"), request.URL.Query().Get("workload_id"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "CHECKPOINTS_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]Checkpoint, 0, len(records))
	for _, record := range records {
		result = append(result, checkpointResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleCheckpointCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateCheckpointRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Kind = strings.ToLower(strings.TrimSpace(input.Kind))
	if input.ID == "" || input.WorkloadID == "" || input.MachineID == "" || (input.Kind != "full" && input.Kind != "incremental") || input.PlainBytes < 0 || input.StoredBytes < 0 || input.ChunkCount < 0 || (input.Kind == "incremental") != (input.ParentID != "") {
		writeError(writer, http.StatusBadRequest, "CHECKPOINT_INVALID", "checkpoint id, workload, machine, kind, and non-negative metrics are required")
		return
	}
	manifest, _ := json.Marshal(input.ManifestOrEmpty())
	record, err := server.database.CreateCheckpoint(request.Context(), database.CheckpointRecord{ID: input.ID, OrganizationID: request.PathValue("organizationID"), WorkloadID: input.WorkloadID, MachineID: input.MachineID, Kind: input.Kind, ParentID: input.ParentID, Manifest: manifest, PlainBytes: input.PlainBytes, StoredBytes: input.StoredBytes, ChunkCount: input.ChunkCount}, server.auditInput(request, "checkpoint.register", "checkpoint", input.ID, map[string]any{"stored_bytes": input.StoredBytes}))
	if err != nil {
		if errors.Is(err, database.ErrStorageEntitlementExceeded) {
			writeError(writer, http.StatusPaymentRequired, "CHECKPOINT_STORAGE_ENTITLEMENT_EXCEEDED", err.Error())
			return
		}
		if errors.Is(err, database.ErrCheckpointMachineMismatch) {
			writeError(writer, http.StatusConflict, "CHECKPOINT_MACHINE_MISMATCH", err.Error())
			return
		}
		if errors.Is(err, database.ErrCheckpointParentInvalid) {
			writeError(writer, http.StatusUnprocessableEntity, "CHECKPOINT_PARENT_INVALID", err.Error())
			return
		}
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "CHECKPOINT_RESOURCE_NOT_FOUND", "workload or machine was not found")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "CHECKPOINT_EXISTS", "checkpoint metadata already exists")
			return
		}
		writeError(writer, http.StatusBadRequest, "CHECKPOINT_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, checkpointResponse(record))
}
