package database

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// Regression cover for relay ticket replay (ship finding 04 blocker 1): a
// captured ticket must be one-use for its whole TTL — pair teardown burns it,
// and no presentation after that (same machine or another) re-opens the leg.
// PostgreSQL-gated.
func TestRelayTicketIsOneUseAcrossClaimsAndConsume(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "relay-replay")

	machine := func(label, url string) string {
		record, err := store.CreateMachine(ctx, MachineRecord{
			ID: mustRegressionID(t), OrganizationID: organizationID, MachineID: label, Name: label,
			AgentURL: url, Capabilities: json.RawMessage(`{}`),
		}, testAudit(organizationID, "machine.create", label))
		if err != nil {
			t.Fatal(err)
		}
		return record.MachineID
	}
	source := machine("relay-source", "https://127.0.0.1:18443")
	destination := machine("relay-destination", "https://127.0.0.1:18444")

	workloadID := mustRegressionID(t)
	if _, err := store.UpsertWorkload(ctx, WorkloadRecord{
		ID: workloadID, OrganizationID: organizationID, MachineID: source,
		Name: "relay-workload", Spec: json.RawMessage(`{"name":"relay-workload"}`),
	}, testAudit(organizationID, "workload.upsert", workloadID)); err != nil {
		t.Fatal(err)
	}
	migration, _, err := store.CreateMigration(ctx, MigrationRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, WorkloadID: workloadID,
		SourceMachineID: source, DestinationMachineID: destination,
		Mode: "cold", CreatedBy: testOrganizationOwner(t, store, organizationID), Managed: false, TimeoutSeconds: 600,
	}, testAudit(organizationID, "migration.create", "relay"))
	if err != nil {
		t.Fatal(err)
	}

	nonce := "relay-nonce-" + migration.ID
	token := "relay-token-" + migration.ID
	if _, err := store.CreateRelayTicket(ctx, RelayTicketRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, MigrationID: migration.ID,
		SourceMachineID: source, DestinationMachineID: destination,
		Nonce: nonce, TokenHash: RelayTokenHash(token), ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}, testAudit(organizationID, "relay.ticket", nonce)); err != nil {
		t.Fatal(err)
	}

	// Both parties claim their legs with the captured ticket — the happy
	// pairing the relay service performs.
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce, token, source, "relay-1"); err != nil {
		t.Fatalf("source leg claim refused: %v", err)
	}
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce, token, destination, "relay-1"); err != nil {
		t.Fatalf("destination leg claim refused: %v", err)
	}

	// A machine that is not a party to the ticket can never claim with it.
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce, token, "intruder", "relay-1"); !errors.Is(err, ErrRelayTicketReplay) {
		t.Fatalf("non-party claim error = %v, want ErrRelayTicketReplay", err)
	}
	// A wrong token is a wrong ticket.
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce, "forged", source, "relay-1"); !errors.Is(err, ErrRelayTicketReplay) {
		t.Fatalf("forged-token claim error = %v, want ErrRelayTicketReplay", err)
	}

	// Pair teardown burns the ticket. From here the captured ticket is dead
	// for its whole remaining TTL — that is the replay the finding is about.
	if err := store.ConsumeRelayTicket(ctx, nonce, token, "relay-1"); err != nil {
		t.Fatalf("ticket consume refused: %v", err)
	}
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce, token, source, "relay-2"); !errors.Is(err, ErrRelayTicketReplay) {
		t.Fatalf("post-consume replay claim error = %v, want ErrRelayTicketReplay", err)
	}
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce, token, destination, "relay-2"); !errors.Is(err, ErrRelayTicketReplay) {
		t.Fatalf("post-consume destination replay error = %v, want ErrRelayTicketReplay", err)
	}
	if err := store.ConsumeRelayTicket(ctx, nonce, token, "relay-1"); !errors.Is(err, ErrRelayTicketReplay) {
		t.Fatalf("second consume error = %v, want ErrRelayTicketReplay", err)
	}

	// Settlement revocation: a ticket still in flight is expired with its job.
	nonce2 := "relay-nonce-2-" + migration.ID
	token2 := "relay-token-2-" + migration.ID
	if _, err := store.CreateRelayTicket(ctx, RelayTicketRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, MigrationID: migration.ID,
		SourceMachineID: source, DestinationMachineID: destination,
		Nonce: nonce2, TokenHash: RelayTokenHash(token2), ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}, testAudit(organizationID, "relay.ticket", nonce2)); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeRelayTicketsForMigration(ctx, migration.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimRelayLeg(ctx, organizationID, migration.ID, source, destination, nonce2, token2, source, "relay-1"); !errors.Is(err, ErrRelayTicketReplay) {
		t.Fatalf("revoked-ticket claim error = %v, want ErrRelayTicketReplay", err)
	}
}

// mustRegressionID mints a row id for the relay fixtures.
func mustRegressionID(t *testing.T) string {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// testOrganizationOwner returns the registering owner's user id — migration
// jobs reference a real user through created_by.
func testOrganizationOwner(t *testing.T, store *Store, organizationID string) string {
	t.Helper()
	var userID string
	if err := store.pool.QueryRow(context.Background(), `SELECT user_id FROM organization_members WHERE organization_id=$1 AND role='owner' LIMIT 1`, organizationID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	return userID
}
