package database

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"

	"shift.dev/shift/internal/billing"
)

// trials.go: the 14-day no-card Business trial, once per organization, ever.
// Trial expiry needs no webhook — reads compare trial_expires_at to now() —
// but the stored max_* columns are raised at trial start and restored to the
// free tier's values on release, mirroring ApplyStripeSubscription's
// downgrade pattern.

// TrialRecord is one organization's trial state.
type TrialRecord struct {
	OrganizationID string
	StartedAt      time.Time
	ExpiresAt      time.Time
	Fingerprint    string
	Source         string
	ConvertedAt    *time.Time
}

// ErrTrialAlreadyUsed is returned when an organization that already started
// a trial asks again, in any state. The route maps it to TRIAL_ALREADY_USED.
var ErrTrialAlreadyUsed = errors.New("organization has already used its trial")

// StartTrial records the trial and raises the entitlement to Business limits
// for the trial window. It is idempotent-refusing: a second start fails with
// ErrTrialAlreadyUsed even if the first trial already expired. A real Stripe
// subscription always wins over the trial at webhook time.
func (store *Store) StartTrial(ctx context.Context, organizationID string, now time.Time, fingerprint, source string) (TrialRecord, error) {
	if organizationID == "" {
		return TrialRecord{}, errors.New("trial requires an organization")
	}
	expires := now.UTC().AddDate(0, 0, billing.TrialDays)
	business, ok := billing.FindPlan("business")
	if !ok {
		return TrialRecord{}, errors.New("business plan missing from catalog")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return TrialRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var existingID string
	err = tx.QueryRow(ctx, `SELECT organization_id FROM organization_trials WHERE organization_id=$1`, organizationID).Scan(&existingID)
	if err == nil {
		return TrialRecord{}, ErrTrialAlreadyUsed
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TrialRecord{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_trials(organization_id,started_at,expires_at,fingerprint,source) VALUES($1,$2,$3,$4,$5)`,
		organizationID, now.UTC(), expires, fingerprint, source); err != nil {
		if IsConflict(err) {
			return TrialRecord{}, ErrTrialAlreadyUsed
		}
		return TrialRecord{}, err
	}
	// Trial capacity mirrors ApplyStripeSubscription's write so every existing
	// check (machine registration, checkpoint storage) inherits it.
	if _, err := tx.Exec(ctx, `UPDATE entitlements SET trial_expires_at=$2,max_storage_bytes=$3,max_machines=$4,updated_at=now() WHERE organization_id=$1`,
		organizationID, expires, billing.EffectiveStorageBytes(business, 1), business.MaxMachines); err != nil {
		return TrialRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TrialRecord{}, err
	}
	return TrialRecord{OrganizationID: organizationID, StartedAt: now.UTC(), ExpiresAt: expires, Fingerprint: fingerprint, Source: source}, nil
}

// Trial returns one organization's trial row, or ErrTrialAlreadyUsed's
// counterpart for "no trial": found=false with a nil error.
func (store *Store) Trial(ctx context.Context, organizationID string) (TrialRecord, bool, error) {
	var record TrialRecord
	var convertedAt *time.Time
	err := store.pool.QueryRow(ctx, `SELECT organization_id,started_at,expires_at,COALESCE(fingerprint,''),COALESCE(source,''),converted_at FROM organization_trials WHERE organization_id=$1`, organizationID).
		Scan(&record.OrganizationID, &record.StartedAt, &record.ExpiresAt, &record.Fingerprint, &record.Source, &convertedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TrialRecord{}, false, nil
		}
		return TrialRecord{}, false, err
	}
	record.ConvertedAt = convertedAt
	return record, true, nil
}

// ReleaseTrialLimits restores free-tier capacity after trial expiry. It runs
// from the entitlement read path's expiry handling and the reconciliation
// job; it is safe to call repeatedly (the WHERE clause only matches rows that
// still carry an expired, unconverted trial marker). A row that carries a
// Stripe subscription id is never touched: its limits come from the
// subscription, not from the trial. Data is never touched.
func (store *Store) ReleaseTrialLimits(ctx context.Context, organizationID string, now time.Time) error {
	free, ok := billing.FindPlan("free")
	if !ok {
		return errors.New("free plan missing from catalog")
	}
	_, err := store.pool.Exec(ctx, `UPDATE entitlements SET trial_expires_at=NULL,max_storage_bytes=$2,max_machines=$3,updated_at=now() WHERE organization_id=$1 AND trial_expires_at IS NOT NULL AND trial_expires_at <= $4 AND (stripe_subscription_id IS NULL OR stripe_subscription_id = '')`,
		organizationID, free.MaxStorageBytes, free.MaxMachines, now.UTC())
	return err
}

// ReleaseExpiredTrials sweeps every lapsed trial marker in one statement —
// the reconciliation job's batched counterpart to ReleaseTrialLimits — and
// reports how many organizations fell back to free-tier capacity.
func (store *Store) ReleaseExpiredTrials(ctx context.Context, now time.Time) (int64, error) {
	free, ok := billing.FindPlan("free")
	if !ok {
		return 0, errors.New("free plan missing from catalog")
	}
	command, err := store.pool.Exec(ctx, `UPDATE entitlements SET trial_expires_at=NULL,max_storage_bytes=$1,max_machines=$2,updated_at=now() WHERE trial_expires_at IS NOT NULL AND trial_expires_at <= $3 AND (stripe_subscription_id IS NULL OR stripe_subscription_id = '')`,
		free.MaxStorageBytes, free.MaxMachines, now.UTC())
	if err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}

// MarkTrialConverted records that the trial turned into a paid subscription.
func (store *Store) MarkTrialConverted(ctx context.Context, organizationID string, at time.Time) error {
	_, err := store.pool.Exec(ctx, `UPDATE organization_trials SET converted_at=$2 WHERE organization_id=$1 AND converted_at IS NULL`, organizationID, at.UTC())
	return err
}
