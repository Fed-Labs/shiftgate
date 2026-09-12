package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// migrations.go: migration job records and their event streams. Job creation
// validates the workload's machine assignment and both machine registrations
// under lock, so a queued job always names real machines in one organization.

func (store *Store) CreateMigration(ctx context.Context, record MigrationRecord, audit AuditInput) (MigrationRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MigrationRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workloadMachine string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(machine_id,'') FROM workloads WHERE id=$1 AND organization_id=$2 FOR UPDATE`, record.WorkloadID, record.OrganizationID).Scan(&workloadMachine); err != nil {
		return MigrationRecord{}, err
	}
	if workloadMachine != "" && workloadMachine != record.SourceMachineID {
		return MigrationRecord{}, errors.New("workload is not assigned to the requested source machine")
	}
	var machineCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM machines WHERE organization_id=$1 AND machine_id=ANY($2) AND status IN ('online','offline','draining')`, record.OrganizationID, []string{record.SourceMachineID, record.DestinationMachineID}).Scan(&machineCount); err != nil {
		return MigrationRecord{}, err
	}
	if machineCount != 2 {
		return MigrationRecord{}, pgx.ErrNoRows
	}
	record.Status = "queued"
	record.Progress = json.RawMessage(`{"stage":"QUEUED","progress":0}`)
	err = tx.QueryRow(ctx, `INSERT INTO migration_jobs(id,organization_id,workload_id,source_machine_id,destination_machine_id,mode,status,progress,created_by)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING created_at,updated_at`, record.ID, record.OrganizationID, record.WorkloadID,
		record.SourceMachineID, record.DestinationMachineID, record.Mode, record.Status, record.Progress, record.CreatedBy).
		Scan(&record.CreatedAt, &record.UpdatedAt)
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

func (store *Store) Migrations(ctx context.Context, organizationID string) ([]MigrationRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,workload_id,source_machine_id,destination_machine_id,mode,status,progress,error_message,created_by,created_at,updated_at,completed_at FROM migration_jobs WHERE organization_id=$1 ORDER BY created_at DESC`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MigrationRecord
	for rows.Next() {
		var record MigrationRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.WorkloadID, &record.SourceMachineID, &record.DestinationMachineID, &record.Mode, &record.Status, &record.Progress, &record.ErrorMessage, &record.CreatedBy, &record.CreatedAt, &record.UpdatedAt, &record.CompletedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) CreateMigrationForAgent(ctx context.Context, record MigrationRecord, audit AuditInput) (MigrationRecord, error) {
	record.Status = "queued"
	record.Progress = json.RawMessage(`{"stage":"CREATED","progress":0}`)
	return store.CreateMigration(ctx, record, audit)
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
	var record MigrationRecord
	err = tx.QueryRow(ctx, `UPDATE migration_jobs SET status=$3,progress=$4,error_message=$5,
			completed_at=CASE WHEN $3 IN ('completed','failed','cancelled') THEN now() ELSE completed_at END,updated_at=now()
		WHERE organization_id=$1 AND id=$2
		RETURNING id,organization_id,workload_id,source_machine_id,destination_machine_id,mode,status,progress,error_message,created_by,created_at,updated_at,completed_at`,
		organizationID, migrationID, status, progress, errorMessage).
		Scan(&record.ID, &record.OrganizationID, &record.WorkloadID, &record.SourceMachineID, &record.DestinationMachineID,
			&record.Mode, &record.Status, &record.Progress, &record.ErrorMessage, &record.CreatedBy, &record.CreatedAt, &record.UpdatedAt, &record.CompletedAt)
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

func (store *Store) MigrationEvents(ctx context.Context, organizationID, migrationID string) ([]MigrationEventRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT e.id,e.migration_id,e.sequence,e.stage,e.message,e.progress,e.bytes_done,e.bytes_total,e.created_at
		FROM migration_events e JOIN migration_jobs j ON j.id=e.migration_id WHERE j.organization_id=$1 AND e.migration_id=$2 ORDER BY e.sequence`, organizationID, migrationID)
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
