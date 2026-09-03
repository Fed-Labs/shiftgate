package controlplane

import (
	"errors"
	"net/http"
	"strconv"

	"shift.dev/shift/internal/database"
)

func (server *Server) handleOrganizations(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	organizations, err := server.database.Organizations(request.Context(), principal.UserID)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ORGANIZATIONS_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]Organization, 0, len(organizations))
	for _, value := range organizations {
		if principal.APIKeyID != "" && value.ID != principal.OrganizationID {
			continue
		}
		result = append(result, organizationResponse(value))
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleMemberAdd(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Email string `json:"email"`
		Role  Role   `json:"role"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil || !input.Role.Valid() || input.Role == RoleOwner {
		writeError(writer, http.StatusBadRequest, "MEMBER_INVALID", "a valid non-owner member email and role are required")
		return
	}
	if err := server.database.AddMember(request.Context(), request.PathValue("organizationID"), email, string(input.Role), server.auditInput(request, "organization.member_upsert", "organization_member", email, map[string]any{"role": input.Role})); err != nil {
		if errors.Is(err, database.ErrOwnerRoleImmutable) {
			writeError(writer, http.StatusConflict, "OWNER_ROLE_IMMUTABLE", err.Error())
			return
		}
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "USER_NOT_FOUND", "user account was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "MEMBER_UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "updated"})
}

func (server *Server) handleAuditList(writer http.ResponseWriter, request *http.Request) {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	records, err := server.database.AuditEvents(request.Context(), request.PathValue("organizationID"), limit)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "AUDIT_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]AuditEvent, 0, len(records))
	for _, record := range records {
		result = append(result, auditResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}
