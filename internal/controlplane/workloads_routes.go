package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleWorkloadList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.Workloads(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "WORKLOADS_LOOKUP_FAILED", err.Error())
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
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 200 || input.Spec == nil {
		writeError(writer, http.StatusBadRequest, "WORKLOAD_INVALID", "workload name and spec are required")
		return
	}
	if len(input.Spec) > 128 {
		writeError(writer, http.StatusBadRequest, "WORKLOAD_INVALID", "workload spec contains too many fields")
		return
	}
	status := input.Status
	if status == nil {
		status = map[string]any{"status": "registered"}
	}
	id, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	spec, _ := json.Marshal(input.Spec)
	statusJSON, _ := json.Marshal(status)
	record, err := server.database.CreateWorkload(request.Context(), database.WorkloadRecord{ID: id, OrganizationID: request.PathValue("organizationID"), MachineID: strings.TrimSpace(input.MachineID), Name: strings.TrimSpace(input.Name), Spec: spec, Status: statusJSON}, server.auditInput(request, "workload.create", "workload", id, nil))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "assigned machine was not found")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "WORKLOAD_EXISTS", "workload id already exists")
			return
		}
		writeError(writer, http.StatusInternalServerError, "WORKLOAD_CREATE_FAILED", err.Error())
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
	status, _ := json.Marshal(input)
	record, err := server.database.UpdateWorkloadStatus(request.Context(), request.PathValue("organizationID"), request.PathValue("workloadID"), status, server.auditInput(request, "workload.status_update", "workload", request.PathValue("workloadID"), input))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "WORKLOAD_STATUS_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, workloadResponse(record))
}
