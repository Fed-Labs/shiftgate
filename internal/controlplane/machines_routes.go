package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleMachineList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.Machines(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "MACHINES_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]Machine, 0, len(records))
	for _, record := range records {
		result = append(result, machineResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleMachineCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateMachineRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if err := validateMachineInput(input); err != nil {
		writeError(writer, http.StatusBadRequest, "MACHINE_INVALID", err.Error())
		return
	}
	id, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	capabilities, _ := json.Marshal(input.Capabilities)
	record, err := server.database.CreateMachine(request.Context(), database.MachineRecord{ID: id, OrganizationID: request.PathValue("organizationID"), MachineID: strings.TrimSpace(input.MachineID), Name: strings.TrimSpace(input.Name), AgentURL: strings.TrimRight(input.AgentURL, "/"), Capabilities: capabilities}, server.auditInput(request, "machine.create", "machine", id, map[string]any{"machine_id": input.MachineID}))
	if err != nil {
		if errors.Is(err, database.ErrEntitlementExceeded) {
			writeError(writer, http.StatusPaymentRequired, "MACHINE_ENTITLEMENT_EXCEEDED", "organization machine entitlement is exhausted")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "MACHINE_EXISTS", "machine is already registered")
			return
		}
		writeError(writer, http.StatusInternalServerError, "MACHINE_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, machineResponse(record))
}

func (server *Server) handleMachineHeartbeat(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Name         string         `json:"name"`
		AgentURL     string         `json:"agent_url"`
		Capabilities map[string]any `json:"capabilities"`
		Status       string         `json:"status"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if input.Status == "" {
		input.Status = "online"
	}
	if input.Status != "online" && input.Status != "offline" && input.Status != "draining" {
		writeError(writer, http.StatusBadRequest, "MACHINE_STATUS_INVALID", "unsupported machine status")
		return
	}
	var capabilities []byte
	if input.Capabilities != nil {
		capabilities, _ = json.Marshal(input.Capabilities)
	}
	record, err := server.database.UpdateMachineHeartbeat(request.Context(), request.PathValue("organizationID"), request.PathValue("machineID"), strings.TrimSpace(input.Name), strings.TrimRight(strings.TrimSpace(input.AgentURL), "/"), capabilities, input.Status)
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "machine was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "MACHINE_HEARTBEAT_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, machineResponse(record))
}

// validateMachineInput rejects machine registrations without a stable id, a
// name, or an https agent URL — the agent URL is what peers and the dispatcher
// dial, so a plain-http value would silently break both.
func validateMachineInput(input CreateMachineRequest) error {
	if strings.TrimSpace(input.MachineID) == "" || len(input.MachineID) > 256 || strings.TrimSpace(input.Name) == "" || len(input.Name) > 200 {
		return errors.New("machine id and name are required")
	}
	parsed, err := url.Parse(strings.TrimSpace(input.AgentURL))

	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("agent URL must be an https URL")
	}
	if input.Capabilities == nil {
		input.Capabilities = map[string]any{}
	}
	return nil
}

// handleMachineCapability answers a fleet query against the capability
// projection: which machines report this capability, filtered by kind and —
// for numbers — a minimum value. The kind is required because a capability
// name means different things in different documents, and guessing would
// answer a question the caller did not ask.
func (server *Server) handleMachineCapability(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	capability := strings.TrimSpace(query.Get("capability"))
	kind := strings.TrimSpace(query.Get("kind"))
	if capability == "" || len(capability) > 256 {
		writeError(writer, http.StatusBadRequest, "CAPABILITY_INVALID", "the capability query parameter is required")
		return
	}
	if kind != "number" && kind != "text" && kind != "boolean" {
		writeError(writer, http.StatusBadRequest, "CAPABILITY_KIND_INVALID", "kind must be number, text, or boolean")
		return
	}
	var minimum *float64
	if raw := strings.TrimSpace(query.Get("minimum")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || kind != "number" {
			writeError(writer, http.StatusBadRequest, "CAPABILITY_MINIMUM_INVALID", "minimum must be a number and is only valid with kind=number")
			return
		}
		minimum = &parsed
	}
	records, err := server.database.MachinesWithCapability(request.Context(), request.PathValue("organizationID"), capability, kind, minimum)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "CAPABILITY_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]Machine, 0, len(records))
	for _, record := range records {
		result = append(result, machineResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}
