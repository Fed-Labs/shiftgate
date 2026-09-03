package controlplane

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleAPIKeyList(writer http.ResponseWriter, request *http.Request) {
	records, err := server.database.APIKeys(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "API_KEYS_LOOKUP_FAILED", err.Error())
		return
	}
	result := make([]APIKey, 0, len(records))
	for _, record := range records {
		result = append(result, apiKeyResponse(record))
	}
	writeJSON(writer, http.StatusOK, result)
}

// handleAPIKeyCreate mints an organization API key. The secret is returned
// exactly once, in this response; only its peppered digest and a 16-character
// prefix (for recognition in listings) are stored.
func (server *Server) handleAPIKeyCreate(writer http.ResponseWriter, request *http.Request) {
	var input CreateAPIKeyRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 200 || !validScopes(input.Scopes) {
		writeError(writer, http.StatusBadRequest, "API_KEY_INVALID", "name and valid scopes are required")
		return
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now().UTC().Add(time.Hour)) {
		writeError(writer, http.StatusBadRequest, "API_KEY_EXPIRY_INVALID", "API key expiry must be more than one hour in the future")
		return
	}
	secret, _, err := newToken(apiKeyPrefix)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "TOKEN_GENERATION_FAILED", err.Error())
		return
	}
	id, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	scopes, _ := json.Marshal(input.Scopes)
	principal := principalFrom(request.Context())
	record, err := server.database.CreateAPIKey(request.Context(), database.APIKeyRecord{ID: id, OrganizationID: request.PathValue("organizationID"), UserID: principal.UserID, Name: input.Name, Prefix: secret[:minInt(len(secret), 16)], Scopes: scopes, ExpiresAt: input.ExpiresAt}, tokenDigest(server.config.TokenPepper, secret), server.auditInput(request, "api_key.create", "api_key", id, map[string]any{"scopes": input.Scopes}))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "API_KEY_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, APIKeyCreated{APIKey: apiKeyResponse(record), Secret: secret})
}

func (server *Server) handleAPIKeyRevoke(writer http.ResponseWriter, request *http.Request) {
	if err := server.database.RevokeAPIKey(request.Context(), request.PathValue("organizationID"), request.PathValue("keyID"), server.auditInput(request, "api_key.revoke", "api_key", request.PathValue("keyID"), nil)); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "API_KEY_NOT_FOUND", "API key was not found or already revoked")
			return
		}
		writeError(writer, http.StatusInternalServerError, "API_KEY_REVOKE_FAILED", err.Error())
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
