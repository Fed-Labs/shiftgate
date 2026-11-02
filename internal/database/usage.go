package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"shift.dev/shift/internal/model"
)

// usage.go: metered usage records for billing summaries.

// Usage kinds recorded in usage_records. The set is fixed by the table's
// CHECK constraint; bandwidth (relay, egress, restores) is metered under
// UsageKindTransferBytes with a channel in the metadata, so cost-side
// economics are measurable alongside storage.
const (
	UsageKindCheckpointStorage = "checkpoint_storage"
	UsageKindTransferBytes     = "transfer_bytes"
	UsageKindMachineHours      = "machine_hours"
	UsageKindAPIRequests       = "api_requests"
)

// Transfer channels recorded in the metadata of UsageKindTransferBytes rows.
const (
	TransferChannelRelay   = "relay"
	TransferChannelEgress  = "egress"
	TransferChannelRestore = "restore"
	TransferChannelPeer    = "peer"
)

func (store *Store) AddUsage(ctx context.Context, record UsageRecord) error {
	_, err := store.pool.Exec(ctx, `INSERT INTO usage_records(id,organization_id,kind,quantity,period_start,period_end,metadata,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,COALESCE($8,now())) ON CONFLICT (id) DO NOTHING`, record.ID, record.OrganizationID, record.Kind, record.Quantity, record.PeriodStart, record.PeriodEnd, record.Metadata, record.CreatedAt)
	return err
}

// RecordTransferBytes meters one bandwidth event for an organization: relay
// passthrough, egress/restore delivery, or a direct peer transfer the
// platform observed. The call is append-only and cheap (one insert), so the
// relay and transfer paths can call it per session, per chunk batch, or per
// completed transfer. bytes must be non-negative; the day bucket is the
// event time.
func (store *Store) RecordTransferBytes(ctx context.Context, organizationID, channel string, bytes int64) error {
	if err := validateTransferMetering(organizationID, channel, bytes); err != nil {
		return err
	}
	now := time.Now().UTC()
	metadata, err := json.Marshal(map[string]string{"channel": channel})
	if err != nil {
		return err
	}
	id, err := model.NewID()
	if err != nil {
		return err
	}
	return store.AddUsage(ctx, UsageRecord{
		ID:             id,
		OrganizationID: organizationID,
		Kind:           UsageKindTransferBytes,
		Quantity:       bytes,
		PeriodStart:    now,
		PeriodEnd:      now,
		Metadata:       metadata,
		CreatedAt:      now,
	})
}

// RecordTransferBytesEvent meters one bandwidth event under a caller-derived
// idempotency key (the transfer and chunk range the bytes came from). A
// retried call with the same event id is absorbed instead of double-counted.
func (store *Store) RecordTransferBytesEvent(ctx context.Context, organizationID, channel, eventID string, bytes int64) error {
	if eventID == "" {
		return errors.New("transfer usage event requires an id")
	}
	if err := validateTransferMetering(organizationID, channel, bytes); err != nil {
		return err
	}
	now := time.Now().UTC()
	metadata, err := json.Marshal(map[string]string{"channel": channel})
	if err != nil {
		return err
	}
	return store.AddUsage(ctx, UsageRecord{
		ID:             "transfer-" + eventID,
		OrganizationID: organizationID,
		Kind:           UsageKindTransferBytes,
		Quantity:       bytes,
		PeriodStart:    now,
		PeriodEnd:      now,
		Metadata:       metadata,
		CreatedAt:      now,
	})
}

func validateTransferMetering(organizationID, channel string, bytes int64) error {
	if organizationID == "" {
		return errors.New("transfer usage requires an organization")
	}
	if bytes < 0 {
		return errors.New("transfer bytes cannot be negative")
	}
	switch channel {
	case TransferChannelRelay, TransferChannelEgress, TransferChannelRestore, TransferChannelPeer:
		return nil
	default:
		return fmt.Errorf("unknown transfer channel %q", channel)
	}
}

// utcDayBoundSQL renders a UTC-derived DATE bound for a timestamptz query
// parameter. usage_records stores DATE columns, and a bare `$n::date` cast
// converts a timestamptz parameter through the session TimeZone — so a pool
// whose sessions are not pinned to UTC shifts calendar-month and day windows
// at their boundaries (edge events land in the wrong month or in both
// months). Deriving the date through `AT TIME ZONE 'UTC'` first makes the
// bound UTC-exact regardless of the session clock.
func utcDayBoundSQL(parameter string) string {
	return "(" + parameter + "::timestamptz AT TIME ZONE 'UTC')::date"
}

// utcDayInstantSQL renders a UTC-midnight timestamptz bound for a timestamp
// column, the instant form of utcDayBoundSQL.
func utcDayInstantSQL(parameter string) string {
	return "date_trunc('day', " + parameter + "::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'"
}

// TransferBytes aggregates the metered bandwidth of one organization over
// the half-open window [from, to) — the billing summary's cost-side
// counterpart to the storage gauge. Both bounds are UTC-day aligned, and the
// open end matches TransferBytesMonth's documented [monthStart, nextMonth)
// contract: a boundary event counts in exactly one window, never two.
func (store *Store) TransferBytes(ctx context.Context, organizationID string, from, to time.Time) (int64, error) {
	var total int64
	err := store.pool.QueryRow(ctx, `SELECT COALESCE(SUM(quantity),0) FROM usage_records WHERE organization_id=$1 AND kind=$2 AND period_start >= `+utcDayBoundSQL("$3")+` AND period_end < `+utcDayBoundSQL("$4"),
		organizationID, UsageKindTransferBytes, from, to).Scan(&total)
	return total, err
}

