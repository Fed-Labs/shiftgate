package controlplane

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleOwnershipAcquire(writer http.ResponseWriter, request *http.Request) {
	var input AcquireOwnershipRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.WorkloadID = strings.TrimSpace(input.WorkloadID)
	input.MachineID = strings.TrimSpace(input.MachineID)
	if input.WorkloadID == "" || input.MachineID == "" || input.LeaseSeconds < 10 || input.LeaseSeconds > 10*60 {
		writeError(writer, http.StatusBadRequest, "OWNERSHIP_INVALID", "workload_id, machine_id, and a lease between 10 and 600 seconds are required")
		return
	}
	principal := principalFrom(request.Context())
	organizationID := request.PathValue("organizationID")
	if _, err := server.database.Machine(request.Context(), organizationID, input.MachineID); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "MACHINE_NOT_FOUND", "machine is not registered in the organization")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "OWNERSHIP_LOOKUP_FAILED", err)
		return
	}
	if _, err := server.database.Workload(request.Context(), organizationID, input.WorkloadID); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload is not registered in the organization")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "OWNERSHIP_LOOKUP_FAILED", err)
		return
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "OWNERSHIP_TOKEN_FAILED", err)
		return
	}
	token := "shift_own_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
	auditID, err := model.NewID()
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", err)
		return
	}
	record, err := server.database.AcquireOwnership(request.Context(), organizationID, input.WorkloadID, input.MachineID, token, time.Duration(input.LeaseSeconds)*time.Second, database.AuditInput{ID: auditID, OrganizationID: organizationID, ActorUserID: principal.UserID, Action: "ownership.acquire", ResourceType: "workload", ResourceID: input.WorkloadID, Metadata: map[string]any{"machine_id": input.MachineID}})
	if err != nil {
		switch {
		case errors.Is(err, database.ErrOwnershipConflict):
			writeError(writer, http.StatusConflict, "OWNERSHIP_CONFLICT", err.Error())
		case database.IsNotFound(err):
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload is not registered in the organization")
		default:
			server.writeInternalError(writer, request, http.StatusInternalServerError, "OWNERSHIP_ACQUIRE_FAILED", err)
		}
		return
	}
	writeJSON(writer, http.StatusOK, Ownership{WorkloadID: record.WorkloadID, OrganizationID: record.OrganizationID, Epoch: record.Epoch, OwnerMachineID: record.OwnerMachineID, State: record.State, LeaseExpiresAt: record.LeaseExpiresAt, Token: token})
}

func (server *Server) handleOwnershipFence(writer http.ResponseWriter, request *http.Request) {
	var input FenceOwnershipRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if strings.TrimSpace(input.WorkloadID) == "" || strings.TrimSpace(input.MachineID) == "" || input.Epoch == 0 || strings.TrimSpace(input.Token) == "" {
		writeError(writer, http.StatusBadRequest, "OWNERSHIP_INVALID", "workload_id, machine_id, epoch, and token are required")
		return
	}
	organizationID := request.PathValue("organizationID")
	if _, err := server.database.Workload(request.Context(), organizationID, input.WorkloadID); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "WORKLOAD_NOT_FOUND", "workload is not registered in the organization")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "OWNERSHIP_FENCE_FAILED", err)
		return
	}
	if err := server.database.MarkOwnershipFenced(request.Context(), organizationID, input.WorkloadID, input.MachineID, input.Token, input.Epoch); err != nil {
		if errors.Is(err, database.ErrOwnershipConflict) {
			writeError(writer, http.StatusConflict, "OWNERSHIP_CONFLICT", err.Error())
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "OWNERSHIP_FENCE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"state": "fenced", "epoch": input.Epoch})
}
