package billing

import (
	"strings"
	"testing"
)

func TestCatalogPlansAreCoherent(t *testing.T) {
	catalog := Catalog()
	if len(catalog) != 4 {
		t.Fatalf("catalog has %d plans, want 4", len(catalog))
	}
	keys := map[string]bool{}
	for _, plan := range catalog {
		if keys[plan.Key] {
			t.Fatalf("duplicate plan key %q", plan.Key)
		}
		keys[plan.Key] = true
		if plan.Key == "" || plan.Name == "" || plan.Description == "" {
			t.Fatalf("plan %+v is missing identity fields", plan)
		}
		if len(plan.Features) == 0 {
			t.Fatalf("plan %q advertises no features", plan.Key)
		}
		// Every paid plan needs a price; every priced plan needs limits. Only
		// enterprise is custom, and only free is zero-priced.
		if plan.PriceCents < 0 && plan.Key != "enterprise" {
			t.Fatalf("plan %q has a custom price", plan.Key)
		}
		if plan.PriceCents == 0 && plan.Key != "free" {
			t.Fatalf("plan %q is priced at zero", plan.Key)
		}
		if plan.MaxMachines == 0 || plan.MaxStorageBytes == 0 {
			t.Fatalf("plan %q has a zero limit, which would block all use", plan.Key)
		}
		if plan.PriceCents > 0 && plan.MaxMachines < 0 && plan.MaxStorageBytes < 0 {
			t.Fatalf("plan %q is paid but unlimited in every dimension", plan.Key)
		}
	}
	for _, key := range []string{"free", "pro", "business", "enterprise"} {
		if !keys[key] {
			t.Fatalf("catalog is missing the %q plan", key)
		}
	}
}

func TestFreePlanMatchesEntitlementDefaults(t *testing.T) {
	free, ok := FindPlan("free")
	if !ok {
		t.Fatal("free plan not found")
	}
	// The entitlements table defaults new organizations to 2 machines and
	// 10 GiB; the catalog must not disagree with what the database enforces
	// before the first Stripe webhook arrives.
	if free.MaxMachines != 2 {
		t.Fatalf("free plan max machines = %d, want 2", free.MaxMachines)
	}
	if free.MaxStorageBytes != 10*1024*1024*1024 {
		t.Fatalf("free plan storage = %d, want 10 GiB", free.MaxStorageBytes)
	}
}

func TestFindPlanRejectsUnknownKeys(t *testing.T) {
	if _, ok := FindPlan("paid"); ok {
		t.Fatal("legacy plan name resolved to a catalog plan")
	}
	if _, ok := FindPlan(""); ok {
		t.Fatal("empty plan key resolved to a catalog plan")
	}
	if _, ok := FindPlan("enterprise"); !ok {
		t.Fatal("enterprise plan not found")
	}
}

func TestCatalogV2QuotasMatchUnitEconomics(t *testing.T) {
	// The economics revision: Pro includes 120 GiB (not 500 GB) and Business
	// pools 500 GiB + 100 GiB per seat capped at 2 TiB. These numbers are the
	// margin fix; the pricing page and every gate read them from here.
	pro, ok := FindPlan("pro")
	if !ok {
		t.Fatal("pro plan not found")
	}
	if pro.MaxStorageBytes != 120*gib {
		t.Fatalf("pro storage = %d, want 120 GiB", pro.MaxStorageBytes)
	}
	if pro.PriceCents != 2900 || pro.PriceCentsAnnual != 29000 {
		t.Fatalf("pro prices = (%d, %d), want (2900, 29000)", pro.PriceCents, pro.PriceCentsAnnual)
	}
	business, ok := FindPlan("business")
	if !ok {
		t.Fatal("business plan not found")
	}
	if !business.SeatDerivedStorage || business.StoragePerSeatBytes != 100*gib || business.StorageCapBytes != 2*tib {
		t.Fatalf("business pooled storage formula = %+v", business)
	}
	if business.IncludedSeats != 5 || business.SeatOverageCents != 1900 {
		t.Fatalf("business seats = (%d, %d), want (5, 1900)", business.IncludedSeats, business.SeatOverageCents)
	}
	if business.PerSeat {
		t.Fatal("business should be flat-with-included, not per-seat")
	}
	if got := EffectiveStorageBytes(business, 7); got != 500*gib+700*gib {
		t.Fatalf("business 7-seat storage = %d", got)
	}
	if got := EffectiveStorageBytes(business, 15); got != 2000*gib {
		t.Fatalf("business 15-seat storage = %d, want 2000 GiB (\"2 TB at 15 seats\")", got)
	}
	if got := EffectiveStorageBytes(business, 30); got != 2*tib {
		t.Fatalf("business 30-seat storage = %d, want the 2 TiB cap", got)
	}
}

