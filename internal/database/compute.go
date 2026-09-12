package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ComputeOfferRecord is one machine's advertised resource offer. The JSON
// columns hold the scheduler's own value types verbatim so the control plane and
// the scheduler can never disagree about what an operator published.
type ComputeOfferRecord struct {
	ID               string
	OrganizationID   string
	MachineID        string
	MachineName      string
	AgentURL         string
	Visibility       string
	Status           string
	Trust            string
	IdentityVerified bool
	Region           string
	Country          string
	Exposed          json.RawMessage
	Pricing          json.RawMessage
	Geography        json.RawMessage
	Policy           json.RawMessage
	Availability     json.RawMessage
	Capabilities     json.RawMessage
	Latency          json.RawMessage
	LastSeenAt       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	WithdrawnAt      *time.Time
}

// ComputeReservationRecord is a real hold on an offer's capacity. Every
// reservation carries an expiry so capacity abandoned by a failed migration is
// reclaimed instead of leaking.
type ComputeReservationRecord struct {
	ID             string
	OfferID        string
	OrganizationID string
	MachineID      string
	WorkloadID     string
	MigrationID    string
	State          string
	Requested      json.RawMessage
	HourlyMicros   int64
	Currency       string
	ErrorMessage   string
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ReleasedAt     *time.Time
}

var (
	// ErrComputeCapacityUnavailable is returned when a reservation loses the race
	// for capacity a placement was calculated against.
	ErrComputeCapacityUnavailable = errors.New("offer no longer has enough free capacity for this reservation")
	// ErrComputeReservationTransition is returned when a caller asks for a state
	// change the reservation state machine forbids.
	ErrComputeReservationTransition = errors.New("reservation cannot move to the requested state")
)

const computeOfferColumns = `id,organization_id,machine_id,machine_name,agent_url,visibility,status,trust,identity_verified,` +
	`region,country,exposed,pricing,geography,policy,availability,capabilities,latency,last_seen_at,created_at,updated_at,withdrawn_at`

const computeReservationColumns = `id,offer_id,organization_id,machine_id,workload_id,migration_id,state,requested,` +
	`hourly_micros,currency,error_message,expires_at,created_at,updated_at,released_at`

func scanComputeOffer(row pgx.Row) (ComputeOfferRecord, error) {
	var record ComputeOfferRecord
	err := row.Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.MachineName, &record.AgentURL,
		&record.Visibility, &record.Status, &record.Trust, &record.IdentityVerified, &record.Region, &record.Country,
		&record.Exposed, &record.Pricing, &record.Geography, &record.Policy, &record.Availability, &record.Capabilities,
		&record.Latency, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt, &record.WithdrawnAt)
	return record, err
}

func scanComputeReservation(row pgx.Row) (ComputeReservationRecord, error) {
	var record ComputeReservationRecord
	err := row.Scan(&record.ID, &record.OfferID, &record.OrganizationID, &record.MachineID, &record.WorkloadID,
		&record.MigrationID, &record.State, &record.Requested, &record.HourlyMicros, &record.Currency,
		&record.ErrorMessage, &record.ExpiresAt, &record.CreatedAt, &record.UpdatedAt, &record.ReleasedAt)
	return record, err
}

