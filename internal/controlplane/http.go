package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// http.go carries request and response plumbing shared by every control-plane
// handler: bounded JSON decoding, the protocol-version header, error bodies,
// audit-record assembly, and the rate-limit key.

const controlBodyLimit = int64(2 << 20)

// auditInput assembles the audit record for a mutating request. It never
// fails a request: an ID-generation error degrades to a timestamped fallback
// rather than discarding the audit trail.
func (server *Server) auditInput(request *http.Request, action, resourceType, resourceID string, metadata any) database.AuditInput {
	id, err := model.NewID()
	if err != nil {
		id = fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	principal := principalFrom(request.Context())
	return database.AuditInput{
		ID: id, OrganizationID: request.PathValue("organizationID"), ActorUserID: principal.UserID,
		Action: action, ResourceType: resourceType, ResourceID: resourceID,
		Metadata: metadataOrEmpty(metadata), RequestID: requestID(request), RemoteAddr: request.RemoteAddr,
	}
}

func requestID(request *http.Request) string {
	value := request.Header.Get("X-Request-ID")
	if value == "" || len(value) > 128 {
		return "unknown"
	}
	return value
}

// rateLimitKey buckets requests per credential when one is presented and per
// client IP otherwise. The token itself is never used as the key — only a
// digest of it.
func rateLimitKey(request *http.Request) string {
	if token, ok := parseBearer(request.Header.Get("Authorization")); ok {
		digest := tokenDigest("rate-limit-key", token)
		return fmt.Sprintf("token:%x", digest[:8])
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return "ip:" + host
	}
	return "ip:" + request.RemoteAddr
}

func metadataOrEmpty(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func parseMetadataInt(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// decodeJSON decodes one JSON object into target, rejecting unknown fields
// and trailing data so a typo'd request fails loudly instead of half-applying.
func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, controlBodyLimit)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", "request contains trailing data")
		return false
	}
	return true
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	// Every response states the protocol version this control plane speaks, so an
	// agent deciding whether to install an update knows what it has to remain
	// able to talk to.
	writer.Header().Set(model.ProtocolVersionHeader, strconv.Itoa(model.ProtocolVersion))
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, ErrorResponse{Code: code, Message: message})
}