func TestFairUseTransferBytesIsDoubleQuota(t *testing.T) {
	// The hard monthly transfer ceiling: fair_use_transfer_multiplier (2.0)
	// × the effective storage quota. Free lands on 20 GiB — the documented
	// 20 GB/mo hard stop.
	free, _ := FindPlan("free")
	if got := FairUseTransferBytes(free, 0); got != FreeRelayBytes {
		t.Fatalf("free fair-use transfer = %d, want %d", got, FreeRelayBytes)
	}
	pro, _ := FindPlan("pro")
	if got := FairUseTransferBytes(pro, 0); got != 240*gib {
		t.Fatalf("pro fair-use transfer = %d, want 240 GiB", got)
	}
	if !ClampFairUseTransferMultiplier(1.5) || FairUseTransferMultiplier != 1.5 {
		t.Fatal("multiplier override refused")
	}
	FairUseTransferMultiplier = 2.0
	if ClampFairUseTransferMultiplier(0) || ClampFairUseTransferMultiplier(11) {
		t.Fatal("out-of-range multiplier accepted")
	}
}

func TestV2GatesAndAddOns(t *testing.T) {
	pro, _ := FindPlan("pro")
	business, _ := FindPlan("business")
	free, _ := FindPlan("free")
	for feature, want := range map[Feature]bool{
		FeaturePeriodicPolicies: true,
		FeatureStorageReclaim:   true,
		FeatureFailover:         false,
		FeatureSourceGuard:      false,
	} {
		if pro.Allows(feature) != want {
			t.Fatalf("pro allows %s = %v, want %v", feature, pro.Allows(feature), want)
		}
		if !business.Allows(feature) {
			t.Fatalf("business should include %s", feature)
		}
		if free.Allows(feature) {
			t.Fatalf("free should not include %s", feature)
		}
	}
	ha, ok := FindAddOn("ha")
	if !ok || ha.PriceCents != 3900 || ha.Feature != FeatureFailover {
		t.Fatalf("HA Pack add-on = %+v", ha)
	}
	sg, ok := FindAddOn("sourceguard")
	if !ok || sg.PriceCents != 1900 || sg.Feature != FeatureSourceGuard {
		t.Fatalf("SourceGuard add-on = %+v", sg)
	}
	if _, ok := FindAddOn("nope"); ok {
		t.Fatal("unknown add-on resolved")
	}
}

func TestSeatCeilingAndExtraQuantity(t *testing.T) {
	business, _ := FindPlan("business")
	if got := SeatCeiling(business, 0); got != 5 {
		t.Fatalf("ceiling with no purchase = %d, want 5 included", got)
	}
	if got := SeatCeiling(business, 7); got != 7 {
		t.Fatalf("ceiling with 7 purchased = %d", got)
	}
	if got := ExtraSeatQuantity(business, 7); got != 2 {
		t.Fatalf("extra seat quantity for 7 = %d, want 2", got)
	}
	if got := ExtraSeatQuantity(business, 3); got != 1 {
		t.Fatalf("extra seat quantity below included = %d, want 1 (Stripe minimum)", got)
	}
}

