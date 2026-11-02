package controlplane

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

// transfer_quota.go: the fair-use transfer throttle and the per-plan relay
// allotment gate. Both read the same metered window (usage_records
// transfer_bytes, calendar month UTC) and both are enforced fail-closed at
// the two platform-mediated surfaces: relay ticket issuance and
// restore/egress credential issuance. Peer-to-peer direct transfers keep
// metering but are outside platform enforcement (terms §1,
// docs/limitations.md) — the throttle never claims otherwise.
//
// The hard ceiling is billing.FairUseTransferBytes: fair_use_transfer_multiplier
// (default 2.0) × the effective storage quota. The response code is
// TRANSFER_FAIR_USE_EXCEEDED with X-Shift-Transfer-Limit / X-Shift-Transfer-Used
// headers; at 80% a warning flag rides the usage summary (BR-05) instead of a
// header-only warn.

const transferWarnFraction = 0.8

// transferBudget is the checked state of one organization's month window.
type transferBudget struct {
	UsedBytes   int64
	LimitBytes  int64
	IncludeFree bool
	NextReset   time.Time
}

// exceeded reports whether the hard fair-use ceiling is exhausted.
func (budget transferBudget) exceeded() bool {
	return budget.LimitBytes >= 0 && budget.UsedBytes >= budget.LimitBytes
}

// warn reports whether the org is at or above the 80% warn band.
func (budget transferBudget) warn() bool {
	return budget.LimitBytes > 0 && float64(budget.UsedBytes) >= float64(budget.LimitBytes)*transferWarnFraction
}

// checkTransferBudget resolves and returns the organization's current monthly
// transfer budget state. Free organizations also carry the plan's included
// allotment as a hard stop (20 GiB/mo: RELAY_QUOTA_EXCEEDED at ticket issuance
// with the upgrade path); paid organizations meter above their allotment and
// stop at the fair-use ceiling. Trial expiry is settled on this read path so
// an expired trial can never keep its Business allotment.
func (server *Server) checkTransferBudget(ctx context.Context, organizationID string) (transferBudget, error) {
	entitlement, err := server.database.Entitlement(ctx, organizationID)
	if err != nil {
		return transferBudget{}, err
	}
	server.releaseExpiredTrial(ctx, entitlement)
	used, nextReset, err := server.database.TransferBytesMonth(ctx, organizationID, time.Now().UTC())
	if err != nil {
		return transferBudget{}, err
	}
	return transferBudgetFor(entitlement, used, nextReset), nil
}

// transferBudgetFor computes the monthly transfer budget from billing state:
// the fair-use ceiling (fair_use_transfer_multiplier × effective storage
// quota), with the FREE plan's included allotment as the hard stop whenever
// the organization is on free or has no live subscription. A plan key outside
// the catalog ("paid", a negotiated tier) derives its ceiling from the
// stored quota instead of the zero-value catalog entry, so paying customers
// can never land on a 0-byte budget.
func transferBudgetFor(entitlement database.EntitlementRecord, used int64, nextReset time.Time) transferBudget {
	plan, known := billing.FindPlan(entitlement.Plan)
	if !known {
		plan = billing.Plan{Key: entitlement.Plan, MaxStorageBytes: entitlement.MaxStorageBytes, MaxMachines: entitlement.MaxMachines}
	}
	// A running trial carries Business-level allotment for the window.
	if entitlement.TrialActive() && entitlement.Active() {
		if business, ok := billing.FindPlan("business"); ok {
			plan = business
		}
	}
	budget := transferBudget{UsedBytes: used, NextReset: nextReset, LimitBytes: billing.FairUseTransferBytes(plan, entitlement.Seats)}
	// The FREE plan's included allotment is the hard stop: no metering, no
	// billing, an upgrade CTA instead. A lapsed (canceled) subscription falls
	// back to the same free ceiling — not the retained display plan's
	// allowance, which is kept only so the dashboard can say "Business ·
	// cancelled".
	if plan.Key == "free" || (!entitlement.Active() && !entitlement.TrialActive()) {
		if free, ok := billing.FindPlan("free"); ok && free.RelayAllowanceBytes > 0 {
			if budget.LimitBytes < 0 || free.RelayAllowanceBytes < budget.LimitBytes {
				budget.LimitBytes = free.RelayAllowanceBytes
			}
		}
		budget.IncludeFree = true
	}
	return budget
}

