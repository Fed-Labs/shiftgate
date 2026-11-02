package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"shift.dev/shift/internal/model"
)

// funnel_events.go: append-only product funnel events (paywall views, upgrade
// clicks, trial starts, refusals, churn). Privacy posture: keyed by org where
// known, never by email, never workload contents. The single SQL readout is
// "conversion by wall" — count of kind per surface/feature per month.

// FunnelEvent is one product-funnel data point.
type FunnelEvent struct {
	OrganizationID string
	Kind           string
	Surface        string
	Feature        string
	Metadata       map[string]string
}

// Funnel event kinds. The set is open (a new wall adds a kind) but these are
// the taxonomy of record.
const (
	FunnelPaywallView     = "paywall_view"
	FunnelUpgradeClick    = "upgrade_click"
	FunnelTrialStart      = "trial_start"
	FunnelTrialConvert    = "trial_convert"
	FunnelCheckoutStart   = "checkout_start"
	FunnelCheckoutSuccess = "checkout_success"
	FunnelChurn           = "churn"
	FunnelPauseAccept     = "pause_accept"
	FunnelWinbackSent     = "winback_offer_sent"
	FunnelWinbackAccept   = "winback_accept"
	FunnelRefusal         = "refusal"
)

// AddFunnelEvent appends one funnel event. It never fails a request path —
// callers treat errors as drop-the-metric.
func (store *Store) AddFunnelEvent(ctx context.Context, event FunnelEvent) error {
	if event.Kind == "" {
		return errors.New("funnel event requires a kind")
	}
	id, err := model.NewID()
	if err != nil {
		return err
	}
	metadata := json.RawMessage("{}")
	if len(event.Metadata) > 0 {
		if encoded, err := json.Marshal(event.Metadata); err == nil {
			metadata = encoded
		}
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO funnel_events(id,organization_id,kind,surface,feature,metadata) VALUES($1,NULLIF($2,''),$3,$4,$5,$6)`,
		id, event.OrganizationID, event.Kind, event.Surface, event.Feature, metadata)
	return err
}

// AddBillingEvent appends one subscription lifecycle event (churn, dunning,
// pause, win-back) for the win-back queue and the experiments' metrics.
func (store *Store) AddBillingEvent(ctx context.Context, organizationID, kind string, metadata map[string]string) error {
	if kind == "" {
		return errors.New("billing event requires a kind")
	}
	id, err := model.NewID()
	if err != nil {
		return err
	}
	encoded := json.RawMessage("{}")
	if len(metadata) > 0 {
		if marshaled, err := json.Marshal(metadata); err == nil {
			encoded = marshaled
		}
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO billing_events(id,organization_id,kind,metadata) VALUES($1,$2,$3,$4)`, id, organizationID, kind, encoded)
	return err
}

// FunnelCounts aggregates funnel events by kind for a window — the weekly
// scoreboard's funnel block and the W-readouts' "conversion by wall".
func (store *Store) FunnelCounts(ctx context.Context, from, to time.Time) (map[string]int, error) {
	rows, err := store.pool.Query(ctx, `SELECT kind, COUNT(*) FROM funnel_events WHERE at >= $1::date AND at <= $2::date GROUP BY kind`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		result[kind] = count
	}
	return result, rows.Err()
}
