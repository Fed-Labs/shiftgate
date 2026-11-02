package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// reconcile.go: the daily Stripe↔entitlement reconciliation job (plan 4
// BR-12). It (a) re-drives stripe_events with processed_at NULL older than
// 1h, (b) converges plan/status/seats from Stripe truth for every subscribed
// org — paying-but-lapsed and free-but-subscribed are both fixed and audited
// (billing.reconciled), (c) exposes the drift count for the KPI-12 alert.
// It never emails customers: Stripe owns dunning (terms §3).

const reconcileInterval = 24 * time.Hour

// reconcileStartupDelay is how long after boot the first pass runs. A replica
// redeployed more often than the daily tick (the launch-week norm) must still
// run the convergence instead of resetting the ticker forever.
const reconcileStartupDelay = 15 * time.Second

// startStripeReconciler runs the reconciliation loop: one pass shortly after
// startup, then a daily tick. Stripe must be configured; otherwise the worker
// does not start.
func (server *Server) startStripeReconciler(ctx context.Context) {
	if !server.stripe.Enabled() {
		return
	}
	server.backgroundWg.Add(1)
	go func() {
		defer server.backgroundWg.Done()
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconcileStartupDelay):
		}
		for {
			jobCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			server.runStripeReconciliation(jobCtx)
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// runStripeReconciliation is one full pass. Idempotent and lease-guarded by
// the event-level ON CONFLICT DO NOTHING plus the per-row WHERE clauses: two
// replicas racing converge to the same state and only audit once.
func (server *Server) runStripeReconciliation(ctx context.Context) {
	reprocessed := server.redriveStripeEvents(ctx)
	drifted := 0
	orgs, err := server.database.SubscribedOrganizations(ctx)
	if err != nil {
		server.logger.Warn("stripe reconciliation: org listing failed", "error", err)
		return
	}
	for _, org := range orgs {
		if !server.stripe.Enabled() {
			break
		}
		subscription, err := server.stripe.GetSubscription(ctx, org.StripeSubscriptionID)
		if err != nil {
			server.logger.Warn("stripe reconciliation: subscription fetch failed", "organization_id", org.OrganizationID, "error", err)
			continue
		}
		if server.applyReconciledSubscription(ctx, org, subscription) {
			drifted++
		}
	}
	if reprocessed > 0 || drifted > 0 {
		server.logger.Info("stripe reconciliation complete", "events_reprocessed", reprocessed, "drift_rows_fixed", drifted)
	}
}

// redriveStripeEvents re-applies the unprocessed webhook backlog from the
// event payload (plan 4 BR-12a). Replay is keyed on a deterministic derived
// event id per source event, so the same backlog row applies exactly once no
// matter how many passes re-fetch it, and a conflict no-op is never counted
// as reprocessed.
func (server *Server) redriveStripeEvents(ctx context.Context) int {
	events, err := server.database.UnprocessedStripeEvents(ctx, time.Now().UTC().Add(-time.Hour), 100)
	if err != nil {
		server.logger.Warn("stripe reconciliation: backlog query failed", "error", err)
		return 0
	}
	reprocessed := 0
	for _, event := range events {
		if !strings.HasPrefix(event.EventType, "customer.subscription.") {
			// Not replayable through the subscription apply path. It stays in
			// the backlog (and in the alert) until the webhook store can mark
			// rows processed-or-failed.
			continue
		}
		var parsed StripeEvent
		if err := json.Unmarshal(event.Payload, &parsed); err != nil {
			continue
		}
		subscription, err := parseStripeSubscription(parsed)
		if err != nil {
			continue
		}
		organizationID := subscription.Metadata["organization_id"]
		if organizationID == "" {
			continue
		}
		plan, maxStorage, maxMachines := resolveSubscriptionLimits(subscription.Metadata)
		status := subscription.Status
		if event.EventType == "customer.subscription.deleted" {
			status = "canceled"
		}
		applied, err := server.database.ApplyStripeSubscription(ctx,
			"redrive-"+event.EventID,
			event.EventType, event.Payload, organizationID, plan, status,
			subscription.Customer, subscription.ID, maxStorage, maxMachines)
		if err != nil {
			server.logger.Warn("stripe reconciliation: backlog apply failed", "event_id", event.EventID, "error", err)
			continue
		}
		if applied {
			reprocessed++
		}
	}
	return reprocessed
}

// applyReconciledSubscription converges one org's entitlement row to Stripe
// truth and audits the fix. Returns true when a drift row was corrected.
// The write is guarded against live webhooks: when the entitlement row no
// longer matches the pass's snapshot (a webhook landed between fetch and
// apply), the fresher webhook state wins and this pass skips the org.
func (server *Server) applyReconciledSubscription(ctx context.Context, org database.SubscribedOrganization, subscription billing.StripeSubscription) bool {
	changed := false
	if org.Plan != subscription.Plan && subscription.Plan != "" {
		changed = true
	}
	if org.Status != subscription.Status && subscription.Status != "" {
		changed = true
	}
	if subscription.Seats > 0 && org.Seats != subscription.Seats {
		changed = true
	}
	if !changed {
		return false
	}
	// Compare-and-set on the live row: a write since the pass snapshot means
	// a live webhook (or another replica) is fresher than this snapshot.
	current, err := server.database.Entitlement(ctx, org.OrganizationID)
	if err != nil {
		server.logger.Warn("stripe reconciliation: entitlement re-read failed", "organization_id", org.OrganizationID, "error", err)
		return false
	}
	if current.Plan != org.Plan || current.Status != org.Status || current.Seats != org.Seats {
		server.logger.Info("stripe reconciliation: skipping org whose entitlement changed under the pass", "organization_id", org.OrganizationID)
		return false
	}
	maxStorage := org.MaxStorageBytes
	maxMachines := org.MaxMachines
	if plan, ok := billing.FindPlan(subscription.Plan); ok {
		if plan.Key == "business" || plan.SeatDerivedStorage {
			maxStorage = billing.EffectiveStorageBytes(plan, subscription.Seats)
		} else {
			maxStorage = plan.MaxStorageBytes
		}
		maxMachines = plan.MaxMachines
	}
	// A unique event id per fix: a second drift the same UTC day must apply
	// too, and only a real apply is audited and counted.
	applied, err := server.database.ApplyStripeSubscription(ctx,
		"reconcile-"+org.OrganizationID+"-"+time.Now().UTC().Format("20060102150405.000000000"),
		"reconcile.drift_fix",
		[]byte("{}"),
		org.OrganizationID, subscription.Plan, subscription.Status,
		org.StripeCustomerID, subscription.ID,
		maxStorage, maxMachines)
	if err != nil {
		server.logger.Warn("stripe reconciliation: apply failed", "organization_id", org.OrganizationID, "error", err)
		return false
	}
	if !applied {
		return false
	}
	_ = server.database.AddBillingEvent(ctx, org.OrganizationID, "billing.reconciled", map[string]string{
		"from_plan": org.Plan, "from_status": org.Status,
		"to_plan": subscription.Plan, "to_status": subscription.Status,
	})
	return true
}
