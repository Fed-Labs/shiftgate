package billing

import "testing"

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
