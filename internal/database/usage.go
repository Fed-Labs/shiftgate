package database

import (
	"context"
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
