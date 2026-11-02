package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

const maxPageOffset = 100000

func pageFromCappedRequest(request *http.Request) database.Page {
	page := pageFromRequest(request)
	if page.Offset > maxPageOffset {
		page.Offset = maxPageOffset
	}
	return page
}

func (server *Server) handleWorkloadList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.Workloads(request.Context(), request.PathValue("organizationID"), pageFromCappedRequest(request))
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "WORKLOADS_LOOKUP_FAILED", err)
		return
	}
	result := make([]Workload, 0, len(records))
	for _, record := range records {
		result = append(result, workloadResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleWorkloadCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateWorkloadRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 200 || input.Spec == nil {
		writeError(writer, http.StatusBadRequest, "WORKLOAD_INVALID", "workload name and spec are required")
		return
	}
	if len(input.Spec) > 128 {
		writeError(writer, http.StatusBadRequest, "WORKLOAD_INVALID", "workload spec contains too many fields")
		return
	}
	// Plan gates (plan 2 BR-07/BR-08): a periodic checkpoint policy is a
	// paid capability, and warm-standby failover is HA Pack / Business. Cold
	// migration and manual checkpoints stay free — this gates only the
	// schedule and the standby designation, never the funnel's basics.
	organizationID := request.PathValue("organizationID")
	if specHasField(input.Spec, "checkpoint_policy") {
		if !server.requirePlanFeature(writer, request, organizationID, billing.FeaturePeriodicPolicies) {
			server.recordRefusalEvent(request.Context(), organizationID, "workload_create", billing.PeriodicPolicyPlanRequiredCode)
			return
		}
	}
	if specHasField(input.Spec, "failover_policy") {
		if !server.requirePlanFeature(writer, request, organizationID, billing.FeatureFailover) {
			server.recordRefusalEvent(request.Context(), organizationID, "workload_create", billing.FailoverPlanRequiredCode)
			return
		}
	}
	status := input.Status
	if status == nil {
		status = map[string]any{"state": "registered", "status": "registered"}
	}
	status = unifyWorkloadStatusKeys(status)
	id, err := model.NewID()
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", err)
		return
	}
	spec, _ := json.Marshal(input.Spec)
	statusJSON, _ := json.Marshal(status)
	record, err := server.database.CreateWorkload(request.Context(), database.WorkloadRecord{ID: id, OrganizationID: organizationID, MachineID: strings.TrimSpace(input.MachineID), Name: name, Spec: spec, Status: statusJSON}, server.auditInput(request, "workload.create", "workload", id, nil))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "assigned machine was not found")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "WORKLOAD_EXISTS", "workload id already exists")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "WORKLOAD_CREATE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusCreated, workloadResponse(record))
}

func (server *Server) handleWorkloadStatus(writer http.ResponseWriter, request *http.Request) {
	var input map[string]any
	if !decodeJSON(writer, request, &input) {
		return
	}
	if len(input) == 0 {
		writeError(writer, http.StatusBadRequest, "STATUS_INVALID", "status payload cannot be empty")
		return
	}
	organizationID := request.PathValue("organizationID")
	workloadID := request.PathValue("workloadID")
	active, err := server.hasActiveMigration(request.Context(), organizationID, workloadID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "WORKLOAD_STATUS_FAILED", err)
		return
	}
	if active {
		writeError(writer, http.StatusConflict, "MIGRATION_ACTIVE", "workload has an active migration")
		return
	}
	status, _ := json.Marshal(unifyWorkloadStatusKeys(input))
	record, err := server.database.UpdateWorkloadStatus(request.Context(), organizationID, workloadID, status, server.auditInput(request, "workload.status_update", "workload", workloadID, input))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "WORKLOAD_STATUS_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, workloadResponse(record))
}

func unifyWorkloadStatusKeys(status map[string]any) map[string]any {
	if status == nil {
		return map[string]any{}
	}
	value, ok := workloadStateValue(status)
	if !ok {
		return status
	}
	if _, ok := status["state"].(string); !ok {
		status["state"] = value
	}
	if _, ok := status["status"].(string); !ok {
		status["status"] = value
	}
	return status
}

func workloadStateValue(status map[string]any) (string, bool) {
	for _, key := range []string{"state", "status"} {
		if value, ok := status[key].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return value, true
			}
		}
	}
	return "", false
}

func workloadStatusDeleted(status json.RawMessage) bool {
	var document map[string]any
	if err := json.Unmarshal(status, &document); err != nil {
		return false
	}
	for _, key := range []string{"state", "status"} {
		if value, ok := document[key].(string); ok && strings.EqualFold(strings.TrimSpace(value), "deleted") {
			return true
		}
	}
	return false
}

// specHasField reports whether the loose workload-spec object carries a
// non-null field with this name — the plan gates read the raw spec map
// rather than a typed decode, because the spec is an open schema.
func specHasField(spec map[string]any, field string) bool {
	value, ok := spec[field]
	if !ok || value == nil {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		return len(typed) > 0
	case bool:
		return typed
	default:
		return true
	}
}
