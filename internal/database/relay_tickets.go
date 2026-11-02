package database

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrRelayTicketReplay = errors.New("relay ticket was already consumed or revoked")

func RelayTokenHash(token string) []byte { digest := sha256.Sum256([]byte(token)); return digest[:] }

type RelayTicketRecord struct {
	ID                   string
	OrganizationID       string
	MigrationID          string
	SourceMachineID      string
	DestinationMachineID string
	Nonce                string
	TokenHash            []byte
	ExpiresAt            time.Time
	State                string
	ConsumedAt           *time.Time
	RelayID              string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func (store *Store) CreateRelayTicket(ctx context.Context, ticket RelayTicketRecord, audit AuditInput) (RelayTicketRecord, error) {
	if ticket.ID == "" || ticket.OrganizationID == "" || ticket.MigrationID == "" || ticket.SourceMachineID == "" || ticket.DestinationMachineID == "" || ticket.Nonce == "" || len(ticket.TokenHash) == 0 {
		return RelayTicketRecord{}, errors.New("relay ticket fields are required")
	}
	if !ticket.ExpiresAt.After(time.Now().UTC()) {
		return RelayTicketRecord{}, errors.New("relay ticket expiry must be in the future")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return RelayTicketRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `SELECT source_machine_id,destination_machine_id FROM migration_jobs WHERE id=$1 AND organization_id=$2`, ticket.MigrationID, ticket.OrganizationID).Scan(&ticket.SourceMachineID, &ticket.DestinationMachineID); err != nil {
		return RelayTicketRecord{}, err
	}
	ticket.State = "issued"
	ticket.CreatedAt = time.Now().UTC()
	ticket.UpdatedAt = ticket.CreatedAt
	err = tx.QueryRow(ctx, `INSERT INTO relay_tickets(id,organization_id,migration_id,source_machine_id,destination_machine_id,nonce,token_hash,expires_at,state,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) RETURNING created_at,updated_at`, ticket.ID, ticket.OrganizationID, ticket.MigrationID, ticket.SourceMachineID, ticket.DestinationMachineID, ticket.Nonce, ticket.TokenHash, ticket.ExpiresAt, ticket.State, ticket.CreatedAt).Scan(&ticket.CreatedAt, &ticket.UpdatedAt)
	if err != nil {
		return RelayTicketRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return RelayTicketRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RelayTicketRecord{}, err
	}
	return ticket, nil
}

func (store *Store) ClaimRelayLeg(ctx context.Context, org, migrationID, source, destination, nonce, token, machine, relayID string) error {
	if machine != source && machine != destination {
		return ErrRelayTicketReplay
	}
	column := "source_claimed_at"
	if machine == destination {
		column = "destination_claimed_at"
	}
	query := `UPDATE relay_tickets SET ` + column + `=COALESCE(` + column + `,now()), relay_id=$4, updated_at=now() WHERE nonce=$1 AND token_hash=$2 AND state='issued' AND expires_at>now() AND organization_id=$3 AND source_machine_id=$5 AND destination_machine_id=$6 AND migration_id=$7 AND (` + column + ` IS NULL OR ` + column + `>now()-interval '2 minutes')`
	result, err := store.pool.Exec(ctx, query, nonce, RelayTokenHash(token), org, relayID, source, destination, migrationID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrRelayTicketReplay
	}
	return nil
}

// ConsumeRelayTicket burns one ticket presentation at pair teardown: the
// ticket moves to consumed and can no longer be claimed, which is what makes
// a captured ticket one-use for its whole TTL. It is idempotent — consuming
// an already-consumed or revoked ticket reports ErrRelayTicketReplay without
// changing state.
func (store *Store) ConsumeRelayTicket(ctx context.Context, nonce string, token string, relayID string) error {
	if nonce == "" || token == "" || relayID == "" {
		return errors.New("relay consume fields are required")
	}
	var id string
	now := time.Now().UTC()
	err := store.pool.QueryRow(ctx, `UPDATE relay_tickets SET state='consumed',consumed_at=$4,relay_id=$3,updated_at=$4 WHERE nonce=$1 AND token_hash=$2 AND state='issued' AND expires_at>$4 RETURNING id`, nonce, RelayTokenHash(token), relayID, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRelayTicketReplay
	}
	return err
}

func (store *Store) RevokeExpiredRelayTickets(ctx context.Context) error {
	_, err := store.pool.Exec(ctx, `UPDATE relay_tickets SET state='expired',updated_at=now() WHERE state='issued' AND expires_at<=now()`)
	return err
}

// RevokeRelayTicketsForMigration expires every still-issued ticket of one
// migration: ticket reconciliation at job settlement, so no issued ticket
// outlives the job it was minted for.
func (store *Store) RevokeRelayTicketsForMigration(ctx context.Context, migrationID string) error {
	if migrationID == "" {
		return errors.New("migration id is required")
	}
	_, err := store.pool.Exec(ctx, `UPDATE relay_tickets SET state='expired',updated_at=now() WHERE migration_id=$1 AND state='issued'`, migrationID)
	return err
}

// RelayTicketsIssuedThisHour counts tickets issued in the current UTC hour —
// the 120/h abuse ceiling (plan 4 BR-13c).
func (store *Store) RelayTicketsIssuedThisHour(ctx context.Context, organizationID string, now time.Time) (int, error) {
	hour := now.UTC().Truncate(time.Hour)
	var count int
	err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM relay_tickets WHERE organization_id=$1 AND created_at >= $2`, organizationID, hour).Scan(&count)
	return count, err
}
