package database

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// machines.go: machine registration, presence, and the machine entitlement
// check that gates registration.

func (store *Store) CreateMachine(ctx context.Context, record MachineRecord, audit AuditInput) (MachineRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MachineRecord{}, err
	}
	defer tx.Rollback(ctx)
	var maximum int
	if err := tx.QueryRow(ctx, `SELECT max_machines FROM entitlements WHERE organization_id=$1 FOR UPDATE`, record.OrganizationID).Scan(&maximum); err != nil {
		return MachineRecord{}, err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM machines WHERE organization_id=$1 AND status<>'disabled'`, record.OrganizationID).Scan(&count); err != nil {
		return MachineRecord{}, err
	}
	// A negative maximum is the unlimited tier: no cap to enforce.
	if maximum >= 0 && count >= maximum {
		return MachineRecord{}, ErrEntitlementExceeded
	}
	if record.Status == "" {
		record.Status = "offline"
	}
	err = tx.QueryRow(ctx, `INSERT INTO machines(id,organization_id,machine_id,name,agent_url,capabilities,status) VALUES($1,$2,$3,$4,$5,$6,$7)
		RETURNING created_at,updated_at`, record.ID, record.OrganizationID, record.MachineID, record.Name, record.AgentURL, record.Capabilities, record.Status).
		Scan(&record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return MachineRecord{}, err
	}
	// The capability projection commits with the document it mirrors.
	if err := syncCapabilities(ctx, tx, record.ID, record.Capabilities); err != nil {
		return MachineRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return MachineRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MachineRecord{}, err
	}
	return record, nil
}

func (store *Store) Machines(ctx context.Context, organizationID string) ([]MachineRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,machine_id,name,agent_url,capabilities,status,last_seen_at,created_at,updated_at FROM machines WHERE organization_id=$1 ORDER BY name`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MachineRecord
	for rows.Next() {
		var record MachineRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// Machine returns one registered machine. Callers that need the machine's real
// hardware — publishing a compute offer, for instance — read it here rather than
// trusting a request body.
func (store *Store) Machine(ctx context.Context, organizationID, machineID string) (MachineRecord, error) {
	var record MachineRecord
	err := store.pool.QueryRow(ctx, `SELECT id,organization_id,machine_id,name,agent_url,capabilities,status,last_seen_at,created_at,updated_at FROM machines
		WHERE organization_id=$1 AND machine_id=$2`, organizationID, machineID).
		Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt)
	return record, err
}

// UpdateMachineHeartbeat refreshes presence and the machine's self-description.
// A nil capabilities document means "unchanged", and the returned record carries
// the effective document either way — the projection is rebuilt from what the
// row now holds, not from what the caller sent, so a heartbeat that only bumps
// presence leaves the projection exactly as it was.
func (store *Store) UpdateMachineHeartbeat(ctx context.Context, organizationID, machineID, name, agentURL string, capabilities json.RawMessage, status string) (MachineRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return MachineRecord{}, err
	}
	defer tx.Rollback(ctx)
	var record MachineRecord
	err = tx.QueryRow(ctx, `UPDATE machines SET name=COALESCE(NULLIF($3,''),name),agent_url=COALESCE(NULLIF($4,''),agent_url),capabilities=COALESCE($5,capabilities),status=$6,last_seen_at=now(),updated_at=now()
		WHERE organization_id=$1 AND machine_id=$2
		RETURNING id,organization_id,machine_id,name,agent_url,capabilities,status,last_seen_at,created_at,updated_at`, organizationID, machineID, name, agentURL, capabilities, status).
		Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return MachineRecord{}, err
	}
	if err := syncCapabilities(ctx, tx, record.ID, record.Capabilities); err != nil {
		return MachineRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MachineRecord{}, err
	}
	return record, nil
}
