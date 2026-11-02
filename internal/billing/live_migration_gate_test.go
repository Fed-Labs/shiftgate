package billing

import "testing"

// Regression cover for the P2P live-migration paywall decision (ship finding
// F-01): live mode is a paid entitlement, and this function is the single
// decision every admission point shares. The free tier must be cold-only,
// lapsed subscriptions must fall back to cold-only, and negotiated tiers the
// catalog does not know must keep working while their contract is active.
func TestLiveMigrationAllowed(t *testing.T) {
	cases := []struct {
		name    string
		plan    string
		active  bool
		allowed bool
	}{
		{"free tier is cold-only", "free", true, false},
		{"pro includes live", "pro", true, true},
		{"business includes live", "business", true, true},
		{"enterprise includes live", "enterprise", true, true},
		{"lapsed pro falls back to cold-only", "pro", false, false},
		{"lapsed free is cold-only", "free", false, false},
		{"negotiated tier fails open while active", "legacy-paid", true, true},
		{"negotiated tier fails closed when lapsed", "legacy-paid", false, false},
		{"unknown empty plan is cold-only while active", "", true, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LiveMigrationAllowed(testCase.plan, testCase.active); got != testCase.allowed {
				t.Fatalf("LiveMigrationAllowed(%q, %v) = %v, want %v", testCase.plan, testCase.active, got, testCase.allowed)
			}
		})
	}
}

// The trial-aware form of the same decision: the no-card trial is sold as
// "14 days of Business", so a running trial must pass the gate (the single
// capability the trial is sold for) while a free org with no trial and any
// lapsed subscription stay cold-only.
func TestLiveMigrationAllowedForTrial(t *testing.T) {
	cases := []struct {
		name    string
		state   EntitlementState
		allowed bool
	}{
		{"trial is entitled to live", EntitlementState{PlanKey: "free", SubscriptionActive: true, TrialActive: true}, true},
		{"trial on pro stays entitled", EntitlementState{PlanKey: "pro", SubscriptionActive: true, TrialActive: true}, true},
		{"free without trial is cold-only", EntitlementState{PlanKey: "free", SubscriptionActive: true}, false},
		{"expired trial falls back to cold-only", EntitlementState{PlanKey: "free", SubscriptionActive: true, TrialActive: false}, false},
		{"lapsed trial is cold-only", EntitlementState{PlanKey: "free", SubscriptionActive: false, TrialActive: true}, false},
		{"paid plan stays entitled", EntitlementState{PlanKey: "business", SubscriptionActive: true}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.state.LiveMigrationAllowed(); got != testCase.allowed {
				t.Fatalf("LiveMigrationAllowed(%+v) = %v, want %v", testCase.state, got, testCase.allowed)
			}
		})
	}
}

// The error contract is part of the published API surface: the CLI, the
// agent, and the control plane must refuse with the identical code and
// message so scripts can match on it.
func TestLiveMigrationPlanRequiredContract(t *testing.T) {
	if LiveMigrationPlanRequiredCode != "LIVE_MIGRATION_PLAN_REQUIRED" {
		t.Fatalf("error code changed: %q", LiveMigrationPlanRequiredCode)
	}
	if LiveMigrationPlanRequiredMessage == "" {
		t.Fatal("refusal message must name the remedy")
	}
}

// The pricing page renders catalog feature strings verbatim; none of them may
// promise more than the platform provides (ship finding F-05: the enterprise
// card claimed "Custom data residency" while the hosted service is
// single-region).
func TestEnterpriseResidencyClaimIsQualified(t *testing.T) {
	enterprise, ok := FindPlan("enterprise")
	if !ok {
		t.Fatal("enterprise plan not found")
	}
	for _, feature := range enterprise.Features {
		if feature == "Custom data residency" {
			t.Fatal("enterprise features claim bare custom data residency; hosted residency is self-hosted only")
		}
	}
}
