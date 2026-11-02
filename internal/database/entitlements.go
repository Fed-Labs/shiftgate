package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
)

// entitlements.go: plan entitlements and Stripe subscription application.
// ApplyStripeSubscription records the event id first (ON CONFLICT DO NOTHING),
// so a redelivered webhook is detected and skipped instead of double-applying
// a plan change.

// Subscription status vocabulary as delivered by Stripe. The split is the
// enforcement contract: Active() is true only while the subscription
// legitimately carries plan entitlements, and any other status has already
// lost them.
const (
	StatusActive            = "active"
	StatusTrialing          = "trialing"
	StatusPastDue           = "past_due"
	StatusCanceled          = "canceled"
	StatusIncomplete        = "incomplete"
	StatusIncompleteExpired = "incomplete_expired"
	StatusUnpaid            = "unpaid"
	StatusPaused            = "paused"
)

func (store *Store) Entitlement(ctx context.Context, organizationID string) (EntitlementRecord, error) {
	var record EntitlementRecord
	var trialExpiresAt *time.Time
	var interval string
	var seatsIncluded int
	var sourceguard, failover bool
	err := store.pool.QueryRow(ctx, `SELECT organization_id,plan,status,max_storage_bytes,used_storage_bytes,max_machines,COALESCE(stripe_customer_id,''),COALESCE(stripe_subscription_id,''),seats,COALESCE(stripe_item_id,''),trial_expires_at,COALESCE(overage_cap_cents,0),COALESCE(billing_interval,'month'),COALESCE(seats_included,0),COALESCE(sourceguard,false),COALESCE(failover,false) FROM entitlements WHERE organization_id=$1`, organizationID).
		Scan(&record.OrganizationID, &record.Plan, &record.Status, &record.MaxStorageBytes, &record.UsedStorageBytes, &record.MaxMachines, &record.StripeCustomerID, &record.StripeSubscriptionID, &record.Seats, &record.StripeItemID, &trialExpiresAt, &record.OverageCapCents, &interval, &seatsIncluded, &sourceguard, &failover)
	if err != nil {
		return record, err
	}
	record.TrialExpiresAt = trialExpiresAt
	record.Interval = interval
	record.SeatsIncluded = seatsIncluded
	record.SourceGuard = sourceguard
	record.Failover = failover
	return record, nil
}

// SetEntitlementSeats records the purchased seat quantity (and the Stripe
// subscription item it mirrors) for a per-seat plan. It is called only from
// the subscription webhook's SeatPurchase path: checkout and the customer
// portal change how many seats an organization owns, and nothing else — a
// member count must never overwrite the purchased ceiling.
func (store *Store) SetEntitlementSeats(ctx context.Context, organizationID string, seats int, stripeItemID string) error {
	if seats < 0 {
		return errors.New("seat count cannot be negative")
	}
	command, err := store.pool.Exec(ctx, `UPDATE entitlements SET seats=$2,stripe_item_id=COALESCE(NULLIF($3,''),stripe_item_id),updated_at=now() WHERE organization_id=$1`, organizationID, seats, stripeItemID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrEntitlementMissing
	}
	return nil
}

// Active reports whether the subscription currently carries its plan's
// entitlements. "past_due" stays active for the dunning grace — Stripe is
// still retrying collection, and the terminal events (canceled, unpaid,
// incomplete_expired) revoke entitlements the moment they arrive. An unknown
// status fails closed: it is treated as not entitled rather than as active.
func (record EntitlementRecord) Active() bool {
	switch strings.TrimSpace(record.Status) {
	case StatusActive, StatusTrialing, StatusPastDue:
		return true
	default:
		return false
	}
}

// TrialActive reports whether the 14-day no-card Business trial is running
// right now. Expiry needs no webhook: reads compare trial_expires_at to now()
// and fail closed the moment it passes, exactly like Active().
func (record EntitlementRecord) TrialActive() bool {
	if record.TrialExpiresAt == nil {
		return false
	}
	return record.TrialExpiresAt.After(time.Now().UTC())
}

// Allow reports whether a gated feature may be used right now: the
// subscription must be active AND the plan must include the feature. A
// running trial on an otherwise healthy entitlement counts as a Business
// subscription for the window; a cancelled subscription grants nothing even
// if a trial marker lingers; a free plan without a trial never grants paid
// features.
func (record EntitlementRecord) Allow(feature billing.Feature) bool {
	if record.TrialActive() && record.Active() {
		return billing.PlanFeatureAllowed("business", feature)
	}
	if !record.Active() {
		return false
	}
	return billing.PlanFeatureAllowed(record.Plan, feature)
}