// refuseTransfer writes the throttled response. The exact contract: 429 for
// the fair-use ceiling (retryable next calendar month), 402 for the free
// plan's included allotment (upgrade/trial CTA), both carrying the machine-
// readable code and the X-Shift-Transfer-* headers scripts rely on.
func refuseTransfer(writer http.ResponseWriter, budget transferBudget) {
	writer.Header().Set("X-Shift-Transfer-Limit", strconv.FormatInt(budget.LimitBytes, 10))
	writer.Header().Set("X-Shift-Transfer-Used", strconv.FormatInt(budget.UsedBytes, 10))
	if budget.IncludeFree {
		writeJSON(writer, http.StatusPaymentRequired, map[string]any{
			"code":        billing.RelayQuotaExceededCode,
			"message":     billing.RelayQuotaExceededMessage,
			"used_bytes":  budget.UsedBytes,
			"limit_bytes": budget.LimitBytes,
			"resets_at":   budget.NextReset.Format(time.RFC3339),
			"upgrade_url": upgradeURL("", billing.RelayQuotaExceededCode),
			"trial_url":   trialURL(""),
		})
		return
	}
	writeJSON(writer, http.StatusTooManyRequests, map[string]any{
		"code":        billing.TransferFairUseExceededCode,
		"message":     billing.TransferFairUseExceededMessage,
		"used_bytes":  budget.UsedBytes,
		"limit_bytes": budget.LimitBytes,
		"resets_at":   budget.NextReset.Format(time.RFC3339),
		"upgrade_url": upgradeURL("", billing.TransferFairUseExceededCode),
		"trial_url":   trialURL(""),
	})
}

// enforceTransferBudget refuses the request when the organization's monthly
// platform-mediated transfer is exhausted. It returns false after writing the
// refusal. Callers: relay ticket issuance and restore/egress credential
// issuance.
func (server *Server) enforceTransferBudget(writer http.ResponseWriter, request *http.Request, organizationID string) bool {
	budget, err := server.checkTransferBudget(request.Context(), organizationID)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ENTITLEMENT_LOOKUP_FAILED", err)
		return false
	}
	if budget.exceeded() {
		refuseTransfer(writer, budget)
		return false
	}
	return true
}

// upgradeURL builds the standard paywall next-step URL every refusal carries:
// the hosted upgrade path with the org and feature code for attribution.
func upgradeURL(organizationID, code string) string {
	url := "https://shiftgate.dev/upgrade?feature=" + code + "&src=cli"
	if organizationID != "" {
		url += "&org=" + organizationID
	}
	return url
}

// trialURL builds the no-card trial door every refusal carries alongside the
// upgrade path (BR-12: no refusal prints without a door) — a real URL, not a
// command phrase.
func trialURL(organizationID string) string {
	url := "https://shiftgate.dev/trial?src=cli"
	if organizationID != "" {
		url += "&org=" + organizationID
	}
	return url
}

// refusePlanFeature writes a 402 entitlement refusal with the shared
// upgrade/trial next steps (plan 2 BR-12: no refusal prints without a door).
func refusePlanFeature(writer http.ResponseWriter, organizationID, code, message string) {
	writeJSON(writer, http.StatusPaymentRequired, map[string]any{
		"code":        code,
		"message":     message,
		"upgrade_url": upgradeURL(organizationID, code),
		"trial_url":   trialURL(organizationID),
	})
}

// recordRefusalEvent logs the paywall hit to the funnel events table so the
// conversion-by-wall readout (plan 2 BR-18) has its data. Failures here are
// never fatal to the request path.
func (server *Server) recordRefusalEvent(ctx context.Context, organizationID, surface, feature string) {
	_ = server.database.AddFunnelEvent(ctx, database.FunnelEvent{
		OrganizationID: organizationID,
		Kind:           "refusal",
		Surface:        surface,
		Feature:        feature,
	})
}