// TestV2GateContract mirrors live_migration_gate_test.go's discipline for the
// v2 gates (plan 2 BR-08 acceptance): every admission point shares one
// decision via Plan.Allows + the entitlement's Active() check, and the
// refusal codes are the published contract.
func TestV2GateContract(t *testing.T) {
	cases := []struct {
		feature Feature
		plan    string
		want    bool
	}{
		{FeatureFailover, "free", false},
		{FeatureFailover, "pro", false}, // Pro needs the HA Pack add-on
		{FeatureFailover, "business", true},
		{FeatureFailover, "enterprise", true},
		{FeaturePeriodicPolicies, "free", false},
		{FeaturePeriodicPolicies, "pro", true},
		{FeatureSourceGuard, "pro", false}, // sold as the SourceGuard add-on
		{FeatureSourceGuard, "business", true},
		{FeatureStorageReclaim, "free", false},
		{FeatureStorageReclaim, "pro", true},
	}
	for _, testCase := range cases {
		plan, ok := FindPlan(testCase.plan)
		if !ok {
			t.Fatalf("plan %q not found", testCase.plan)
		}
		if got := plan.Allows(testCase.feature); got != testCase.want {
			t.Fatalf("%s on %s = %v, want %v", testCase.feature, testCase.plan, got, testCase.want)
		}
		if got := PlanFeatureAllowed(testCase.plan, testCase.feature); got != testCase.want {
			t.Fatalf("PlanFeatureAllowed(%s, %s) = %v, want %v", testCase.plan, testCase.feature, got, testCase.want)
		}
	}
	// The add-on path: buying the HA Pack grants failover on Pro via the
	// entitlement's Failover flag (set by the webhook from add-on metadata).
	// The catalog stays the fallback for plans without add-on state.
	for code, message := range map[string]string{
		PeriodicPolicyPlanRequiredCode: PeriodicPolicyPlanRequiredMessage,
		FailoverPlanRequiredCode:       FailoverPlanRequiredMessage,
		StorageReclaimPlanRequiredCode: StorageReclaimPlanRequiredMessage,
		RelayQuotaExceededCode:         RelayQuotaExceededMessage,
		TransferFairUseExceededCode:    TransferFairUseExceededMessage,
		OverageCapReachedCode:          OverageCapReachedMessage,
		RateLimitUpgradeCode:           RateLimitUpgradeMessage,
		TrialAlreadyUsedCode:           TrialAlreadyUsedMessage,
	} {
		if code == "" || message == "" {
			t.Fatal("refusal contract must carry both a code and a message")
		}
	}
}

// TestEntitlementStateTruth is the regression cover for the trial's Business
// activation: a running no-card trial must get the Business feature set it is
// sold with (live migration included), while the free tier stays cold-only
// and a lapsed subscription grants nothing.
func TestEntitlementStateTruth(t *testing.T) {
	cases := []struct {
		name    string
		state   EntitlementState
		feature Feature
		want    bool
	}{
		{"trial gets live migration", EntitlementState{PlanKey: "free", SubscriptionActive: true, TrialActive: true}, FeatureLiveMigration, true},
		{"trial gets teams", EntitlementState{PlanKey: "free", SubscriptionActive: true, TrialActive: true}, FeatureTeamMembers, true},
		{"trial gets failover", EntitlementState{PlanKey: "free", SubscriptionActive: true, TrialActive: true}, FeatureFailover, true},
		{"free stays cold-only", EntitlementState{PlanKey: "free", SubscriptionActive: true}, FeatureLiveMigration, false},
		{"free stays without teams", EntitlementState{PlanKey: "free", SubscriptionActive: true}, FeatureTeamMembers, false},
		{"expired trial reverts to free", EntitlementState{PlanKey: "free", SubscriptionActive: true}, FeatureLiveMigration, false},
		{"lapsed pro grants nothing", EntitlementState{PlanKey: "pro", SubscriptionActive: false}, FeatureLiveMigration, false},
		{"lapsed trial grants nothing", EntitlementState{PlanKey: "business", SubscriptionActive: false, TrialActive: true}, FeatureLiveMigration, false},
		{"pro keeps live", EntitlementState{PlanKey: "pro", SubscriptionActive: true}, FeatureLiveMigration, true},
		{"business keeps failover", EntitlementState{PlanKey: "business", SubscriptionActive: true}, FeatureFailover, true},
		{"negotiated tier keeps live", EntitlementState{PlanKey: "legacy-paid", SubscriptionActive: true}, FeatureLiveMigration, true},
		{"empty plan fails closed", EntitlementState{PlanKey: "", SubscriptionActive: true}, FeatureLiveMigration, false},
		{"empty plan fails closed for features", EntitlementState{PlanKey: "", SubscriptionActive: true}, FeatureAuditLogging, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.state.Allows(testCase.feature); got != testCase.want {
				t.Fatalf("Allows(%s) = %v, want %v (state %+v)", testCase.feature, got, testCase.want, testCase.state)
			}
			if testCase.feature == FeatureLiveMigration {
				if got := testCase.state.LiveMigrationAllowed(); got != testCase.want {
					t.Fatalf("LiveMigrationAllowed() = %v, want %v", got, testCase.want)
				}
			}
		})
	}
}