// EffectiveLimits returns the machine and storage caps this entitlement
// actually enforces. The gate matrix is trial > active subscription > free:
// a running trial carries Business limits, a subscription that is not active
// keeps its data but falls back to the free tier's limits, so a cancelled
// plan cannot keep paid capacity forever. The stored max_* columns are
// downgraded at webhook time by ApplyStripeSubscription; this method is the
// belt-and-braces form for callers that read an entitlement directly.
func (record EntitlementRecord) EffectiveLimits() (maxStorageBytes int64, maxMachines int) {
	if record.TrialActive() && record.Active() {
		if business, ok := billing.FindPlan("business"); ok {
			seats := record.Seats
			if seats < 1 {
				seats = 1
			}
			return billing.EffectiveStorageBytes(business, seats), business.MaxMachines
		}
	}
	if record.Active() {
		return record.MaxStorageBytes, record.MaxMachines
	}
	free, ok := billing.FindPlan("free")
	if !ok {
		return record.MaxStorageBytes, record.MaxMachines
	}
	return free.MaxStorageBytes, free.MaxMachines
}

// QuotaRevisionDate is the release date of the storage-quota revision (Pro
// 500 GB → 120 GiB, Business 2 TB → seat-pooled). Organizations created
// before it are grandfathered: entitlement sync writes
// max_storage_bytes = GREATEST(existing, new) so no customer loses capacity;
// new organizations get the revised numbers. The floor guards active
// subscriptions only — a cancelled subscription still falls back to free-tier
// capacity the moment its terminal event arrives. Data is never deleted
// either way, and storage above quota stays readable/restorable — only new
// mirror credentials fail closed.
var QuotaRevisionDate = time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)