func collectComputeOffers(rows pgx.Rows) ([]ComputeOfferRecord, error) {
	defer rows.Close()
	var result []ComputeOfferRecord
	for rows.Next() {
		record, err := scanComputeOffer(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func collectComputeReservations(rows pgx.Rows) ([]ComputeReservationRecord, error) {
	defer rows.Close()
	var result []ComputeReservationRecord
	for rows.Next() {
		record, err := scanComputeReservation(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func jsonOrDefault(value json.RawMessage, fallback string) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(fallback)
	}
	return value
}

// UpsertComputeOffer publishes or replaces the offer for one registered machine.
// Machine name, agent URL, capabilities, and presence are copied from the control
// plane's own machine registry rather than from the request, so a publisher
// cannot advertise hardware or availability it does not have.
func (store *Store) UpsertComputeOffer(ctx context.Context, record ComputeOfferRecord, audit AuditInput) (ComputeOfferRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ComputeOfferRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var (
		name         string
		agentURL     string
		capabilities json.RawMessage
		lastSeenAt   *time.Time
	)
	if err := tx.QueryRow(ctx, `SELECT name,agent_url,capabilities,last_seen_at FROM machines
		WHERE organization_id=$1 AND machine_id=$2 AND status<>'disabled'`, record.OrganizationID, record.MachineID).
		Scan(&name, &agentURL, &capabilities, &lastSeenAt); err != nil {
		return ComputeOfferRecord{}, err
	}
	record.MachineName = name
	record.AgentURL = agentURL
	record.Capabilities = jsonOrDefault(capabilities, "{}")
	record.LastSeenAt = lastSeenAt
	stored, err := scanComputeOffer(tx.QueryRow(ctx, `INSERT INTO compute_offers(id,organization_id,machine_id,machine_name,
		agent_url,visibility,status,trust,identity_verified,region,country,exposed,pricing,geography,policy,availability,
		capabilities,latency,last_seen_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (organization_id,machine_id) DO UPDATE SET machine_name=EXCLUDED.machine_name,
		agent_url=EXCLUDED.agent_url,visibility=EXCLUDED.visibility,status=EXCLUDED.status,trust=EXCLUDED.trust,
		identity_verified=EXCLUDED.identity_verified,region=EXCLUDED.region,country=EXCLUDED.country,
		exposed=EXCLUDED.exposed,pricing=EXCLUDED.pricing,geography=EXCLUDED.geography,policy=EXCLUDED.policy,
		availability=EXCLUDED.availability,capabilities=EXCLUDED.capabilities,latency=EXCLUDED.latency,
		last_seen_at=EXCLUDED.last_seen_at,withdrawn_at=NULL,updated_at=now()
		RETURNING `+computeOfferColumns,
		record.ID, record.OrganizationID, record.MachineID, record.MachineName, record.AgentURL, record.Visibility,
		record.Status, record.Trust, record.IdentityVerified, record.Region, record.Country,
		jsonOrDefault(record.Exposed, "{}"), jsonOrDefault(record.Pricing, "{}"), jsonOrDefault(record.Geography, "{}"),
		jsonOrDefault(record.Policy, "{}"), jsonOrDefault(record.Availability, "{}"), record.Capabilities,
		jsonOrDefault(record.Latency, "[]"), record.LastSeenAt))
	if err != nil {
		return ComputeOfferRecord{}, err
	}
	audit.ResourceID = stored.ID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return ComputeOfferRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ComputeOfferRecord{}, err
	}
	return stored, nil
}

// ComputeOffers returns every offer this organization has published, withdrawn
// ones included, so operators can see the whole history of what they exposed.
func (store *Store) ComputeOffers(ctx context.Context, organizationID string) ([]ComputeOfferRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT `+computeOfferColumns+` FROM compute_offers
		WHERE organization_id=$1 ORDER BY created_at`, organizationID)
	if err != nil {
		return nil, err
	}
	return collectComputeOffers(rows)
}

// SchedulableComputeOffers returns the inventory one organization is allowed to
// see: its own machines, partner offers that name it explicitly, and — only when
// an operator has opened public trading — the public marketplace. Deny lists and
// every other constraint are enforced by the scheduler, which remains the single
// source of placement truth; this query only narrows visibility.
func (store *Store) SchedulableComputeOffers(ctx context.Context, organizationID string, includePublic bool) ([]ComputeOfferRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT `+computeOfferColumns+` FROM compute_offers
		WHERE withdrawn_at IS NULL AND status<>'withdrawn' AND (
			organization_id=$1
			OR (visibility='organization' AND COALESCE(policy->'allowed_organizations','[]'::jsonb) @> to_jsonb($1::text))
			OR ($2::boolean AND visibility='public')
		) ORDER BY created_at`, organizationID, includePublic)
	if err != nil {
		return nil, err
	}
	return collectComputeOffers(rows)
}

// WithdrawComputeOffer removes an offer from scheduling. Live reservations are
// deliberately left alone: capacity already promised to a running workload must
// not vanish underneath it.
func (store *Store) WithdrawComputeOffer(ctx context.Context, organizationID, offerID string, audit AuditInput) (ComputeOfferRecord, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return ComputeOfferRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := scanComputeOffer(tx.QueryRow(ctx, `UPDATE compute_offers
		SET status='withdrawn',withdrawn_at=now(),updated_at=now()
		WHERE id=$1 AND organization_id=$2 RETURNING `+computeOfferColumns, offerID, organizationID))
	if err != nil {
		return ComputeOfferRecord{}, err
	}
	audit.ResourceID = stored.ID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return ComputeOfferRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ComputeOfferRecord{}, err
	}
	return stored, nil
}

// ExpireComputeReservations releases holds whose expiry has passed. Placement and
// reservation both run this first, so a crashed migration cannot pin capacity
// forever.
func (store *Store) ExpireComputeReservations(ctx context.Context, now time.Time) (int64, error) {
	command, err := store.pool.Exec(ctx, `UPDATE compute_reservations
		SET state='EXPIRED',released_at=COALESCE(released_at,now()),updated_at=now()
		WHERE state IN ('PENDING','ACTIVE') AND expires_at<=$1`, now)
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

// ComputeReservations lists an organization's reservations, newest first. An
// empty offerID returns every offer's reservations.
func (store *Store) ComputeReservations(ctx context.Context, organizationID, offerID string) ([]ComputeReservationRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT `+computeReservationColumns+` FROM compute_reservations
		WHERE organization_id=$1 AND ($2='' OR offer_id=$2) ORDER BY created_at DESC`, organizationID, offerID)
	if err != nil {
		return nil, err
	}
	return collectComputeReservations(rows)
}

// LiveComputeReservations returns the unexpired holds across a set of offers so
// the scheduler can subtract committed capacity before it ranks anything.
func (store *Store) LiveComputeReservations(ctx context.Context, offerIDs []string, now time.Time) ([]ComputeReservationRecord, error) {
	if len(offerIDs) == 0 {
		return nil, nil
	}
	rows, err := store.pool.Query(ctx, `SELECT `+computeReservationColumns+` FROM compute_reservations
		WHERE offer_id = ANY($1) AND state IN ('PENDING','ACTIVE') AND expires_at>$2 ORDER BY created_at`, offerIDs, now)
	if err != nil {
		return nil, err
	}
	return collectComputeReservations(rows)
}

// CreateComputeReservation commits capacity on one offer. The offer row is locked
// for the whole transaction and approve is handed the stored offer together with
// every live hold on it, so two concurrent placements can never both take the
// same device. approve owns the capacity arithmetic and fills in the fields that
// depend on it — machine, price, currency — while this function owns the
// serialization that makes the arithmetic trustworthy.
func (store *Store) CreateComputeReservation(ctx context.Context, record ComputeReservationRecord, approve func(ComputeOfferRecord, []ComputeReservationRecord, *ComputeReservationRecord) error, audit AuditInput) (ComputeReservationRecord, error) {
	if approve == nil {
		return ComputeReservationRecord{}, errors.New("a reservation approval check is required")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	offer, err := scanComputeOffer(tx.QueryRow(ctx, `SELECT `+computeOfferColumns+` FROM compute_offers
		WHERE id=$1 AND withdrawn_at IS NULL FOR UPDATE`, record.OfferID))
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE compute_reservations
		SET state='EXPIRED',released_at=COALESCE(released_at,now()),updated_at=now()
		WHERE offer_id=$1 AND state IN ('PENDING','ACTIVE') AND expires_at<=now()`, record.OfferID); err != nil {
		return ComputeReservationRecord{}, err
	}
	rows, err := tx.Query(ctx, `SELECT `+computeReservationColumns+` FROM compute_reservations
		WHERE offer_id=$1 AND state IN ('PENDING','ACTIVE') ORDER BY created_at`, record.OfferID)
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	live, err := collectComputeReservations(rows)
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	if err := approve(offer, live, &record); err != nil {
		return ComputeReservationRecord{}, err
	}
	stored, err := scanComputeReservation(tx.QueryRow(ctx, `INSERT INTO compute_reservations(id,offer_id,organization_id,
		machine_id,workload_id,migration_id,state,requested,hourly_micros,currency,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+computeReservationColumns,
		record.ID, record.OfferID, record.OrganizationID, record.MachineID, record.WorkloadID, record.MigrationID,
		record.State, jsonOrDefault(record.Requested, "{}"), record.HourlyMicros, record.Currency, record.ExpiresAt))
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	audit.ResourceID = stored.ID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return ComputeReservationRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ComputeReservationRecord{}, err
	}
	return stored, nil
}

// UpdateComputeReservationState moves one reservation through the reservation
// state machine. allow is the state machine itself: it receives the state that is
// actually stored, under a row lock, so a stale client cannot skip a transition
// or resurrect capacity that was already released.
func (store *Store) UpdateComputeReservationState(ctx context.Context, organizationID, reservationID, next, errorMessage string, allow func(current string) error, audit AuditInput) (ComputeReservationRecord, error) {
	if allow == nil {
		return ComputeReservationRecord{}, errors.New("a reservation transition check is required")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current string
	if err := tx.QueryRow(ctx, `SELECT state FROM compute_reservations
		WHERE id=$1 AND organization_id=$2 FOR UPDATE`, reservationID, organizationID).Scan(&current); err != nil {
		return ComputeReservationRecord{}, err
	}
	if err := allow(current); err != nil {
		return ComputeReservationRecord{}, err
	}
	stored, err := scanComputeReservation(tx.QueryRow(ctx, `UPDATE compute_reservations
		SET state=$3,error_message=$4,updated_at=now(),
		released_at=CASE WHEN $3 IN ('RELEASED','EXPIRED') THEN COALESCE(released_at,now()) ELSE released_at END
		WHERE id=$1 AND organization_id=$2 RETURNING `+computeReservationColumns,
		reservationID, organizationID, next, truncate(errorMessage, 2048)))
	if err != nil {
		return ComputeReservationRecord{}, err
	}
	audit.ResourceID = stored.ID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return ComputeReservationRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ComputeReservationRecord{}, err
	}
	return stored, nil
}
