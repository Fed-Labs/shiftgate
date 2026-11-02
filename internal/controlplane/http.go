package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// http.go carries request and response plumbing shared by every control-plane
// handler: bounded JSON decoding, the protocol-version header, error bodies,
// audit-record assembly, client-IP derivation behind trusted proxies, and the
// rate-limit key.

const controlBodyLimit = int64(2 << 20)

// auditInput assembles the audit record for a mutating request. It never
// fails a request: an ID-generation error degrades to a timestamped fallback
// rather than discarding the audit trail. The recorded remote address is the
// derived client IP, not the socket peer, so audit rows stay per-client behind
// the documented TLS reverse proxy.
func (server *Server) auditInput(request *http.Request, action, resourceType, resourceID string, metadata any) database.AuditInput {
	id, err := model.NewID()
	if err != nil {
		id = fmt.Sprintf("audit-%d", time.Now().UnixNano())
	}
	principal := principalFrom(request.Context())
	return database.AuditInput{
		ID: id, OrganizationID: request.PathValue("organizationID"), ActorUserID: principal.UserID,
		Action: action, ResourceType: resourceType, ResourceID: resourceID,
		Metadata: metadataOrEmpty(metadata), RequestID: requestID(request), RemoteAddr: server.clientIP(request),
	}
}

func requestID(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("X-Request-ID"))
	if !validRequestID(value) {
		return "unknown"
	}
	return value
}

// validRequestID accepts correlation ids that are safe to echo and to write
// to logs and audit rows: printable, unambiguous characters only, so a client
// cannot smuggle log line breaks or spoofed key=value pairs through the id.
func validRequestID(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
		default:
			return false
		}
	}
	return true
}

// idempotencyKey returns the caller-supplied retry key for a mutating request.
// Keys are opaque, bounded strings: they are persisted for lookup but never
// used as an authorization credential or echoed into logs.
func validIdempotencyKey(value string) bool {
	return len(value) <= 255 && !strings.ContainsAny(value, "\r\n")
}

func idempotencyKey(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.Header.Get("Idempotency-Key"))
	if value == "" {
		return "", true
	}
	return value, validIdempotencyKey(value)
}

// clientIP resolves the requesting client's address. Forwarding headers are
// honored only when the socket peer is a configured trusted proxy, and within
// X-Forwarded-For the rightmost hop that is not itself a trusted proxy is the
// client — so a client cannot forge its own address by sending the header
// directly.
func (server *Server) clientIP(request *http.Request) string {
	return resolveClientIP(request.RemoteAddr, request.Header.Values("X-Forwarded-For"), request.Header.Get("X-Real-IP"), server.isTrustedProxy)
}

// resolveClientIP is the pure client-derivation rule behind clientIP.
// forwardedFor holds every X-Forwarded-For header value in arrival order;
// trusted reports whether an IP belongs to the operator's proxy fleet.
func resolveClientIP(remoteAddr string, forwardedFor []string, realIP string, trusted func(net.IP) bool) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = strings.TrimSpace(remoteAddr)
	}
	peer := net.ParseIP(host)
	if peer == nil || trusted == nil || !trusted(peer) {
		return host
	}
	// X-Real-IP from a trusted hop is the platform edge's own statement of the
	// client's remote address (the platform's public networking documents the header
	// as exactly that), so it wins over the forwarding chain: the chain's
	// rightmost hop is the edge POP address — a CDN range that shifts by
	// region and request — and rightmost-untrusted selection would misreport
	// that POP as the client. A value that is itself a trusted proxy is an
	// overwrite-chain intermediate, not an end client; those fall through to
	// the chain walk below.
	if candidate := realIPHost(realIP); candidate != "" && !trusted(net.ParseIP(candidate)) {
		return candidate
	}
	hops := parseForwardedFor(forwardedFor)
	for index := len(hops) - 1; index >= 0; index-- {
		hop := net.ParseIP(hops[index])
		if hop == nil {
			// A malformed hop proves nothing about the client; stop trusting
			// the chain and fall back to the socket peer.
			return host
		}
		if trusted(hop) {
			continue
		}
		return hops[index]
	}
	if len(hops) > 0 {
		return hops[0]
	}
	return host
}

