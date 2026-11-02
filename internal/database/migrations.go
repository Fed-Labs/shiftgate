package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"shift.dev/shift/internal/persistence"

	"github.com/jackc/pgx/v5"
)

// ErrMigrationSourceMismatch reports a migration request against a workload
// assigned to a different machine — a caller mistake, not an outage.
var ErrMigrationSourceMismatch = errors.New("workload is not assigned to the requested source machine")

// ErrMigrationMachineMissing reports a migration whose source/destination
// machine pair is not registered in the organization — a validation failure
// the API answers as not-found, distinct from a missing migration row.
var ErrMigrationMachineMissing = fmt.Errorf("source and destination machines must both be registered: %w", pgx.ErrNoRows)

// ErrMigrationAlreadyActive reports a second migration request for a workload
// that already has one queued or running. Two live migrations of one workload
// split its state between machines; the second must wait for the first to
// settle.
var ErrMigrationAlreadyActive = errors.New("workload already has an active migration")

const migrationColumns = `id,organization_id,workload_id,source_machine_id,destination_machine_id,mode,COALESCE(route,''),COALESCE(relay_endpoint,''),status,progress,error_message,created_by,
	COALESCE(idempotency_key,''),attempt_count,COALESCE(lease_owner,''),lease_expires_at,COALESCE(agent_migration_id,''),managed,cancel_requested,timeout_seconds,created_at,updated_at,completed_at`
const migrationColumnsQualified = `job.id,job.organization_id,job.workload_id,job.source_machine_id,job.destination_machine_id,job.mode,COALESCE(job.route,''),COALESCE(job.relay_endpoint,''),job.status,job.progress,job.error_message,job.created_by,
	COALESCE(job.idempotency_key,''),job.attempt_count,COALESCE(job.lease_owner,''),job.lease_expires_at,COALESCE(job.agent_migration_id,''),job.managed,job.cancel_requested,job.timeout_seconds,job.created_at,job.updated_at,job.completed_at`

func scanMigration(row pgx.Row) (MigrationRecord, error) {
	var record MigrationRecord
	err := row.Scan(&record.ID, &record.OrganizationID, &record.WorkloadID, &record.SourceMachineID, &record.DestinationMachineID,
		&record.Mode, &record.Route, &record.RelayEndpoint, &record.Status, &record.Progress, &record.ErrorMessage, &record.CreatedBy, &record.IdempotencyKey,
		&record.AttemptCount, &record.LeaseOwner, &record.LeaseExpiresAt, &record.AgentMigrationID, &record.Managed,
		&record.CancelRequested, &record.TimeoutSeconds, &record.CreatedAt, &record.UpdatedAt, &record.CompletedAt)
	return record, err
}

func sameMigrationRequest(a, b MigrationRecord) bool {
	return a.WorkloadID == b.WorkloadID && a.SourceMachineID == b.SourceMachineID &&
		a.DestinationMachineID == b.DestinationMachineID && a.Mode == b.Mode && a.TimeoutSeconds == b.TimeoutSeconds
}

