package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"

	"github.com/jackc/pgx/v5"
)

// relay_routes_test.go pins the relay-setup failure contract: an unconfigured
// relay is a typed deployment state (503 with a caller-actionable message),
// and every mapped failure carries a stable, cause-free message — never the
// generic "internal error" text and never the wrapped cause.

func TestRelayTicketFailsClosedWithoutConfiguration(t *testing.T) {
	server := New(config.DefaultControlPlane(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := server.issueRelayForMigration(context.Background(), database.AuditInput{}, "org_1", "m1", "m2", "mig_1", 0)
	if !errors.Is(err, errRelayNotConfigured) {
		t.Fatalf("unconfigured relay error = %v, want errRelayNotConfigured", err)
	}
	status, code, message := relayFailureResponse(err)
	if status != http.StatusServiceUnavailable || code != "RELAY_NOT_CONFIGURED" {
		t.Fatalf("unconfigured relay mapping = %d %s, want 503 RELAY_NOT_CONFIGURED", status, code)
	}
	if message != "relay transport is not configured on this control plane" {
		t.Fatalf("unconfigured relay message = %q, want the stable contract message", message)
	}
	if strings.Contains(message, "internal error") {
		t.Fatal("a deployment state must not read as an internal error")
	}
}

// The signing key alone (no endpoint) is still "not configured": tickets must
// stay fail-closed until both halves of the relay wiring are present.
func TestRelayTicketFailsClosedWithSigningKeyButNoEndpoint(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.RelaySigningKey = strings.Repeat("relay-signing-key-", 4)
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := server.issueRelayForMigration(context.Background(), database.AuditInput{}, "org_1", "m1", "m2", "mig_1", 0)
	if !errors.Is(err, errRelayNotConfigured) {
		t.Fatalf("half-configured relay error = %v, want errRelayNotConfigured", err)
	}
}

// relayFailureResponse's contract is a stable, cause-free message for every
// failure it maps — clients and the OpenAPI document both rely on it.
func TestRelayFailureResponseMessagesAreStable(t *testing.T) {
	for _, test := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{errRelayNotConfigured, http.StatusServiceUnavailable, "RELAY_NOT_CONFIGURED", "relay transport is not configured on this control plane"},
		{errRelayPairMismatch, http.StatusConflict, "RELAY_TICKET_FAILED", "relay pair does not match an active migration"},
		{errRelayTTLInvalid, http.StatusBadRequest, "RELAY_TICKET_FAILED", "ttl_seconds must be between 10 and 600"},
		{fmt.Errorf("migration not found: %w", pgx.ErrNoRows), http.StatusNotFound, "MIGRATION_NOT_FOUND", "migration was not found"},
		{errors.New("connection reset by peer at 10.1.2.3:5432"), http.StatusBadGateway, "RELAY_SETUP_FAILED", "relay setup failed; the migration was settled as failed"},
	} {
		status, code, message := relayFailureResponse(test.err)
		if status != test.status || code != test.code || message != test.message {
			t.Fatalf("relayFailureResponse(%v) = %d %s %q, want %d %s %q", test.err, status, code, message, test.status, test.code, test.message)
		}
	}
}
