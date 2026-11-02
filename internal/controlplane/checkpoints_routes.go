package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleCheckpointList(writer http.ResponseWriter, request *http.Request) {
	organizationID := request.PathValue("organizationID")
	includeDeleted := request.URL.Query().Get("include_deleted") == "true"
	records, err := server.database.Checkpoints(request.Context(), organizationID, request.URL.Query().Get("workload_id"), pageFromCappedRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "CHECKPOINTS_LOOKUP_FAILED", err)
		return
	}
	result := make([]Checkpoint, 0, len(records))
	for _, record := range records {
		if !includeDeleted && (record.Status == "deleted" || record.DeletedAt != nil) {
			continue
		}
		result = append(result, checkpointRecordResponse(organizationID, record))
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
	if !validCheckpointID(input.ID) || (input.ParentID != "" && !validCheckpointID(input.ParentID)) {
		writeError(writer, http.StatusBadRequest, "CHECKPOINT_INVALID", "checkpoint ids must be 1 to 128 characters of letters, digits, dot, underscore, or dash")
		return
	}
	manifest, err := publicManifestJSON(input.ManifestOrEmpty())
	if err != nil {
		writeError(writer, http.StatusBadRequest, "CHECKPOINT_MANIFEST_INVALID", "manifest is not a checkpoint manifest")
		return
	}
	organizationID := request.PathValue("organizationID")
	existing, err := server.allCheckpoints(request.Context(), organizationID, input.WorkloadID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "CHECKPOINTS_LOOKUP_FAILED", err)
		return
	}
	storedID := checkpointStorageID(organizationID, input.ID)
	storedParentID := ""
	if input.ParentID != "" {
		storedParentID = checkpointStorageID(organizationID, input.ParentID)
	}
	for _, record := range existing {
		if checkpointClientID(organizationID, record.ID) == input.ID {
			writeError(writer, http.StatusConflict, "CHECKPOINT_EXISTS", "checkpoint metadata already exists")
			return
		}
		if input.ParentID != "" && checkpointClientID(organizationID, record.ID) == input.ParentID {
			storedParentID = record.ID
		}
	}
	audit := server.auditInput(request, "checkpoint.register", "checkpoint", input.ID, map[string]any{"stored_bytes": input.StoredBytes})
	checkpointInput := database.CheckpointRecord{ID: storedID, OrganizationID: organizationID, WorkloadID: input.WorkloadID, MachineID: input.MachineID, Kind: input.Kind, ParentID: storedParentID, Manifest: manifest, PlainBytes: input.PlainBytes, StoredBytes: input.StoredBytes, ChunkCount: input.ChunkCount}
	record, err := server.database.CreateCheckpoint(request.Context(), checkpointInput, audit)
	if errors.Is(err, database.ErrCheckpointMachineMismatch) && server.settleWorkloadAssignment(request.Context(), organizationID, input.WorkloadID) {
		record, err = server.database.CreateCheckpoint(request.Context(), checkpointInput, audit)
	}
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
		// Everything else is storage trouble, not caller input: a database
		// outage must read as one, never as a validation error with raw SQL
		// text attached.
		server.writeInternalError(writer, request, http.StatusInternalServerError, "CHECKPOINT_CREATE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusCreated, checkpointRecordResponse(organizationID, record))
}

const checkpointIDPattern = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

func validCheckpointID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	if strings.Contains(value, "..") {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune(checkpointIDPattern, char) {
			return false
		}
	}
	return true
}

func checkpointStorageID(organizationID, id string) string {
	return organizationID + ":" + id
}

func checkpointClientID(organizationID, id string) string {
	return strings.TrimPrefix(id, organizationID+":")
}

func checkpointRecordResponse(organizationID string, record database.CheckpointRecord) Checkpoint {
	response := checkpointResponse(record)
	response.ID = checkpointClientID(organizationID, record.ID)
	if response.ParentID != "" {
		response.ParentID = checkpointClientID(organizationID, record.ParentID)
	}
	return response
}

// publicManifestJSON projects a registration manifest to the redacted form
// the control plane is allowed to retain. The plaintext side of a manifest —
// environment values (routinely credentials), command arguments, declared
// paths, health-check details, unkeyed plaintext chunk digests — is stripped
// before anything is written; keyed chunk addresses, sizes, signer, engine
// facts, and the device needs placement reads all survive. Agents are
// expected to export the projection already; this is the enforcement stop so
// one misbehaving agent cannot persist secrets in control-plane metadata.
func publicManifestJSON(manifest map[string]any) (json.RawMessage, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	var typed model.CheckpointManifest
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return nil, err
	}
	return json.Marshal(typed.Public())
}
