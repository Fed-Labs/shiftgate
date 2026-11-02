package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// capabilities.go: the relational projection of each machine's JSONB
// capabilities document. The JSONB column stays the source of truth for the
// full report; machine_capabilities mirrors the scalar leaves (cpu_count,
// memory_bytes, cuda_version, supports_live_migration, ...) into typed rows so
// fleet queries filter through an index instead of a JSONB scan. The
// projection is rebuilt inside the same transaction that writes the document,
// so the two can never disagree for long — and never disagree at rest.

// capabilityRow is one scalar leaf.
type capabilityRow struct {
	Capability string
	Kind       string // number, text, boolean
	Number     float64
	Text       string
	Boolean    bool
}

// classifyCapability puts one scalar into its typed row. Objects and arrays
// stay in the JSONB document only — flattening them into synthetic rows would
// invent a schema nobody agreed to.
func classifyCapability(name string, value any) (capabilityRow, bool) {
	switch typed := value.(type) {
	case float64:
		return capabilityRow{Capability: name, Kind: "number", Number: typed}, true
	case string:
		return capabilityRow{Capability: name, Kind: "text", Text: typed}, true
	case bool:
		return capabilityRow{Capability: name, Kind: "boolean", Boolean: typed}, true
	default:
		return capabilityRow{}, false
	}
}

