package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/relay"
)

// Relay setup failures separate caller mistakes (unconfigured relay, bad
// pair, bad TTL) from infrastructure trouble so handlers can answer with the
// status the caller can act on.
var (
	errRelayNotConfigured = errors.New("relay is not configured")
	errRelayPairMismatch  = errors.New("relay pair does not match an active migration")
	errRelayTTLInvalid    = errors.New("ttl_seconds must be between 10 and 600")
)

func (server *Server) issueRelayForMigration(ctx context.Context, audit database.AuditInput, organizationID, source, destination, migrationID string, ttl int) (RelayTicketResponse, error) {
	if strings.TrimSpace(server.config.RelaySigningKey) == "" || strings.TrimSpace(server.config.RelayEndpoint) == "" {
		return RelayTicketResponse{}, errRelayNotConfigured
	}
	if ttl == 0 {
		ttl = 120
	}
	if ttl < 10 || ttl > 600 {
		return RelayTicketResponse{}, errRelayTTLInvalid
	}
	migration, err := server.database.Migration(ctx, organizationID, migrationID)
	if err != nil {
		return RelayTicketResponse{}, fmt.Errorf("migration not found: %w", err)
	}
	if migration.SourceMachineID != source || migration.DestinationMachineID != destination || migration.Status == "completed" || migration.Status == "failed" || migration.Status == "cancelled" {
		return RelayTicketResponse{}, errRelayPairMismatch
	}
	claims, err := relay.NewClaims(organizationID, source, destination, migrationID, time.Duration(ttl)*time.Second)
	if err != nil {
		return RelayTicketResponse{}, err
	}
	// The ticket is bound to the relay endpoint that will honor it, so a
	// captured ticket cannot be replayed at a different relay deployment.
	claims.Audience = strings.TrimSpace(server.config.RelayEndpoint)
	token, err := (relay.Signer{Key: []byte(server.config.RelaySigningKey)}).Sign(claims)
	if err != nil {
		return RelayTicketResponse{}, err
	}
	ticketID, err := model.NewID()
	if err != nil {
		return RelayTicketResponse{}, err
	}
	if _, err := server.database.CreateRelayTicket(ctx, database.RelayTicketRecord{ID: ticketID, OrganizationID: organizationID, MigrationID: migrationID, SourceMachineID: source, DestinationMachineID: destination, Nonce: claims.Nonce, TokenHash: database.RelayTokenHash(token), ExpiresAt: time.Unix(claims.ExpiresUnix, 0).UTC()}, audit); err != nil {
		return RelayTicketResponse{}, err
	}
	commandID, err := model.NewID()
	if err != nil {
		return RelayTicketResponse{}, err
	}
	payload, err := json.Marshal(map[string]any{"ticket": token, "endpoint": server.config.RelayEndpoint, "source_machine_id": source, "destination_machine_id": destination, "migration_id": migrationID, "expires_at": time.Unix(claims.ExpiresUnix, 0).UTC()})
	if err != nil {
		return RelayTicketResponse{}, fmt.Errorf("encode relay join command: %w", err)
	}
	commandAudit := audit
	if commandAudit.ID, err = model.NewID(); err != nil {
		return RelayTicketResponse{}, err
	}
	if err := server.database.EnqueueAgentCommand(ctx, database.AgentCommandRecord{ID: commandID, OrganizationID: organizationID, MachineID: destination, Action: "relay.join", Payload: payload}, commandAudit); err != nil {
		return RelayTicketResponse{}, err
	}
	// A ticket issued means the migration is live: follow it to a terminal
	// state even though relay-route jobs are not managed by the worker.
	server.watchUnmanagedMigration(organizationID, migrationID)
	return RelayTicketResponse{Endpoint: server.config.RelayEndpoint, Token: token, ExpiresAt: time.Unix(claims.ExpiresUnix, 0).UTC(), OrganizationID: organizationID, SourceMachineID: source, DestinationMachineID: destination, MigrationID: migrationID}, nil
}

func (server *Server) handleRelayTicket(writer http.ResponseWriter, request *http.Request) {
	var input RelayTicketRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	org := request.PathValue("organizationID")
	// Abuse velocity (plan 4 BR-13c): 120 relay tickets/hour per org.
	if !server.checkRelayTicketRate(writer, request, org) {
		return
	}
	// Fair-use throttle (plan 4 BR-01): fail-closed at ticket issuance — the
	// platform-mediated transfer surface. Peer-to-peer legs are metered
	// elsewhere and are outside platform enforcement.
	if !server.enforceTransferBudget(writer, request, org) {
		server.recordRefusalEvent(request.Context(), org, "relay_ticket", billing.TransferFairUseExceededCode)
		return
	}
	result, err := server.issueRelayForMigration(request.Context(), server.auditInput(request, "relay.ticket.issue", "migration", input.MigrationID, map[string]any{"source_machine_id": input.SourceMachineID, "destination_machine_id": input.DestinationMachineID}), org, input.SourceMachineID, input.DestinationMachineID, input.MigrationID, input.TTLSeconds)
	if err != nil {
		status, code, message := relayFailureResponse(err)
		if status >= 500 {
			// relayFailureResponse maps every failure to a stable, cause-free
			// message — that message is the response, even for 5xx. The
			// wrapped cause goes to the log only; and an unconfigured relay is
			// a deployment state (503) the caller can act on, which must not
			// read as "internal error".
			server.logger.Error("relay ticket issuance failed", "request_id", requestID(request), "code", code, "error", err)
		}
		writeError(writer, status, code, message)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

// reconcileRelayTicketUsage is the ticket reconciliation hook for a settled
// relay migration: lapsed tickets for the job are expired so no issued ticket
// outlives its migration. Relayed bandwidth is deliberately NOT metered here:
// the relay server counts the session (both directions) into usage_records at
// close, and that is the single authoritative vantage point — metering again
// from the settle hook or from the agent bridges would double-bill every
// relayed byte.
func (server *Server) reconcileRelayTicketUsage(ctx context.Context, organizationID, migrationID string, relayedBytes int64) {
	_ = relayedBytes
	if migrationID != "" {
		if err := server.database.RevokeRelayTicketsForMigration(ctx, migrationID); err != nil {
			server.logger.Warn("relay ticket reconciliation failed", "migration_id", migrationID, "error", err)
		}
	}
}
