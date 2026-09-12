package database

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// checkpoints.go: checkpoint metadata. Creation enforces, under lock, that the
// machine matches the workload's assignment, the parent of an incremental
// exists in the same workload, and the organization's storage entitlement
// still has room — then bumps used storage in the same transaction.

func (store *Store) CreateCheckpoint(ctx context.Context, record CheckpointRecord, audit AuditInput) (CheckpointRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CheckpointRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workloadMachine string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(machine_id,'') FROM workloads WHERE id=$1 AND organization_id=$2 FOR UPDATE`, record.WorkloadID, record.OrganizationID).Scan(&workloadMachine); err != nil {
		return CheckpointRecord{}, err
	}
	if workloadMachine != "" && workloadMachine != record.MachineID {
		return CheckpointRecord{}, ErrCheckpointMachineMismatch
	}
	var machineExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM machines WHERE organization_id=$1 AND machine_id=$2 AND status<>'disabled')`, record.OrganizationID, record.MachineID).Scan(&machineExists); err != nil {
		return CheckpointRecord{}, err
	}
	if !machineExists {
		return CheckpointRecord{}, pgx.ErrNoRows
	}
	if record.ParentID != "" {
		var parentExists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoints WHERE id=$1 AND organization_id=$2 AND workload_id=$3 AND status='available')`, record.ParentID, record.OrganizationID, record.WorkloadID).Scan(&parentExists); err != nil {
			return CheckpointRecord{}, err
		}
		if !parentExists {
			return CheckpointRecord{}, ErrCheckpointParentInvalid
		}
	}
	var maximumStorage, usedStorage int64
	if err := tx.QueryRow(ctx, `SELECT max_storage_bytes,used_storage_bytes FROM entitlements WHERE organization_id=$1 FOR UPDATE`, record.OrganizationID).Scan(&maximumStorage, &usedStorage); err != nil {
		return CheckpointRecord{}, err
	}
	// A negative maximum is the unlimited tier: no cap to enforce.
	if maximumStorage >= 0 && record.StoredBytes > maximumStorage-usedStorage {
		return CheckpointRecord{}, ErrStorageEntitlementExceeded
	}
	if record.Status == "" {
		record.Status = "available"
	}
	err = tx.QueryRow(ctx, `INSERT INTO checkpoints(id,organization_id,workload_id,machine_id,kind,parent_id,manifest,plain_bytes,stored_bytes,chunk_count,status)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11) RETURNING created_at`, record.ID, record.OrganizationID, record.WorkloadID, record.MachineID, record.Kind, record.ParentID, record.Manifest, record.PlainBytes, record.StoredBytes, record.ChunkCount, record.Status).Scan(&record.CreatedAt)
	if err != nil {
		return CheckpointRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return CheckpointRecord{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlements SET used_storage_bytes=used_storage_bytes+$2,updated_at=now() WHERE organization_id=$1`, record.OrganizationID, record.StoredBytes); err != nil {
		return CheckpointRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CheckpointRecord{}, err
	}
	return record, nil
}

func (store *Store) CreateCheckpointForAgent(ctx context.Context, record CheckpointRecord, audit AuditInput) (CheckpointRecord, error) {
	record.Status = "available"
	return store.CreateCheckpoint(ctx, record, audit)
}

func (store *Store) Checkpoints(ctx context.Context, organizationID, workloadID string) ([]CheckpointRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,workload_id,machine_id,kind,COALESCE(parent_id,''),manifest,plain_bytes,stored_bytes,chunk_count,status,created_at,deleted_at
		FROM checkpoints WHERE organization_id=$1 AND ($2='' OR workload_id=$2) ORDER BY created_at DESC`, organizationID, workloadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CheckpointRecord
	for rows.Next() {
		var record CheckpointRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.WorkloadID, &record.MachineID, &record.Kind, &record.ParentID, &record.Manifest, &record.PlainBytes, &record.StoredBytes, &record.ChunkCount, &record.Status, &record.CreatedAt, &record.DeletedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}
