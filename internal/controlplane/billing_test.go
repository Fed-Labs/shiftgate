package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"testing"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

func TestVerifyStripeSignature(t *testing.T) {
	secret := "whsec_test_secret"
	payload := []byte(`{"id":"evt_123","type":"customer.subscription.updated"}`)
	timestamp := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "." + string(payload)))
	header := "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifyStripeSignature(payload, header, secret, time.Unix(timestamp, 0), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := VerifyStripeSignature(payload, header, "wrong", time.Unix(timestamp, 0), time.Minute); err == nil {
		t.Fatal("invalid Stripe secret was accepted")
	}
	oldHeader := "t=" + strconv.FormatInt(timestamp-3600, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
	if err := VerifyStripeSignature(payload, oldHeader, secret, time.Unix(timestamp, 0), time.Minute); err == nil {
		t.Fatal("stale Stripe signature was accepted")
	}
}

// Seat regression: the purchased ceiling comes from checkout metadata — not
// from a line-item quantity — and the seat sync target is the extras line,
// never the flat base price (whose price Stripe would multiply).
func TestSeatPurchaseUsesSeatsMetadataAndExtrasItem(t *testing.T) {
	subscription := stripeSubscription{
		Metadata: map[string]string{"plan": "business", "seats": "12", "interval": "month"},
		Items: stripeItems{Data: []stripeSubscriptionItem{
			{ID: "si_base", Quantity: 1, Price: stripeSubscriptionPrice{ID: "price_business", UnitAmount: 9900}},
			{ID: "si_addon", Quantity: 1, Price: stripeSubscriptionPrice{ID: "price_addon_sourceguard", UnitAmount: 1900}},
			{ID: "si_seats", Quantity: 7, Price: stripeSubscriptionPrice{ID: "price_generated", UnitAmount: 1900}},
		}},
	}
	plan, _ := billing.FindPlan("business")
	known := map[string]bool{"price_business": true, "price_addon_sourceguard": true}
	itemID, seats := subscription.SeatPurchase(plan, known)
	if seats != 12 {
		t.Fatalf("seats = %d, want the 12-seat purchase, not a line-item quantity", seats)
	}
	if itemID != "si_seats" {
		t.Fatalf("seat item = %q, want the extras line si_seats (add-on shares the unit price but is known)", itemID)
	}
}

func TestSeatPurchaseLegacyFallbacks(t *testing.T) {
	plan, _ := billing.FindPlan("business")
	// Flat-with-included, no seats metadata: the base-line quantity (always 1)
	// must not collapse the stored ceiling — write nothing.
	flat := stripeSubscription{
		Metadata: map[string]string{"plan": "business"},
		Items:    stripeItems{Data: []stripeSubscriptionItem{{ID: "si_base", Quantity: 1, Price: stripeSubscriptionPrice{ID: "price_business", UnitAmount: 9900}}}},
	}
	if itemID, seats := flat.SeatPurchase(plan, map[string]bool{"price_business": true}); itemID != "" || seats != 0 {
		t.Fatalf("flat legacy purchase = (%q, %d), want untouched ceiling", itemID, seats)
	}
	// Legacy per-seat plans bill seats on the base line and keep it as the
	// sync target.
	perSeat := billing.Plan{Key: "legacy-per-seat", PerSeat: true, SeatOverageCents: 1900}
	legacy := stripeSubscription{
		Metadata: map[string]string{"plan": "legacy-per-seat"},
		Items:    stripeItems{Data: []stripeSubscriptionItem{{ID: "si_base", Quantity: 9, Price: stripeSubscriptionPrice{ID: "price_legacy", UnitAmount: 1900}}}},
	}
	if itemID, seats := legacy.SeatPurchase(perSeat, nil); itemID != "si_base" || seats != 9 {
		t.Fatalf("per-seat legacy purchase = (%q, %d), want (si_base, 9)", itemID, seats)
	}
	// Negotiated tier outside the catalog keeps the historical first-item
	// fallback.
	negotiated := stripeSubscription{
		Metadata: map[string]string{"plan": "enterprise-custom"},
		Items:    stripeItems{Data: []stripeSubscriptionItem{{ID: "si_base", Quantity: 4, Price: stripeSubscriptionPrice{ID: "price_custom", UnitAmount: 1}}}},
	}
	if itemID, seats := negotiated.SeatPurchase(billing.Plan{}, nil); itemID != "si_base" || seats != 4 {
		t.Fatalf("negotiated purchase = (%q, %d), want (si_base, 4)", itemID, seats)
	}
}

