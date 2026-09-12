package database

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// workloads.go: workload registration, listing, and status updates. UpsertWorkload
// exists for the agent reporter, which reconciles remote state rather than
// assuming a create-only model.

func (store *Store) CreateWorkload(ctx context.Context, record WorkloadRecord, audit AuditInput) (WorkloadRecord, error) {
	if record.MachineID != "" {
		var exists bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM machines WHERE organization_id=$1 AND machine_id=$2)`, record.OrganizationID, record.MachineID).Scan(&exists); err != nil {
			return WorkloadRecord{}, err
		}
		if !exists {
			return WorkloadRecord{}, pgx.ErrNoRows
		}
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return WorkloadRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// An omitted status starts as empty JSON, the column's declared default —
	// binding a nil RawMessage would send NULL and violate NOT NULL, the same
	// default UpsertWorkload applies.
	status := record.Status
	if len(status) == 0 {
		status = json.RawMessage(`{}`)
	}
	err = tx.QueryRow(ctx, `INSERT INTO workloads(id,organization_id,machine_id,name,spec,status) VALUES($1,$2,NULLIF($3,''),$4,$5,$6) RETURNING created_at,updated_at`,
		record.ID, record.OrganizationID, record.MachineID, record.Name, record.Spec, status).Scan(&record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return WorkloadRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return WorkloadRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkloadRecord{}, err
	}
	return record, nil
}

func (store *Store) Workloads(ctx context.Context, organizationID string) ([]WorkloadRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,COALESCE(machine_id,''),name,spec,status,created_at,updated_at FROM workloads WHERE organization_id=$1 ORDER BY updated_at DESC`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WorkloadRecord
	for rows.Next() {
		var record WorkloadRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.Spec, &record.Status, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) UpdateWorkloadStatus(ctx context.Context, organizationID, workloadID string, status json.RawMessage, audit AuditInput) (WorkloadRecord, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return WorkloadRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var record WorkloadRecord
	err = tx.QueryRow(ctx, `UPDATE workloads SET status=$3,updated_at=now() WHERE organization_id=$1 AND id=$2
		RETURNING id,organization_id,COALESCE(machine_id,''),name,spec,status,created_at,updated_at`, organizationID, workloadID, status).
		Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.Spec, &record.Status, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return WorkloadRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return WorkloadRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkloadRecord{}, err
	}
	return record, nil
}

func (store *Store) UpsertWorkload(ctx context.Context, record WorkloadRecord, audit AuditInput) (WorkloadRecord, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return WorkloadRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var machineExists bool
	if record.MachineID == "" {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM machines WHERE organization_id=$1 AND machine_id='')`, record.OrganizationID).Scan(&machineExists)
	} else {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM machines WHERE organization_id=$1 AND machine_id=$2)`, record.OrganizationID, record.MachineID).Scan(&machineExists)
	}
	if err != nil {
		return WorkloadRecord{}, err
	}
	if !machineExists {
		return WorkloadRecord{}, pgx.ErrNoRows
	}
	status := record.Status
	if len(status) == 0 {
		status = json.RawMessage(`{}`)
	}
	err = tx.QueryRow(ctx, `INSERT INTO workloads(id,organization_id,machine_id,name,spec,status)
		VALUES($1,$2,NULLIF($3,''),$4,$5,$6)
		ON CONFLICT (id) DO UPDATE SET organization_id=EXCLUDED.organization_id,machine_id=EXCLUDED.machine_id,name=EXCLUDED.name,
			spec=EXCLUDED.spec,status=EXCLUDED.status,updated_at=now()
		RETURNING created_at,updated_at`,
		record.ID, record.OrganizationID, record.MachineID, record.Name, record.Spec, status).
		Scan(&record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return WorkloadRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return WorkloadRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkloadRecord{}, err
	}
	return record, nil
}