// syncCapabilities deletes the machine's projection and reinserts it from the
// document, inside the caller's transaction. Delete-then-insert rather than a
// diff: capability documents are small, and a diff that misses a removal
// would leave stale rows answering fleet queries forever.
func syncCapabilities(ctx context.Context, tx pgx.Tx, machineRecordID string, document json.RawMessage) error {
	if _, err := tx.Exec(ctx, `DELETE FROM machine_capabilities WHERE machine_record_id=$1`, machineRecordID); err != nil {
		return err
	}
	if len(document) == 0 {
		return nil
	}
	var flat map[string]any
	if err := json.Unmarshal(document, &flat); err != nil {
		// The machines table guarantees valid JSONB, so this is a caller
		// passing something it did not read from the database. Refuse rather
		// than project half a document.
		return fmt.Errorf("decode capabilities of machine %s: %w", machineRecordID, err)
	}
	for name, value := range flat {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		row, ok := classifyCapability(name, value)
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO machine_capabilities(machine_record_id,capability,kind,number_value,text_value,boolean_value,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,now())`, machineRecordID, row.Capability, row.Kind, row.Number, row.Text, row.Boolean); err != nil {
			return err
		}
	}
	return nil
}

// MachinesWithCapability answers fleet queries through the projection: machines
// whose text capability equals a value, whose boolean capability is set, or
// whose numeric capability meets a minimum (nil minimum = any value). It
// returns full records — callers already want names and status, not just ids.
func (store *Store) MachinesWithCapability(ctx context.Context, organizationID, capability, kind string, minimum *float64, page Page) ([]MachineRecord, error) {
	var query string
	var args []any
	switch kind {
	case "number":
		query = `SELECT m.id,m.organization_id,m.machine_id,m.name,m.agent_url,m.capabilities,` + effectiveMachineStatusSQL("m") + `,m.last_seen_at,m.created_at,m.updated_at
			FROM machines m JOIN machine_capabilities c ON c.machine_record_id=m.id
			WHERE m.organization_id=$1 AND c.capability=$2 AND c.kind='number'`
		args = []any{organizationID, capability}
		if minimum != nil {
			query += ` AND c.number_value >= $3`
			args = append(args, *minimum)
		}
		query += ` ORDER BY c.number_value DESC, m.name`
	case "text":
		query = `SELECT m.id,m.organization_id,m.machine_id,m.name,m.agent_url,m.capabilities,` + effectiveMachineStatusSQL("m") + `,m.last_seen_at,m.created_at,m.updated_at
			FROM machines m JOIN machine_capabilities c ON c.machine_record_id=m.id
			WHERE m.organization_id=$1 AND c.capability=$2 AND c.kind='text' ORDER BY m.name`
		args = []any{organizationID, capability}
	case "boolean":
		query = `SELECT m.id,m.organization_id,m.machine_id,m.name,m.agent_url,m.capabilities,` + effectiveMachineStatusSQL("m") + `,m.last_seen_at,m.created_at,m.updated_at
			FROM machines m JOIN machine_capabilities c ON c.machine_record_id=m.id
			WHERE m.organization_id=$1 AND c.capability=$2 AND c.kind='boolean' AND c.boolean_value ORDER BY m.name`
		args = []any{organizationID, capability}
	default:
		return nil, fmt.Errorf("capability kind must be number, text, or boolean, not %q", kind)
	}
	rows, err := store.pool.Query(ctx, query+page.clause(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []MachineRecord
	for rows.Next() {
		var record MachineRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// Retention shares this file's transaction habit: every policy change lands
// with its audit row or not at all, and enforcement is set-based so a sweep
// costs one statement per class of record, not one per row.

// RetentionPolicy is an organization's retention windows in days. Zero means
// keep forever — for audit logs that is usually wrong, so the API surface
// documents the default rather than inventing one silently.
type RetentionPolicy struct {
	OrganizationID              string
	AuditRetentionDays          int
	CheckpointRetentionDays     int
	DeletedStorageRetentionDays int
	UpdatedAt                   time.Time
}

// RetentionPolicy returns one organization's policy, or the defaults when none
// was configured.
func (store *Store) RetentionPolicy(ctx context.Context, organizationID string) (RetentionPolicy, error) {
	var policy RetentionPolicy
	err := store.pool.QueryRow(ctx, `SELECT organization_id,audit_retention_days,checkpoint_retention_days,deleted_storage_retention_days,updated_at
		FROM retention_policies WHERE organization_id=$1`, organizationID).
		Scan(&policy.OrganizationID, &policy.AuditRetentionDays, &policy.CheckpointRetentionDays, &policy.DeletedStorageRetentionDays, &policy.UpdatedAt)
	if IsNotFound(err) {
		return RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 365, CheckpointRetentionDays: 90, DeletedStorageRetentionDays: 30}, nil
	}
	return policy, err
}

// SetRetentionPolicy writes an organization's policy. Zero days means keep
// forever, matching the column constraint, and the change takes effect on the
// next enforcement pass — there is no backdated mass delete.
func (store *Store) SetRetentionPolicy(ctx context.Context, policy RetentionPolicy, audit AuditInput) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO retention_policies(organization_id,audit_retention_days,checkpoint_retention_days,deleted_storage_retention_days,updated_at)
		VALUES($1,$2,$3,$4,now())
		ON CONFLICT (organization_id) DO UPDATE SET audit_retention_days=$2,checkpoint_retention_days=$3,deleted_storage_retention_days=$4,updated_at=now()`,
		policy.OrganizationID, policy.AuditRetentionDays, policy.CheckpointRetentionDays, policy.DeletedStorageRetentionDays); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetentionSweep is what one enforcement pass deleted or marked.
type RetentionSweep struct {
	AuditEventsDeleted    int64
	CheckpointsMarked     int64
	StorageObjectsPurged  int64
	SessionsPurged        int64
	APIKeysPurged         int64
	MigrationJobsPurged   int64
	AgentCommandsPurged   int64
	RelayTicketsPurged    int64
	RelayNoncesPurged     int64
	StripeEventsPurged    int64
	UsageRecordsPurged    int64
	ReservationsExpired   int64
	ReservationsPurged    int64
	MachinesMarkedOffline int64
}

// housekeepingLockKey serializes the housekeeping pass across replicas: two
// replicas sweeping the same tables on the same tick would double the WAL
// churn for no benefit. Distinct from the migration lock.
const housekeepingLockKey int64 = migrationLockKey + 1

// housekeepingBatch bounds each delete transaction. A first sweep after a long
// backlog must not open one multi-gigabyte transaction: batches keep WAL
// spikes, replication lag, and vacuum debt bounded.
const housekeepingBatch = 1000

// EnforceRetention applies every organization's policy once and runs the
// fixed-lifetime housekeeping for tables that outlive any policy. Organizations
// with no retention_policies row are held to the documented defaults (audit
// 365 days, checkpoints 90 days, deleted storage 30 days) — the windows the
// API advertises as the active policy are the windows that actually run.
// Audit rows older than their window are deleted, checkpoints older than their
// window are marked deleted, storage objects already soft-deleted past their
// grace window are purged, and sessions, API keys, terminal migration jobs
// (with their events), consumed agent commands, and expired relay tickets are
// removed on their fixed retention clocks so no table grows without bound.
// Checkpoint marking never touches a workload's newest live checkpoint, nor
// any checkpoint a surviving checkpoint still hangs off as a parent — a
// retention sweep must never delete the last remaining copy of a workload's
// checkpoint or break the incremental chain a restore depends on. Expired
// compute reservations release here too — expiry must not wait for an
// unrelated request to stumble over them — and machines whose presence has
// gone stale are marked offline so the fleet view cannot lie. Every delete is
// batched, and the pass takes an advisory lock so only one replica sweeps per
// tick.
func (store *Store) EnforceRetention(ctx context.Context) (RetentionSweep, error) {
	var sweep RetentionSweep
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		return sweep, err
	}
	defer conn.Release()
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, housekeepingLockKey).Scan(&acquired); err != nil {
		return sweep, err
	}
	if !acquired {
		// Another replica is mid-sweep; skipping is the point of the lock.
		return sweep, nil
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, housekeepingLockKey) }()

	// Audit rows past their window. Orgs with no policy row get the
	// documented 365-day default so the advertised window is enforced.
	deleted, err := runInBatches(ctx, conn, `DELETE FROM audit_events WHERE id IN (
		SELECT a.id FROM audit_events a
		LEFT JOIN retention_policies p ON p.organization_id = a.organization_id
		WHERE COALESCE(p.audit_retention_days, 365) > 0
		  AND a.created_at < now() - make_interval(days => COALESCE(p.audit_retention_days, 365))
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.AuditEventsDeleted = deleted
	// Checkpoints past their window: mark deleted, once. Batched like the
	// deletes so a first sweep with a huge backlog cannot open one
	// multi-gigabyte update; the rows stay for lineage and their objects for
	// the storage-object grace window to purge. The keep-newest guard is the
	// data-loss line: every workload's newest live checkpoint is excluded
	// from eligibility no matter how old, and so is every checkpoint a
	// surviving checkpoint still references as a parent (recursively) —
	// incremental chains stay restorable and no workload is ever left without
	// a restore point. Orgs with no policy row get the documented 90-day
	// default, which the guard makes safe to enforce.
	tag, err := runInBatches(ctx, conn, `UPDATE checkpoints SET status='deleted', deleted_at=COALESCE(deleted_at,now())
		WHERE id IN (
			WITH RECURSIVE newest_per_workload AS (
				SELECT DISTINCT ON (c.organization_id, c.workload_id) c.id, c.parent_id
				FROM checkpoints c
				WHERE c.status IN ('available','creating')
				ORDER BY c.organization_id, c.workload_id, c.created_at DESC, c.id DESC
			), protected AS (
				SELECT id, parent_id FROM newest_per_workload
				UNION
				SELECT c.id, c.parent_id FROM checkpoints c
				JOIN protected pr ON c.id = pr.parent_id
			)
			SELECT c.id FROM checkpoints c
			LEFT JOIN retention_policies p ON p.organization_id = c.organization_id
			WHERE COALESCE(p.checkpoint_retention_days, 90) > 0
			  AND c.status IN ('available','creating')
			  AND c.created_at < now() - make_interval(days => COALESCE(p.checkpoint_retention_days, 90))
			  AND c.id NOT IN (SELECT id FROM protected)
			LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.CheckpointsMarked = tag
	// Storage objects soft-deleted past their grace window. Orgs with no
	// policy row get the documented 30-day grace default.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM storage_objects WHERE id IN (
		SELECT o.id FROM storage_objects o
		LEFT JOIN retention_policies p ON p.organization_id = o.organization_id
		WHERE COALESCE(p.deleted_storage_retention_days, 30) > 0
		  AND o.state='deleted'
		  AND o.deleted_at < now() - make_interval(days => COALESCE(p.deleted_storage_retention_days, 30))
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.StorageObjectsPurged = deleted
	// Sessions: revoked rows, and rows whose refresh lifetime ended more than
	// 30 days ago. Live sessions are never swept regardless of access-token
	// age — refresh is what keeps them alive.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM sessions WHERE id IN (
		SELECT id FROM sessions
		WHERE (revoked_at IS NOT NULL AND revoked_at < now() - interval '30 days')
		   OR (refresh_expires_at < now() - interval '30 days')
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.SessionsPurged = deleted
	// API keys: revoked or expired for 90 days. The 90-day grace keeps
	// LastUsedAt investigations possible after a key is retired.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM api_keys WHERE id IN (
		SELECT id FROM api_keys
		WHERE (revoked_at IS NOT NULL AND revoked_at < now() - interval '90 days')
		   OR (expires_at IS NOT NULL AND expires_at < now() - interval '90 days')
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.APIKeysPurged = deleted
	// Terminal migration jobs older than 90 days go with their events
	// (ON DELETE CASCADE). Queued and running jobs are never swept.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM migration_jobs WHERE id IN (
		SELECT id FROM migration_jobs
		WHERE status IN ('completed','failed','cancelled')
		  AND COALESCE(completed_at, updated_at) < now() - interval '90 days'
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.MigrationJobsPurged = deleted
	// Agent commands are one-shot instructions: once consumed or failed for
	// 30 days they carry nothing an operator still needs, and a queued or
	// claimed command past its expires_at must never execute — the sweep is
	// what retires those too.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM agent_commands WHERE id IN (
		SELECT id FROM agent_commands
		WHERE (status IN ('done','failed')
		       AND COALESCE(completed_at, created_at) < now() - interval '30 days')
		   OR (status IN ('queued','claimed')
		       AND expires_at IS NOT NULL AND expires_at < now())
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.AgentCommandsPurged = deleted
	// Relay tickets expire within minutes by design; 30 days after any sign
	// of life they are pure history.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM relay_tickets WHERE id IN (
		SELECT id FROM relay_tickets
		WHERE expires_at < now() - interval '30 days'
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.RelayTicketsPurged = deleted
	// Relay request nonces are one-use replay guards: past their expiry they
	// can never authorize a request again, so they leave immediately.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM relay_request_nonces WHERE (machine_id,nonce) IN (
		SELECT machine_id,nonce FROM relay_request_nonces
		WHERE expires_at < now()
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.RelayNoncesPurged = deleted
	// Stripe webhook deliveries are dedup evidence first and history second:
	// 90 days after processing (or receipt, when never processed) an event
	// can no longer be redelivered into the window and is purgeable.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM stripe_events WHERE event_id IN (
		SELECT event_id FROM stripe_events
		WHERE COALESCE(processed_at, received_at) < now() - interval '90 days'
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.StripeEventsPurged = deleted
	// Usage records are billing evidence first and history second: every row
	// older than one full billing period (35 days) is purgeable, so metering
	// can be append-only without unbounded growth. Rows inside the window are
	// never touched — a current-period invoice dispute always has its rows.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM usage_records WHERE id IN (
		SELECT id FROM usage_records
		WHERE created_at < now() - interval '35 days'
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.UsageRecordsPurged = deleted
	// Compute reservations whose hold has lapsed release capacity here, not
	// when some unrelated request happens past.
	expired, err := store.ExpireComputeReservations(ctx, time.Now().UTC())
	if err != nil {
		return sweep, err
	}
	sweep.ReservationsExpired = expired
	// Terminal reservations 90 days past release are billing history, not
	// live state: without this sweep every migration's holds accumulate
	// forever.
	deleted, err = runInBatches(ctx, conn, `DELETE FROM compute_reservations WHERE id IN (
		SELECT id FROM compute_reservations
		WHERE state IN ('RELEASED','EXPIRED','FAILED')
		  AND COALESCE(released_at, updated_at) < now() - interval '90 days'
		LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.ReservationsPurged = deleted
	// Machine presence: a self-reported "online" whose heartbeat stopped more
	// than two minutes ago is offline in every view, and the row is corrected
	// so queries and machine checks agree.
	tag, err = runInBatches(ctx, conn, `UPDATE machines SET status='offline', updated_at=now()
		WHERE id IN (
			SELECT id FROM machines
			WHERE status='online' AND COALESCE(last_seen_at, updated_at) < now() - interval '2 minutes'
			LIMIT `+batchLimitSQL()+`)`)
	if err != nil {
		return sweep, err
	}
	sweep.MachinesMarkedOffline = tag
	return sweep, nil
}

// batchLimitSQL renders the batch size into a statement that cannot take a
// bind parameter (LIMIT inside a delete-target subquery).
func batchLimitSQL() string {
	return fmt.Sprintf("%d", housekeepingBatch)
}

// runInBatches runs a self-bounded delete or update statement repeatedly
// until it stops matching rows, returning the total rows changed. The
// statement must carry its own LIMIT subselect so each round stays bounded.
func runInBatches(ctx context.Context, conn *pgxpool.Conn, statement string) (int64, error) {
	var total int64
	for {
		tag, err := conn.Exec(ctx, statement)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < housekeepingBatch {
			return total, nil
		}
	}
}