func (store *Store) CreateMigration(ctx context.Context, record MigrationRecord, audit AuditInput) (MigrationRecord, bool, error) {
	// Lock before reading under READ COMMITTED so simultaneous retries see the
	// winner's commit. A serializable snapshot taken before waiting would not.
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return MigrationRecord{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if record.IdempotencyKey != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, record.OrganizationID+":"+record.IdempotencyKey); err != nil {
			return MigrationRecord{}, false, err
		}
		existing, err := scanMigration(tx.QueryRow(ctx, `SELECT `+migrationColumns+` FROM migration_jobs WHERE organization_id=$1 AND idempotency_key=$2`, record.OrganizationID, record.IdempotencyKey))
		if err == nil {
			if !sameMigrationRequest(existing, record) {
				return MigrationRecord{}, false, ErrIdempotencyConflict
			}
			return existing, false, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return MigrationRecord{}, false, err
		}
	}
	var workloadMachine string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(machine_id,'') FROM workloads WHERE id=$1 AND organization_id=$2 FOR UPDATE`, record.WorkloadID, record.OrganizationID).Scan(&workloadMachine); err != nil {
		return MigrationRecord{}, false, err
	}
	if workloadMachine != "" && workloadMachine != record.SourceMachineID {
		return MigrationRecord{}, false, ErrMigrationSourceMismatch
	}
	// One migration per workload at a time, checked under the workload row
	// lock so two concurrent creates cannot both see "no active job".
	var activeCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM migration_jobs WHERE workload_id=$1 AND organization_id=$2 AND status NOT IN ('completed','failed','cancelled')`, record.WorkloadID, record.OrganizationID).Scan(&activeCount); err != nil {
		return MigrationRecord{}, false, err
	}
	if activeCount > 0 {
		return MigrationRecord{}, false, ErrMigrationAlreadyActive
	}
	var machineCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM machines WHERE organization_id=$1 AND machine_id=ANY($2) AND status IN ('online','offline','draining')`, record.OrganizationID, []string{record.SourceMachineID, record.DestinationMachineID}).Scan(&machineCount); err != nil {
		return MigrationRecord{}, false, err
	}
	if machineCount != 2 {
		return MigrationRecord{}, false, ErrMigrationMachineMissing
	}
	record.Status = "queued"
	record.Progress = json.RawMessage(`{"stage":"QUEUED","progress":0}`)
	err = tx.QueryRow(ctx, `INSERT INTO migration_jobs(id,organization_id,workload_id,source_machine_id,destination_machine_id,mode,route,relay_endpoint,status,progress,created_by,idempotency_key,managed,timeout_seconds)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,''),$13,$14) RETURNING created_at,updated_at`, record.ID, record.OrganizationID, record.WorkloadID,
		record.SourceMachineID, record.DestinationMachineID, record.Mode, record.Route, record.RelayEndpoint, record.Status, record.Progress, record.CreatedBy, record.IdempotencyKey, record.Managed, record.TimeoutSeconds).
		Scan(&record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return MigrationRecord{}, false, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return MigrationRecord{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrationRecord{}, false, err
	}
	return record, true, nil
}

func (store *Store) Migration(ctx context.Context, organizationID, id string) (MigrationRecord, error) {
	return scanMigration(store.pool.QueryRow(ctx, `SELECT `+migrationColumns+` FROM migration_jobs WHERE organization_id=$1 AND id=$2`, organizationID, id))
}

func (store *Store) Migrations(ctx context.Context, organizationID string, page Page) ([]MigrationRecord, error) {
	query := `SELECT ` + migrationColumns + ` FROM migration_jobs WHERE organization_id=$1 ORDER BY created_at DESC` + page.clause()
	rows, err := store.pool.Query(ctx, query, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MigrationRecord
	for rows.Next() {
		record, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) CreateMigrationForAgent(ctx context.Context, record MigrationRecord, audit AuditInput) (MigrationRecord, error) {
	record.Managed = false
	created, _, err := store.CreateMigration(ctx, record, audit)
	return created, err
}

func (store *Store) UpdateMigrationStatus(ctx context.Context, organizationID, migrationID, status string, progress json.RawMessage, errorMessage string, audit AuditInput) (MigrationRecord, error) {
	switch status {
	case "queued", "running", "completed", "failed", "cancelled": //nolint:misspell // persisted status vocabulary
	default:
		return MigrationRecord{}, fmt.Errorf("unsupported migration status %q", status)
	}
	if len(progress) == 0 {
		progress = json.RawMessage(`{}`)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return MigrationRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	record, err := scanMigration(tx.QueryRow(ctx, `UPDATE migration_jobs SET status=$3,progress=$4,error_message=$5,
		result=CASE WHEN $3 IN ('completed','failed','cancelled') THEN COALESCE(result,'{}'::jsonb) || jsonb_build_object('duration_ms', GREATEST(0, (EXTRACT(EPOCH FROM (now()-created_at))*1000)::bigint)) ELSE result END,
		completed_at=CASE WHEN $3 IN ('completed','failed','cancelled') THEN now() ELSE completed_at END,updated_at=now()
		WHERE organization_id=$1 AND id=$2 RETURNING `+migrationColumns, organizationID, migrationID, status, progress, errorMessage))
	if err != nil {
		return MigrationRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return MigrationRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrationRecord{}, err
	}
	return record, nil
}

func (store *Store) AddMigrationEvent(ctx context.Context, record MigrationEventRecord) error {
	_, err := store.pool.Exec(ctx, `INSERT INTO migration_events(id,migration_id,sequence,stage,message,progress,bytes_done,bytes_total,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,COALESCE($9,now())) ON CONFLICT (migration_id,sequence) DO NOTHING`, record.ID, record.MigrationID, record.Sequence, record.Stage, record.Message, record.Progress, record.BytesDone, record.BytesTotal, record.CreatedAt)
	return err
}

func (store *Store) MigrationEvents(ctx context.Context, organizationID, migrationID string, page Page) ([]MigrationEventRecord, error) {
	query := `SELECT e.id,e.migration_id,e.sequence,e.stage,e.message,e.progress,e.bytes_done,e.bytes_total,e.created_at
		FROM migration_events e JOIN migration_jobs j ON j.id=e.migration_id WHERE j.organization_id=$1 AND e.migration_id=$2 ORDER BY e.sequence` + page.clause()
	rows, err := store.pool.Query(ctx, query, organizationID, migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MigrationEventRecord
	for rows.Next() {
		var record MigrationEventRecord
		if err := rows.Scan(&record.ID, &record.MigrationID, &record.Sequence, &record.Stage, &record.Message, &record.Progress, &record.BytesDone, &record.BytesTotal, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// ClaimNextManagedMigration atomically claims one coordinator-managed job. The
// lease makes a crashed control-plane replica's work visible to another worker.
func (store *Store) ClaimNextManagedMigration(ctx context.Context, owner string, lease time.Duration) (MigrationRecord, error) {
	if owner == "" || lease <= 0 {
		return MigrationRecord{}, errors.New("migration worker owner and lease are required")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return MigrationRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	record, err := scanMigration(tx.QueryRow(ctx, `WITH candidate AS (
		SELECT id FROM migration_jobs
		WHERE managed AND status IN ('queued','running') AND next_attempt_at <= now()
			AND (lease_owner IS NULL OR lease_expires_at < now())
		ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE migration_jobs AS job SET lease_owner=$1, lease_expires_at=now()+make_interval(secs => $2), attempt_count=attempt_count+1, status='running', updated_at=now()
	FROM candidate WHERE job.id=candidate.id RETURNING `+migrationColumnsQualified, owner, lease.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return MigrationRecord{}, persistence.ErrNotFound
	}
	if err != nil {
		return MigrationRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MigrationRecord{}, err
	}
	return record, nil
}

func (store *Store) UpdateManagedMigration(ctx context.Context, record MigrationRecord, status string, progress json.RawMessage, errorMessage, agentMigrationID string, retryAt time.Time, lease time.Duration) (MigrationRecord, error) {
	if len(progress) == 0 {
		progress = json.RawMessage(`{}`)
	}
	completed := status == "completed" || status == "failed" || status == "cancelled"
	renew := status == "running"
	leaseOwner := record.LeaseOwner
	result, err := scanMigration(store.pool.QueryRow(ctx, `UPDATE migration_jobs SET status=$3,progress=$4,error_message=$5,agent_migration_id=COALESCE(NULLIF($6,''),agent_migration_id),next_attempt_at=$7,lease_owner=CASE WHEN $8 THEN $2 ELSE NULL END,lease_expires_at=CASE WHEN $8 THEN now()+make_interval(secs => $9) ELSE NULL END,
		result=CASE WHEN $10 THEN COALESCE(result,'{}'::jsonb) || jsonb_build_object('duration_ms', GREATEST(0, (EXTRACT(EPOCH FROM ((CASE WHEN completed_at IS NULL THEN now() ELSE completed_at END)-created_at))*1000)::bigint)) ELSE result END,
		completed_at=CASE WHEN $10 THEN COALESCE(completed_at,now()) ELSE completed_at END,updated_at=now()
		WHERE id=$1 AND managed AND lease_owner=$2 RETURNING `+migrationColumns, record.ID, leaseOwner, status, progress, errorMessage, agentMigrationID, retryAt, renew, lease.Seconds(), completed))
	if err != nil {
		return MigrationRecord{}, err
	}
	return result, nil
}

func (store *Store) RequestManagedMigrationCancel(ctx context.Context, organizationID, migrationID string) (MigrationRecord, error) {
	return scanMigration(store.pool.QueryRow(ctx, `UPDATE migration_jobs SET cancel_requested=true,updated_at=now() WHERE organization_id=$1 AND id=$2 AND managed AND status IN ('queued','running') RETURNING `+migrationColumns, organizationID, migrationID))
}

// ManagedMigrationQueueStats reports how deep the durable migration queue is
// and how long its oldest queued job has waited. Both feed /metrics: a queue
// that only grows is the exact signal that workers cannot keep up.
func (store *Store) ManagedMigrationQueueStats(ctx context.Context) (depth int, oldestQueuedSeconds float64, err error) {
	err = store.pool.QueryRow(ctx, `SELECT count(*),
		COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at))) FILTER (WHERE status='queued'), 0)
		FROM migration_jobs WHERE managed AND status IN ('queued','running')`).Scan(&depth, &oldestQueuedSeconds)
	return depth, oldestQueuedSeconds, err
}

// MigrationsCreatedToday counts migration jobs created this UTC day — the
// per-plan MaxMigrationsPerDay cap's measurement side (plan 4 BR-13c).
func (store *Store) MigrationsCreatedToday(ctx context.Context, organizationID string, now time.Time) (int, error) {
	day := now.UTC()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	var count int
	err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM migration_jobs WHERE organization_id=$1 AND created_at >= $2`, organizationID, day).Scan(&count)
	return count, err
}