// TransferBytesByChannel aggregates metered bandwidth per channel
// (relay/egress/restore/peer) over the same half-open UTC-day window as
// TransferBytes — the fair-use watch and the usage summary's by-channel
// breakdown.
func (store *Store) TransferBytesByChannel(ctx context.Context, organizationID string, from, to time.Time) (map[string]int64, error) {
	rows, err := store.pool.Query(ctx, `SELECT COALESCE(metadata->>'channel','?'), COALESCE(SUM(quantity),0) FROM usage_records WHERE organization_id=$1 AND kind=$2 AND period_start >= `+utcDayBoundSQL("$3")+` AND period_end < `+utcDayBoundSQL("$4")+` GROUP BY 1`,
		organizationID, UsageKindTransferBytes, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int64{}
	for rows.Next() {
		var channel string
		var total int64
		if err := rows.Scan(&channel, &total); err != nil {
			return nil, err
		}
		result[channel] = total
	}
	return result, rows.Err()
}

// TransferBytesOrgTotal is the calendar-month windowed total used by the
// fair-use throttle: [monthStart, nextMonthStart) in UTC.
func (store *Store) TransferBytesMonth(ctx context.Context, organizationID string, now time.Time) (int64, time.Time, error) {
	current := now.UTC()
	monthStart := time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	total, err := store.TransferBytes(ctx, organizationID, monthStart, nextMonth)
	return total, nextMonth, err
}

// StorageGBMonthAverage returns the month-to-date average of the daily
// checkpoint_storage gauge rows — the metered overage's GB-month quantity.
// monthStart is already the exact UTC instant of the month boundary; the
// bound is derived to a UTC DATE because the column is DATE and a bare cast
// would re-derive the boundary in the session TimeZone.
func (store *Store) StorageGBMonthAverage(ctx context.Context, organizationID string, now time.Time) (float64, error) {
	current := now.UTC()
	monthStart := time.Date(current.Year(), current.Month(), 1, 0, 0, 0, 0, time.UTC)
	var average *float64
	err := store.pool.QueryRow(ctx, `SELECT AVG(quantity)::float8 / 1073741824.0 FROM usage_records WHERE organization_id=$1 AND kind=$2 AND period_start >= `+utcDayBoundSQL("$3"),
		organizationID, UsageKindCheckpointStorage, monthStart).Scan(&average)
	if err != nil {
		return 0, err
	}
	if average == nil {
		return 0, nil
	}
	return *average, nil
}

// MigrationsSummary counts migration jobs per status inside the half-open
// UTC-day window [from, to) — the usage summary's migration health block.
func (store *Store) MigrationsSummary(ctx context.Context, organizationID string, from, to time.Time) (map[string]int, error) {
	rows, err := store.pool.Query(ctx, `SELECT status, COUNT(*) FROM migration_jobs WHERE organization_id=$1 AND created_at >= `+utcDayInstantSQL("$2")+` AND created_at < `+utcDayInstantSQL("$3")+` GROUP BY status`, organizationID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		result[status] = count
	}
	return result, rows.Err()
}

// IncrementAPIRequests meters one API request for the per-plan budget. The
// increment is one atomic upsert on the deterministic day row — concurrent
// requests each count exactly once — and the limiter's own counter enforces
// the ceiling in real time.
func (store *Store) IncrementAPIRequests(ctx context.Context, organizationID string, now time.Time) error {
	if organizationID == "" {
		return errors.New("api usage requires an organization")
	}
	day := now.UTC()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	id := organizationID + "-" + UsageKindAPIRequests + "-" + day.Format("2006-01-02")
	_, err := store.pool.Exec(ctx, `INSERT INTO usage_records(id,organization_id,kind,quantity,period_start,period_end,metadata,created_at)
		VALUES($1,$2,$3,1,$4,$4,'{}'::jsonb,now())
		ON CONFLICT (id) DO UPDATE SET quantity=usage_records.quantity+1`,
		id, organizationID, UsageKindAPIRequests, day)
	return err
}

// apiRequestsToday reads the current gauge value before an increment.
func (store *Store) apiRequestsToday(ctx context.Context, organizationID string, day time.Time) (int64, error) {
	var quantity *int64
	err := store.pool.QueryRow(ctx, `SELECT quantity FROM usage_records WHERE id=$1`, organizationID+"-"+UsageKindAPIRequests+"-"+day.Format("2006-01-02")).Scan(&quantity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if quantity == nil {
		return 0, nil
	}
	return *quantity, nil
}

// APIRequestsToday returns the metered API request count for the current UTC
// day — the per-plan daily budget's measurement side.
func (store *Store) APIRequestsToday(ctx context.Context, organizationID string, now time.Time) (int64, error) {
	day := now.UTC()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	return store.apiRequestsToday(ctx, organizationID, day)
}

// Usage returns one page of an organization's usage records inside the
// billing window, ordered by period and kind.
func (store *Store) Usage(ctx context.Context, organizationID string, from, to time.Time, page Page) ([]UsageRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,kind,quantity,period_start,period_end,metadata,created_at FROM usage_records WHERE organization_id=$1 AND period_start >= $2::date AND period_end <= $3::date ORDER BY period_start,kind`+page.clause(), organizationID, from, to)
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
