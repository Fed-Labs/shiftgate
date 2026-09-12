package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// usage.go: metered usage records for billing summaries.

func (store *Store) AddUsage(ctx context.Context, record UsageRecord) error {
	_, err := store.pool.Exec(ctx, `INSERT INTO usage_records(id,organization_id,kind,quantity,period_start,period_end,metadata,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,COALESCE($8,now()))`, record.ID, record.OrganizationID, record.Kind, record.Quantity, record.PeriodStart, record.PeriodEnd, record.Metadata, record.CreatedAt)
	return err
}

func (store *Store) Usage(ctx context.Context, organizationID string, from, to time.Time) ([]UsageRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,kind,quantity,period_start,period_end,metadata,created_at FROM usage_records WHERE organization_id=$1 AND period_start >= $2::date AND period_end <= $3::date ORDER BY period_start,kind`, organizationID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []UsageRecord
	for rows.Next() {
		var record UsageRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.Kind, &record.Quantity, &record.PeriodStart, &record.PeriodEnd, &record.Metadata, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// OrganizationIDs lists every organization, for the storage usage reconciler
// that recounts one prefix per organization on each pass.
func (store *Store) OrganizationIDs(ctx context.Context) ([]string, error) {
	rows, err := store.pool.Query(ctx, `SELECT id FROM organizations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

// UpdateStorageUsage records the organization's current hosted-storage
// footprint as counted from the bucket itself. It is the reconciler's write:
// the on-create increments it overrides converge with the bucket within one
// reconcile interval, and the reconciled value is the quota the credential
// broker enforces.
func (store *Store) UpdateStorageUsage(ctx context.Context, organizationID string, used int64) error {
	if used < 0 {
		return errors.New("storage usage cannot be negative")
	}
	command, err := store.pool.Exec(ctx, `UPDATE entitlements SET used_storage_bytes=$2,updated_at=now() WHERE organization_id=$1`, organizationID, used)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrEntitlementMissing
	}
	return nil
}

// UpsertDailyUsage writes one gauge row per organization, kind, and day: the
// quantity is the current total at the moment of the write, not a delta, so a
// later reconcile simply replaces it. The id is deterministic — organization,
// kind, and period — which makes the write idempotent no matter how often the
// reconciler retries.
func (store *Store) UpsertDailyUsage(ctx context.Context, record UsageRecord) error {
	if record.OrganizationID == "" || record.Kind == "" {
		return errors.New("usage record requires an organization and a kind")
	}
	id := record.ID
	if id == "" {
		id = fmt.Sprintf("%s-%s-%s", record.OrganizationID, record.Kind, record.PeriodStart.Format("2006-01-02"))
	}
	metadata := record.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	_, err := store.pool.Exec(ctx, `INSERT INTO usage_records(id,organization_id,kind,quantity,period_start,period_end,metadata,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (id) DO UPDATE SET quantity=EXCLUDED.quantity,metadata=EXCLUDED.metadata,created_at=now()`,
		id, record.OrganizationID, record.Kind, record.Quantity, record.PeriodStart, record.PeriodEnd, metadata)
	return err
}
