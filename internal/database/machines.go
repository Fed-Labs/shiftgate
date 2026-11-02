package database

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// machines.go: machine registration, presence, and the machine entitlement
// check that gates registration. Presence is derived server-side: a machine
// that self-reports "online" but stops heartbeating reads offline after two
// minutes — the fleet view must never show a dead host as live.

// machinePresenceStaleness is how long a machine may go without a heartbeat
// before its self-reported "online" is treated as offline.
const machinePresenceStaleness = "2 minutes"

// effectiveMachineStatusSQL rewrites a self-reported status with the presence
// staleness bound applied. The alias qualifies the columns for join queries
// and may be empty for the machines table alone.
func effectiveMachineStatusSQL(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return "CASE WHEN " + prefix + "status='online' AND COALESCE(" + prefix + "last_seen_at, " + prefix + "updated_at) < now() - interval '" + machinePresenceStaleness + "' THEN 'offline' ELSE " + prefix + "status END"
}

var machineRecordColumns = `id,organization_id,machine_id,name,agent_url,local_agent_url,capabilities,` + effectiveMachineStatusSQL("") + `,last_seen_at,created_at,updated_at`

func (store *Store) CreateMachine(ctx context.Context, record MachineRecord, audit AuditInput) (MachineRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return MachineRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
	// A nil document must bind as an empty object, not SQL NULL: the column
	// default only applies when the parameter is omitted.
	if record.Capabilities == nil {
		record.Capabilities = json.RawMessage(`{}`)
	}
	err = tx.QueryRow(ctx, `INSERT INTO machines(id,organization_id,machine_id,name,agent_url,local_agent_url,capabilities,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING created_at,updated_at`, record.ID, record.OrganizationID, record.MachineID, record.Name, record.AgentURL, record.LocalAgentURL, record.Capabilities, record.Status).
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

func (store *Store) Machines(ctx context.Context, organizationID string, page Page) ([]MachineRecord, error) {
	query := `SELECT ` + machineRecordColumns + ` FROM machines WHERE organization_id=$1 ORDER BY name` + page.clause()
	rows, err := store.pool.Query(ctx, query, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MachineRecord
	for rows.Next() {
		var record MachineRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.LocalAgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt); err != nil {
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
	err := store.pool.QueryRow(ctx, `SELECT `+machineRecordColumns+` FROM machines
		WHERE organization_id=$1 AND machine_id=$2`, organizationID, machineID).
		Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.LocalAgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt)
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
	defer func() { _ = tx.Rollback(ctx) }()
	var record MachineRecord
	err = tx.QueryRow(ctx, `UPDATE machines SET name=COALESCE(NULLIF($3,''),name),agent_url=COALESCE(NULLIF($4,''),agent_url),capabilities=COALESCE($5,capabilities),status=$6,last_seen_at=now(),updated_at=now()
		WHERE organization_id=$1 AND machine_id=$2
		RETURNING id,organization_id,machine_id,name,agent_url,local_agent_url,capabilities,status,last_seen_at,created_at,updated_at`, organizationID, machineID, name, agentURL, capabilities, status).
		Scan(&record.ID, &record.OrganizationID, &record.MachineID, &record.Name, &record.AgentURL, &record.LocalAgentURL, &record.Capabilities, &record.Status, &record.LastSeenAt, &record.CreatedAt, &record.UpdatedAt)
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

// MachinePresenceLag reports the longest silence among non-disabled machines,
// in seconds. It feeds the shift_machine_presence_lag_seconds gauge so a host
// dying between heartbeats is alertable before the staleness sweep flips it
// offline.
func (store *Store) MachinePresenceLag(ctx context.Context) (float64, error) {
	var lag float64
	err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(EXTRACT(EPOCH FROM (now() - COALESCE(last_seen_at, updated_at)))), 0)
		FROM machines WHERE status <> 'disabled'`).Scan(&lag)
	return lag, err
}