// TestEntitlementStateAddOnFlags: the HA Pack and SourceGuard add-ons are
// billed capability flags — a Pro customer who bought one must get it while
// active, and must lose it the moment the subscription lapses.
func TestEntitlementStateAddOnFlags(t *testing.T) {
	withAddOns := EntitlementState{PlanKey: "pro", SubscriptionActive: true, FailoverAddOn: true, SourceGuardAddOn: true}
	if !withAddOns.Allows(FeatureFailover) {
		t.Fatal("HA Pack add-on flag did not unlock failover on Pro")
	}
	if !withAddOns.Allows(FeatureSourceGuard) {
		t.Fatal("SourceGuard add-on flag did not unlock SourceGuard on Pro")
	}
	if withAddOns.Allows(FeatureTeamMembers) {
		t.Fatal("add-on flags leaked a Business-only feature")
	}
	lapsed := EntitlementState{PlanKey: "pro", SubscriptionActive: false, FailoverAddOn: true, SourceGuardAddOn: true}
	if lapsed.Allows(FeatureFailover) || lapsed.Allows(FeatureSourceGuard) {
		t.Fatal("add-on flags granted features to a lapsed subscription")
	}
	without := EntitlementState{PlanKey: "pro", SubscriptionActive: true}
	if without.Allows(FeatureFailover) || without.Allows(FeatureSourceGuard) {
		t.Fatal("Pro without add-ons got an add-on feature")
	}
}

// TestFeatureRefusalContract: every gated feature refuses with a published
// code and message — the per-feature codes where one exists, the generic wall
// otherwise — so scripts can match on them and every surface agrees.
func TestBillingFeatureRefusalContract(t *testing.T) {
	cases := map[Feature][2]string{
		FeatureLiveMigration:    {LiveMigrationPlanRequiredCode, LiveMigrationPlanRequiredMessage},
		FeaturePeriodicPolicies: {PeriodicPolicyPlanRequiredCode, PeriodicPolicyPlanRequiredMessage},
		FeatureFailover:         {FailoverPlanRequiredCode, FailoverPlanRequiredMessage},
		FeatureStorageReclaim:   {StorageReclaimPlanRequiredCode, StorageReclaimPlanRequiredMessage},
		FeatureTeamMembers:      {FeaturePlanRequiredCode, FeaturePlanRequiredMessage},
		FeatureAuditLogging:     {FeaturePlanRequiredCode, FeaturePlanRequiredMessage},
		FeatureSSO:              {FeaturePlanRequiredCode, FeaturePlanRequiredMessage},
		FeatureSCIM:             {FeaturePlanRequiredCode, FeaturePlanRequiredMessage},
		FeatureSourceGuard:      {FeaturePlanRequiredCode, FeaturePlanRequiredMessage},
	}
	for feature, want := range cases {
		code, message := FeatureRefusal(feature)
		if code != want[0] || message != want[1] {
			t.Fatalf("FeatureRefusal(%s) = (%q, %q), want (%q, %q)", feature, code, message, want[0], want[1])
		}
	}
}

// TestCatalogAdvertisesNoUnmeteredOverage: the shipped product hard-stops at
// the included quota and bills no metered per-GB overage, so the catalog must
// not advertise one (ship finding: advertised overage pricing was unenforced
// dead code). The structured overage fields stay zero until the metering loop
// enforces them.
func TestBillingCatalogAdvertisesNoUnmeteredOverage(t *testing.T) {
	for _, plan := range Catalog() {
		for _, feature := range plan.Features {
			if strings.Contains(feature, "/GB") || strings.Contains(feature, "GB-mo") {
				t.Fatalf("plan %q advertises metered overage in %q", plan.Key, feature)
			}
		}
		if plan.OverageStorageCentsPerGB != 0 || plan.OverageTransferCentsPerGB != 0 || plan.OverageCapCentsDefault != 0 {
			t.Fatalf("plan %q prices unenforced metered overage: %+v", plan.Key, plan)
		}
	}
}

// TestFreeTierQuotasStayColdOnly: the free tier must keep exactly the
// advertised shape — 2 machines, 10 GiB storage, cold-only, 20 GiB transfer.
func TestFreeTierQuotasStayColdOnly(t *testing.T) {
	free, ok := FindPlan("free")
	if !ok {
		t.Fatal("free plan not found")
	}
	if free.MaxMachines != 2 || free.MaxStorageBytes != FreeStorageBytes {
		t.Fatalf("free quotas = (%d machines, %d bytes), want (2, %d)", free.MaxMachines, free.MaxStorageBytes, FreeStorageBytes)
	}
	if free.LiveMigration {
		t.Fatal("free tier must stay cold-only")
	}
	if free.RelayAllowanceBytes != FreeRelayBytes {
		t.Fatalf("free relay allowance = %d, want %d", free.RelayAllowanceBytes, FreeRelayBytes)
	}
}