// realIPHost extracts a bare IP from an X-Real-IP value (with or without an
// optional port), or "" when the value is not an address. An unparseable
// header proves nothing and is ignored like any other malformed forwarding
// input.
func realIPHost(value string) string {
	candidate := strings.TrimSpace(value)
	if candidate == "" {
		return ""
	}
	if net.ParseIP(candidate) != nil {
		return candidate
	}
	if host, _, err := net.SplitHostPort(candidate); err == nil && net.ParseIP(host) != nil {
		return host
	}
	return ""
}

// parseForwardedFor flattens X-Forwarded-For header values into bare IP
// strings, tolerating optional ports and stray whitespace.
func parseForwardedFor(values []string) []string {
	var hops []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if host, _, err := net.SplitHostPort(part); err == nil {
				part = host
			}
			hops = append(hops, part)
		}
	}
	return hops
}

// rateLimitKey buckets requests by derived client IP and folds in a digest of
// the presented bearer credential. The credential limb is only honored once
// that credential has authenticated (the limiter tracks validated digests):
// an unauthenticated caller rotating forged Authorization headers always
// shares the per-IP bucket and cannot mint one bucket per forged value. The
// token itself is never used as the key — only a digest of it.
func (server *Server) rateLimitKey(request *http.Request) string {
	ipKey := "ip:" + server.clientIP(request)
	if token, ok := parseBearer(request.Header.Get("Authorization")); ok {
		return ipKey + "|token:" + rateLimitCredentialID(token)
	}
	return ipKey
}

func metadataOrEmpty(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

// pageFromRequest parses limit/offset query parameters into a bounded
// database page. Out-of-range values degrade to the server's own defaults and
// cap instead of failing the request.
func pageFromRequest(request *http.Request) database.Page {
	limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(request.URL.Query().Get("offset"))
	return database.NormalizePage(limit, offset)
}

// allCheckpoints pages through a workload's complete checkpoint list for
// capacity accounting, which must see every checkpoint, not one page of them.
func (server *Server) allCheckpoints(ctx context.Context, organizationID, workloadID string) ([]database.CheckpointRecord, error) {
	var all []database.CheckpointRecord
	for offset := 0; ; offset += database.MaxPageLimit {
		batch, err := server.database.Checkpoints(ctx, organizationID, workloadID, database.NormalizePage(database.MaxPageLimit, offset))
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < database.MaxPageLimit {
			return all, nil
		}
	}
}

// allMachines pages through an organization's complete machine list for
// internal jobs — dispatch, reconcile — that genuinely need every machine and
// must not silently truncate at a page boundary.
func (server *Server) allMachines(ctx context.Context, organizationID string) ([]database.MachineRecord, error) {
	var all []database.MachineRecord
	for offset := 0; ; offset += database.MaxPageLimit {
		batch, err := server.database.Machines(ctx, organizationID, database.NormalizePage(database.MaxPageLimit, offset))
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < database.MaxPageLimit {
			return all, nil
		}
	}
}

func parseMetadataInt(value string, fallback int64) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// decodeJSON decodes one JSON object into target, rejecting unknown fields,
// trailing data, and empty or null bodies so a typo'd request fails loudly
// instead of half-applying.
func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	request.Body = http.MaxBytesReader(writer, request.Body, controlBodyLimit)
	decoder := json.NewDecoder(request.Body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", "request contains trailing data")
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || !strings.HasPrefix(trimmed, "{") {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", "request body must be a JSON object")
		return false
	}
	inner := json.NewDecoder(bytes.NewReader(raw))
	inner.DisallowUnknownFields()
	if err := inner.Decode(target); err != nil {
		writeError(writer, http.StatusBadRequest, "INVALID_JSON", err.Error())
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
	// The observability middleware stamps the correlation id onto the
	// response before handlers run; carry it in the body too, so error
	// surfaces that read body.request_id can show clients the same id the
	// X-Request-ID header and the server logs carry.
	writeJSON(writer, status, ErrorResponse{Code: code, Message: message, RequestID: writer.Header().Get("X-Request-ID")})
}

// writeInternalError is the one exit for server-side failures: the underlying
// cause is logged with its request id and never leaves the process in the
// response body, which carries only the stable error code and the request id
// an operator can grep for. Client-visible validation messages keep using
// writeError directly.
func (server *Server) writeInternalError(writer http.ResponseWriter, request *http.Request, status int, code string, err error) {
	id := requestID(request)
	server.logger.Error("request failed", "request_id", id, "code", code, "status", status, "error", err)
	writeError(writer, status, code, "internal error; see server logs for request id "+id)
}