// An abandoned checkout must never rewrite entitlements: "incomplete" and
// "incomplete_expired" are non-terminal payment states (report: a trial user
// clicking upgrade and abandoning payment was silently demoted mid-trial).
func TestSubscriptionStatusDeferred(t *testing.T) {
	for status, want := range map[string]bool{
		"incomplete":         true,
		"incomplete_expired": true,
		"active":             false,
		"trialing":           false,
		"past_due":           false,
		"canceled":           false,
		"unpaid":             false,
		"paused":             false,
	} {
		if got := subscriptionStatusDeferred(status); got != want {
			t.Fatalf("subscriptionStatusDeferred(%q) = %v, want %v", status, got, want)
		}
	}
}

// Transfer budget truth: negotiated/legacy tiers never land on a 0-byte
// budget, lapsed subscriptions drop to the free 20 GiB hard stop (not the
// retained display plan's allowance), and a running trial carries Business
// allotment.
func TestTransferQuotaBudgetFor(t *testing.T) {
	nextReset := time.Now().UTC().AddDate(0, 0, 1)
	paid := database.EntitlementRecord{Plan: "paid", Status: "active", MaxStorageBytes: 100 << 30, Seats: 1}
	budget := transferBudgetFor(paid, 0, nextReset)
	if budget.LimitBytes != 200<<30 {
		t.Fatalf("legacy paid limit = %d, want 200 GiB from the stored quota (was 0)", budget.LimitBytes)
	}
	if budget.IncludeFree || budget.exceeded() {
		t.Fatalf("legacy paid budget misclassified: %+v", budget)
	}

	lapsed := database.EntitlementRecord{Plan: "business", Status: "canceled", MaxStorageBytes: 600 << 30, Seats: 1}
	budget = transferBudgetFor(lapsed, 21<<30, nextReset)
	if budget.LimitBytes != billing.FreeRelayBytes {
		t.Fatalf("lapsed business limit = %d, want the free %d hard stop", budget.LimitBytes, billing.FreeRelayBytes)
	}
	if !budget.IncludeFree || !budget.exceeded() {
		t.Fatalf("lapsed business budget misclassified: %+v", budget)
	}

	trialExpires := time.Now().UTC().Add(48 * time.Hour)
	trial := database.EntitlementRecord{Plan: "free", Status: "active", Seats: 1, TrialExpiresAt: &trialExpires}
	budget = transferBudgetFor(trial, 0, nextReset)
	if budget.LimitBytes != 1200<<30 || budget.IncludeFree {
		t.Fatalf("trial budget = %+v, want Business fair-use (1200 GiB) without the free stop", budget)
	}

	free := database.EntitlementRecord{Plan: "free", Status: "active", Seats: 1}
	budget = transferBudgetFor(free, 0, nextReset)
	if budget.LimitBytes != billing.FreeRelayBytes || !budget.IncludeFree {
		t.Fatalf("free budget = %+v, want the 20 GiB hard stop", budget)
	}
}

// Trial expiry must revert capacity exactly once: only a lapsed,
// unconverted trial is due (a converted trial's limits belong to its
// subscription).
func TestTrialReleaseDue(t *testing.T) {
	expired := time.Now().UTC().Add(-time.Hour)
	running := time.Now().UTC().Add(time.Hour)
	cases := []struct {
		name        string
		entitlement database.EntitlementRecord
		want        bool
	}{
		{"expired unconverted trial", database.EntitlementRecord{TrialExpiresAt: &expired}, true},
		{"converted trial keeps subscription limits", database.EntitlementRecord{TrialExpiresAt: &expired, StripeSubscriptionID: "sub_1"}, false},
		{"running trial untouched", database.EntitlementRecord{TrialExpiresAt: &running}, false},
		{"no trial untouched", database.EntitlementRecord{}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := trialReleaseDue(testCase.entitlement); got != testCase.want {
				t.Fatalf("trialReleaseDue = %v, want %v", got, testCase.want)
			}
		})
	}
}

// The double-subscription guard's predicate: checkout is refused only while
// a paid subscription is live — a running trial (plan stays "free") must keep
// its conversion checkout, and a canceled org must be able to resubscribe.
func TestHasLiveSubscription(t *testing.T) {
	active := database.EntitlementRecord{Plan: "business", Status: "active"}
	if !hasLiveSubscription(active) {
		t.Fatal("active business subscription not detected")
	}
	trial := database.EntitlementRecord{Plan: "free", Status: "active"}
	if hasLiveSubscription(trial) {
		t.Fatal("a running trial must not block its conversion checkout")
	}
	canceled := database.EntitlementRecord{Plan: "business", Status: "canceled"}
	if hasLiveSubscription(canceled) {
		t.Fatal("a canceled org must be able to resubscribe")
	}
	incomplete := database.EntitlementRecord{Plan: "pro", Status: "incomplete"}
	if hasLiveSubscription(incomplete) {
		t.Fatal("an abandoned checkout must not block a retry")
	}
	legacy := database.EntitlementRecord{Plan: "paid", Status: "active"}
	if !hasLiveSubscription(legacy) {
		t.Fatal("legacy paid subscription not detected")
	}
}