// ApplyStripeSubscription records a subscription lifecycle event and applies
// it to the organization's entitlement row. It is idempotent on the event id.
//
// Enforcement of subscription status lives here so every existing check
// (machine registration, checkpoint storage, live-migration gating) inherits
// it without knowing about it: when the subscription is not active, the
// stored limits are the free tier's — the plan key and status are kept for
// display so the dashboard can say "Business · cancelled", but the capacity
// a cancelled customer can consume is free-tier until they resubscribe.
func (store *Store) ApplyStripeSubscription(ctx context.Context, eventID, eventType string, payload []byte, organizationID, plan, status, customerID, subscriptionID string, maxStorageBytes int64, maxMachines int) (bool, error) {
	if !subscriptionStatusActive(status) {
		// Keep the plan label and status for the customer's history; apply
		// only the free tier's capacity.
		if free, ok := billing.FindPlan("free"); ok {
			maxStorageBytes = free.MaxStorageBytes
			maxMachines = free.MaxMachines
		}
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `INSERT INTO stripe_events(event_id,event_type,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, eventID, eventType, payload)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		return false, nil
	}
	if organizationID != "" {
		// Grandfathering (quota revision): organizations created before the
		// revision keep at least their existing ceiling —
		// max_storage_bytes = GREATEST(existing, new).
		var createdAt time.Time
		grandfather := false
		if err := tx.QueryRow(ctx, `SELECT created_at FROM organizations WHERE id=$1`, organizationID).Scan(&createdAt); err == nil && createdAt.Before(QuotaRevisionDate) {
			grandfather = true
		}
		if grandfather {
			if _, err := tx.Exec(ctx, `UPDATE entitlements SET grandfathered=true WHERE organization_id=$1`, organizationID); err != nil {
				return false, err
			}
		}
		// The floor is only for the quota revision itself: a same-plan
		// re-sync carrying the catalog's own value must not drop a
		// pre-revision customer below their existing ceiling. Everything
		// else applies exactly: a switch to a different plan is a
		// deliberate downgrade or upgrade, and an explicit custom limit
		// (support or test tooling setting a precise ceiling) must land as
		// written — a Pro customer who switches to Free gets Free capacity,
		// never grandfathered Pro capacity.
		floorStorage := false
		if grandfather && subscriptionStatusActive(status) {
			if catalog, ok := billing.FindPlan(plan); ok && catalog.MaxStorageBytes == maxStorageBytes {
				var currentPlan string
				if err := tx.QueryRow(ctx, `SELECT COALESCE(plan,'') FROM entitlements WHERE organization_id=$1`, organizationID).Scan(&currentPlan); err == nil && currentPlan == plan {
					floorStorage = true
				}
			}
		}
		if floorStorage {
			if _, err := tx.Exec(ctx, `UPDATE entitlements SET plan=$2,status=$3,max_storage_bytes=GREATEST(max_storage_bytes,$4),max_machines=$5,stripe_customer_id=NULLIF($6,''),stripe_subscription_id=NULLIF($7,''),updated_at=now() WHERE organization_id=$1`,
				organizationID, plan, status, maxStorageBytes, maxMachines, customerID, subscriptionID); err != nil {
				return false, err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE entitlements SET plan=$2,status=$3,max_storage_bytes=$4,max_machines=$5,stripe_customer_id=NULLIF($6,''),stripe_subscription_id=NULLIF($7,''),updated_at=now() WHERE organization_id=$1`,
			organizationID, plan, status, maxStorageBytes, maxMachines, customerID, subscriptionID); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE stripe_events SET processed_at=now() WHERE event_id=$1`, eventID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// subscriptionStatusActive is the single status predicate shared with
// EntitlementRecord.Active; an unknown status fails closed.
func subscriptionStatusActive(status string) bool {
	return (EntitlementRecord{Status: status}).Active()
}

// SubscribedOrganization is one row of the reconciliation job's work list.
type SubscribedOrganization struct {
	OrganizationID       string
	Plan                 string
	Status               string
	Seats                int
	StripeCustomerID     string
	StripeSubscriptionID string
	MaxStorageBytes      int64
	MaxMachines          int
}

// SubscribedOrganizations lists every organization with a Stripe
// subscription id — the reconciliation job's work list.
func (store *Store) SubscribedOrganizations(ctx context.Context) ([]SubscribedOrganization, error) {
	rows, err := store.pool.Query(ctx, `SELECT organization_id,COALESCE(plan,''),COALESCE(status,''),COALESCE(seats,0),COALESCE(stripe_customer_id,''),stripe_subscription_id,max_storage_bytes,max_machines FROM entitlements WHERE stripe_subscription_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SubscribedOrganization
	for rows.Next() {
		var record SubscribedOrganization
		if err := rows.Scan(&record.OrganizationID, &record.Plan, &record.Status, &record.Seats, &record.StripeCustomerID, &record.StripeSubscriptionID, &record.MaxStorageBytes, &record.MaxMachines); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// StaleStripeEventBacklog counts unprocessed webhook events older than the
// cutoff. It is read-only and safe to call from a metrics scrape.
func (store *Store) StaleStripeEventBacklog(ctx context.Context, olderThan time.Time) (int64, error) {
	var count int64
	err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM stripe_events WHERE processed_at IS NULL AND received_at < $1`, olderThan).Scan(&count)
	return count, err
}

// UnprocessedStripeEvent is one backlog webhook awaiting reapply.
type UnprocessedStripeEvent struct {
	EventID   string
	EventType string
	Payload   []byte
}

// UnprocessedStripeEvents fetches the backlog for reapply, oldest first.
func (store *Store) UnprocessedStripeEvents(ctx context.Context, olderThan time.Time, limit int) ([]UnprocessedStripeEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := store.pool.Query(ctx, `SELECT event_id,event_type,payload FROM stripe_events WHERE processed_at IS NULL AND received_at < $1 ORDER BY received_at LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []UnprocessedStripeEvent
	for rows.Next() {
		var record UnprocessedStripeEvent
		if err := rows.Scan(&record.EventID, &record.EventType, &record.Payload); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

// SetEntitlementExtras persists the billing-v2 columns the webhook carries:
// billing interval, add-on capability flags, and the overage spend cap
// (plan 2 BR-05). Trial expiry is managed by StartTrial and cleared on
// conversion here when a real subscription arrives.
func (store *Store) SetEntitlementExtras(ctx context.Context, organizationID, interval string, seatsIncluded int, sourceguard, failover bool, overageCapCents int64, clearTrial bool) error {
	if interval != "month" && interval != "year" {
		interval = "month"
	}
	if clearTrial {
		_, err := store.pool.Exec(ctx, `UPDATE entitlements SET billing_interval=$2,seats_included=$3,sourceguard=$4,failover=$5,overage_cap_cents=$6,trial_expires_at=NULL,updated_at=now() WHERE organization_id=$1`,
			organizationID, interval, seatsIncluded, sourceguard, failover, overageCapCents)
		return err
	}
	_, err := store.pool.Exec(ctx, `UPDATE entitlements SET billing_interval=$2,seats_included=$3,sourceguard=$4,failover=$5,overage_cap_cents=$6,updated_at=now() WHERE organization_id=$1`,
		organizationID, interval, seatsIncluded, sourceguard, failover, overageCapCents)
	return err
}

// EntitlementDriftRows counts KPI-10: subscribed organizations whose plan or
// status no longer matches a healthy subscription. Must be 0 daily.
func (store *Store) EntitlementDriftRows(ctx context.Context) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM entitlements WHERE stripe_subscription_id <> '' AND (status <> 'active' OR plan = 'free')`).Scan(&count)
	return count, err
}
