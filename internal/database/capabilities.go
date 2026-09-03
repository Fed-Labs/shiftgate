package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
func (store *Store) MachinesWithCapability(ctx context.Context, organizationID, capability string, kind string, minimum *float64) ([]MachineRecord, error) {
	var query string
	var args []any
	switch kind {
	case "number":
		query = `SELECT m.id,m.organization_id,m.machine_id,m.name,m.agent_url,m.capabilities,m.status,m.last_seen_at,m.created_at,m.updated_at
			FROM machines m JOIN machine_capabilities c ON c.machine_record_id=m.id
			WHERE m.organization_id=$1 AND c.capability=$2 AND c.kind='number'`
		args = []any{organizationID, capability}
		if minimum != nil {
			query += ` AND c.number_value >= $3`
			args = append(args, *minimum)
		}
		query += ` ORDER BY c.number_value DESC, m.name`
	case "text":
		query = `SELECT m.id,m.organization_id,m.machine_id,m.name,m.agent_url,m.capabilities,m.status,m.last_seen_at,m.created_at,m.updated_at
			FROM machines m JOIN machine_capabilities c ON c.machine_record_id=m.id
			WHERE m.organization_id=$1 AND c.capability=$2 AND c.kind='text' ORDER BY m.name`
		args = []any{organizationID, capability}
	case "boolean":
		query = `SELECT m.id,m.organization_id,m.machine_id,m.name,m.agent_url,m.capabilities,m.status,m.last_seen_at,m.created_at,m.updated_at
			FROM machines m JOIN machine_capabilities c ON c.machine_record_id=m.id
			WHERE m.organization_id=$1 AND c.capability=$2 AND c.kind='boolean' AND c.boolean_value ORDER BY m.name`
		args = []any{organizationID, capability}
	default:
		return nil, fmt.Errorf("capability kind must be number, text, or boolean, not %q", kind)
	}
	rows, err := store.pool.Query(ctx, query, args...)
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
	defer tx.Rollback(ctx)
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

// RetentionSweep is what one enforcement pass deleted.
type RetentionSweep struct {
	AuditEventsDeleted   int64
	CheckpointsMarked    int64
	StorageObjectsPurged int64
}

// EnforceRetention applies every organization's policy once: audit rows older
// than their window are deleted, checkpoints older than their window are
// marked deleted, and storage objects already soft-deleted past their grace
// window are purged. Checkpoint rows are marked, not dropped — the workload's
// lineage stays intact — and the storage-object purge only removes rows whose
// bytes the object store deleted; the accounting entry goes with them.
func (store *Store) EnforceRetention(ctx context.Context) (RetentionSweep, error) {
	var sweep RetentionSweep
	// Audit rows past their window.
	tag, err := store.pool.Exec(ctx, `DELETE FROM audit_events a
		USING retention_policies p
		WHERE a.organization_id=p.organization_id
		  AND p.audit_retention_days>0
		  AND a.created_at < now() - make_interval(days => p.audit_retention_days)`)
	if err != nil {
		return sweep, err
	}
	sweep.AuditEventsDeleted = tag.RowsAffected()
	// Checkpoints past their window: mark deleted, once.
	tag, err = store.pool.Exec(ctx, `UPDATE checkpoints c
		SET status='deleted', deleted_at=COALESCE(deleted_at,now())
		FROM retention_policies p
		WHERE c.organization_id=p.organization_id
		  AND p.checkpoint_retention_days>0
		  AND c.status IN ('available','creating')
		  AND c.created_at < now() - make_interval(days => p.checkpoint_retention_days)`)
	if err != nil {
		return sweep, err
	}
	sweep.CheckpointsMarked = tag.RowsAffected()
	// Storage objects soft-deleted past their grace window.
	tag, err = store.pool.Exec(ctx, `DELETE FROM storage_objects o
		USING retention_policies p
		WHERE o.organization_id=p.organization_id
		  AND p.deleted_storage_retention_days>0
		  AND o.state='deleted'
		  AND o.deleted_at < now() - make_interval(days => p.deleted_storage_retention_days)`)
	if err != nil {
		return sweep, err
	}
	sweep.StorageObjectsPurged = tag.RowsAffected()
	return sweep, nil
}
