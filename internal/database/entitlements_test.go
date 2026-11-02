package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/model"
)

// TestEntitlementActiveStatuses encodes the subscription-status enforcement
// contract: only live subscriptions carry plan entitlements, past_due keeps
// them for the dunning grace, and everything else — including unknown
// statuses — fails closed.
func TestEntitlementActiveStatuses(t *testing.T) {
	cases := []struct {
		status string
		active bool
	}{
		{"active", true},
		{"trialing", true},
		{"past_due", true},
		{"canceled", false},
		{"cancelled", false},
		{"unpaid", false},
		{"incomplete", false},
		{"incomplete_expired", false},
		{"paused", false},
		{"", false},
		{"nonsense", false},
	}
	for _, c := range cases {
		record := EntitlementRecord{Status: c.status}
		if got := record.Active(); got != c.active {
			t.Fatalf("Active() for status %q = %v, want %v", c.status, got, c.active)
		}
	}
}

// TestEntitlementAllowGatesFeaturesOnStatusAndPlan is the B-4 contract: a
// cancelled subscription loses its plan's features even while its row still
// says "business", and a free plan never grants paid features even while
// active.
func TestEntitlementAllowGatesFeaturesOnStatusAndPlan(t *testing.T) {
	canceled := EntitlementRecord{Plan: "business", Status: "canceled"}
	if canceled.Allow(billing.FeatureSSO) {
		t.Fatal("canceled business subscription allowed SSO")
	}
	if canceled.Allow(billing.FeatureLiveMigration) {
		t.Fatal("canceled business subscription allowed live migration")
	}
	activeBusiness := EntitlementRecord{Plan: "business", Status: "active"}
	for _, feature := range []billing.Feature{billing.FeatureSSO, billing.FeatureSCIM, billing.FeatureAuditLogging, billing.FeatureTeamMembers, billing.FeatureLiveMigration} {
		if !activeBusiness.Allow(feature) {
			t.Fatalf("active business subscription refused %s", feature)
		}
	}
	activeFree := EntitlementRecord{Plan: "free", Status: "active"}
	if activeFree.Allow(billing.FeatureLiveMigration) {
		t.Fatal("free plan allowed live migration")
	}
	if activeFree.Allow(billing.FeatureSSO) {
		t.Fatal("free plan allowed SSO")
	}
	if activeFree.Allow("") {
		t.Fatal("unknown feature should fail closed")
	}
	// Negotiated tiers outside the catalog follow their contract.
	custom := EntitlementRecord{Plan: "paid", Status: "active"}
	if !custom.Allow(billing.FeatureAuditLogging) {
		t.Fatal("negotiated plan refused an upgraded feature")
	}
}

// TestEntitlementEffectiveLimitsDowngradesInactive is the capacity half of
// the same contract: a lapsed subscription keeps its data but not its paid
// machine and storage capacity.
func TestEntitlementEffectiveLimitsDowngradesInactive(t *testing.T) {
	free, ok := billing.FindPlan("free")
	if !ok {
		t.Fatal("free plan missing from catalog")
	}
	active := EntitlementRecord{Plan: "business", Status: "active", MaxStorageBytes: 2 << 40, MaxMachines: -1}
	storage, machines := active.EffectiveLimits()
	if storage != 2<<40 || machines != -1 {
		t.Fatalf("active entitlement limits = %d/%d, want 2TiB/unlimited", storage, machines)
	}
	canceled := EntitlementRecord{Plan: "business", Status: "canceled", MaxStorageBytes: 2 << 40, MaxMachines: -1}
	storage, machines = canceled.EffectiveLimits()
	if storage != free.MaxStorageBytes || machines != free.MaxMachines {
		t.Fatalf("canceled entitlement limits = %d/%d, want free %d/%d", storage, machines, free.MaxStorageBytes, free.MaxMachines)
	}
}

// uniqueEventID keeps stripe_events ids unique across runs against the
// shared test database (isolation comes from fresh organization ids; event
// ids must be just as fresh or the idempotency table rejects the replay).
func uniqueEventID(t *testing.T, label string) string {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return "evt-" + label + "-" + id
}

// TestApplyStripeSubscriptionDowngradesOnCancel proves the webhook path
// stores free-tier capacity for a cancelled subscription while preserving the
// plan label and status for display — and that a later resubscribe restores
// paid capacity. PostgreSQL-gated.
func TestApplyStripeSubscriptionDowngradesOnCancel(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "entitlement-cancel")
	free, _ := billing.FindPlan("free")

	applied, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "ent-active"), "customer.subscription.created",
		[]byte(`{}`), organizationID, "pro", "active", "cus_test", "sub_test", 500*1024*1024*1024, 10)
	if err != nil || !applied {
		t.Fatalf("apply active subscription: applied=%v err=%v", applied, err)
	}
	record, err := store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.MaxMachines != 10 || record.Plan != "pro" {
		t.Fatalf("active subscription stored %+v", record)
	}

	deletedEvent := uniqueEventID(t, "ent-deleted")
	applied, err = store.ApplyStripeSubscription(ctx, deletedEvent, "customer.subscription.deleted",
		[]byte(`{}`), organizationID, "pro", "canceled", "cus_test", "sub_test", 500*1024*1024*1024, 10)
	if err != nil || !applied {
		t.Fatalf("apply deleted subscription: applied=%v err=%v", applied, err)
	}
	record, err = store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.MaxMachines != free.MaxMachines || record.MaxStorageBytes != free.MaxStorageBytes {
		t.Fatalf("canceled subscription kept paid limits: %+v", record)
	}
	if record.Plan != "pro" || record.Status != "canceled" {
		t.Fatalf("canceled subscription lost its history: %+v", record)
	}

	// Idempotency: a redelivered event must not double-apply.
	applied, err = store.ApplyStripeSubscription(ctx, deletedEvent, "customer.subscription.deleted",
		[]byte(`{}`), organizationID, "pro", "canceled", "cus_test", "sub_test", 500*1024*1024*1024, 10)
	if err != nil || applied {
		t.Fatalf("redelivered event applied=%v err=%v", applied, err)
	}
}

// TestApplyStripeSubscriptionPlanSwitchAppliesNewLimits encodes the
// grandfather contract precisely: the quota-revision floor shields only a
// same-plan re-sync carrying the catalog's own value. A deliberate plan
// switch applies the new plan's limits exactly, and an explicit custom
// ceiling (support or test tooling) always lands as written — a Pro
// customer who switches to Free gets Free capacity, never grandfathered
// Pro capacity.
func TestApplyStripeSubscriptionPlanSwitchAppliesNewLimits(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "entitlement-plan-switch")
	free, _ := billing.FindPlan("free")
	pro, ok := billing.FindPlan("pro")
	if !ok {
		t.Fatal("pro plan missing from catalog")
	}

	// Explicit custom ceiling above the catalog's Pro value.
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "ent-switch-pro"), "customer.subscription.created",
		[]byte(`{}`), organizationID, "pro", "active", "cus_test", "sub_switch", pro.MaxStorageBytes+200*1024*1024*1024, 10); err != nil {
		t.Fatal(err)
	}
	// Same-plan re-sync carrying the catalog value: the floor keeps the
	// larger grandfathered ceiling.
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "ent-switch-resync"), "customer.subscription.updated",
		[]byte(`{}`), organizationID, "pro", "active", "cus_test", "sub_switch", pro.MaxStorageBytes, 10); err != nil {
		t.Fatal(err)
	}
	record, err := store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.MaxStorageBytes != pro.MaxStorageBytes+200*1024*1024*1024 {
		t.Fatalf("catalog re-sync dropped grandfathered ceiling: %+v", record)
	}
	// Same-plan explicit custom limit: applied exactly even though it
	// shrinks capacity (this is how support sets a precise ceiling).
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "ent-switch-custom"), "customer.subscription.updated",
		[]byte(`{}`), organizationID, "pro", "active", "cus_test", "sub_switch", 4096, 10); err != nil {
		t.Fatal(err)
	}
	record, err = store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.MaxStorageBytes != 4096 {
		t.Fatalf("explicit custom limit was grandfathered instead of applied: %+v", record)
	}
	// Deliberate downgrade to free while active: limits shrink to free's.
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "ent-switch-free"), "customer.subscription.updated",
		[]byte(`{}`), organizationID, "free", "active", "", "", free.MaxStorageBytes, free.MaxMachines); err != nil {
		t.Fatal(err)
	}
	record, err = store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.MaxStorageBytes != free.MaxStorageBytes || record.MaxMachines != free.MaxMachines {
		t.Fatalf("active plan switch kept grandfathered capacity: %+v", record)
	}
	if record.Plan != "free" {
		t.Fatalf("plan switch lost the plan label: %+v", record)
	}
}

// TestCanceledSubscriptionCannotRegisterMachinesOverFreeLimits walks the
// real enforcement point (CreateMachine reads max_machines) and proves a
// cancelled plan cannot hold paid capacity. PostgreSQL-gated.
func TestCanceledSubscriptionCannotRegisterMachinesOverFreeLimits(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "entitlement-machines")
	free, _ := billing.FindPlan("free")

	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "mach-active"), "customer.subscription.created",
		[]byte(`{}`), organizationID, "pro", "active", "", "", 500*1024*1024*1024, 3); err != nil {
		t.Fatal(err)
	}
	register := func(label string) error {
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.CreateMachine(ctx, MachineRecord{ID: id, OrganizationID: organizationID, MachineID: "mach-" + label, Name: label, AgentURL: "https://" + label + ".example:8443", Capabilities: []byte(`{}`)}, testAudit(organizationID, "machine.create", id))
		return err
	}
	for _, label := range []string{"one", "two", "three"} {
		if err := register(label); err != nil {
			t.Fatalf("paid plan refused machine %s: %v", label, err)
		}
	}
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "mach-deleted"), "customer.subscription.deleted",
		[]byte(`{}`), organizationID, "pro", "canceled", "", "", 500*1024*1024*1024, 3); err != nil {
		t.Fatal(err)
	}
	if err := register("four"); err == nil {
		t.Fatal("canceled subscription registered a machine beyond the free limit")
	} else if err != ErrEntitlementExceeded {
		t.Fatalf("want ErrEntitlementExceeded, got %v", err)
	}
	// The organization already holds 3 (> free's 2): the free ceiling is
	// enforced from the next admission, exactly like a downgrade.
	_ = free
}

// TestRecordTransferBytesMetersBandwidth covers the cost-side metering API:
// channel validation, non-negative bytes, and that the rows are queryable
// through the billing summary. PostgreSQL-gated.
func TestRecordTransferBytesMetersBandwidth(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "usage-transfer")

	if err := store.RecordTransferBytes(ctx, organizationID, TransferChannelRelay, 1024); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTransferBytes(ctx, organizationID, TransferChannelEgress, 2048); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordTransferBytes(ctx, organizationID, "smuggled", 1); err == nil {
		t.Fatal("unknown transfer channel accepted")
	}
	if err := store.RecordTransferBytes(ctx, organizationID, TransferChannelRelay, -5); err == nil {
		t.Fatal("negative transfer bytes accepted")
	}
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().AddDate(0, 0, 1)
	total, err := store.TransferBytes(ctx, organizationID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3072 {
		t.Fatalf("transfer bytes total = %d, want 3072", total)
	}
}

// registerSeatUser creates a throwaway user the member-ceiling test can add
// to its organization. PostgreSQL-gated like the rest of this file.
func registerSeatUser(t *testing.T, store *Store, label string) string {
	t.Helper()
	ctx := context.Background()
	userID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	organizationID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	email := "seat-" + label + "-" + userID + "@example.test"
	audit := AuditInput{ID: "audit-" + userID, OrganizationID: organizationID, ActorUserID: userID, Action: "user.register", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{}}
	if _, _, err := store.Register(ctx, userID, email, "x", "Seat "+label, organizationID, "Seat "+label+" org", audit); err != nil {
		t.Fatal(err)
	}
	return email
}

// TestAddMemberSeatCeiling proves the per-seat member ceiling: a recorded
// seat purchase bounds membership inside the insert transaction, a role
// change on an existing member never counts against it, and no recorded
// purchase (seats = 0) enforces no ceiling.
func TestAddMemberSeatCeiling(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "seat-ceiling")

	if err := store.SetEntitlementSeats(ctx, organizationID, 2, "si_seat_test"); err != nil {
		t.Fatal(err)
	}
	alice := registerSeatUser(t, store, "alice")
	bob := registerSeatUser(t, store, "bob")

	// Two seats: the registrant's owner row is the first, so exactly one
	// more member fits.
	if err := store.AddMember(ctx, organizationID, alice, "viewer", 2, testAudit(organizationID, "organization.member_upsert", alice)); err != nil {
		t.Fatalf("member within seat ceiling rejected: %v", err)
	}
	if err := store.AddMember(ctx, organizationID, alice, "admin", 2, testAudit(organizationID, "organization.member_upsert", alice)); err != nil {
		t.Fatalf("role change on existing member rejected: %v", err)
	}
	if err := store.AddMember(ctx, organizationID, bob, "viewer", 2, testAudit(organizationID, "organization.member_upsert", bob)); !errors.Is(err, ErrSeatCeilingExceeded) {
		t.Fatalf("member beyond seat ceiling: err = %v, want ErrSeatCeilingExceeded", err)
	}
	// No recorded purchase means no ceiling.
	if err := store.AddMember(ctx, organizationID, bob, "viewer", 0, testAudit(organizationID, "organization.member_upsert", bob)); err != nil {
		t.Fatalf("member without seat purchase rejected: %v", err)
	}
}

// TestTrialActiveFailsClosedWithoutWebhook is the trial-expiry contract:
// reads compare trial_expires_at to now(), so expiry reverts capability the
// moment it passes without any webhook arriving.
func TestTrialActiveFailsClosedWithoutWebhook(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)
	expired := EntitlementRecord{Plan: "free", Status: "active", TrialExpiresAt: &past}
	if expired.TrialActive() {
		t.Fatal("expired trial reported active")
	}
	if expired.Allow(billing.FeatureLiveMigration) {
		t.Fatal("expired trial granted live migration")
	}
	running := EntitlementRecord{Plan: "free", Status: "active", TrialExpiresAt: &future}
	if !running.TrialActive() {
		t.Fatal("running trial reported inactive")
	}
	// The trial grants Business-level features while showing "free · trial":
	// the stored plan stays free; the trial window grants the business set.
	if !running.Allow(billing.FeatureLiveMigration) || !running.Allow(billing.FeatureSSO) {
		t.Fatal("running trial should grant business features")
	}
	// A real subscription decision still wins: cancelled + expired trial
	// grants nothing.
	dead := EntitlementRecord{Plan: "business", Status: "canceled", TrialExpiresAt: &future}
	if dead.Allow(billing.FeatureSSO) {
		t.Fatal("canceled subscription allowed SSO via trial marker")
	}
}

// TestEffectiveLimitsGateMatrix: trial > active subscription > free fallback.
func TestEffectiveLimitsGateMatrix(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour)
	past := time.Now().UTC().Add(-time.Hour)
	trial := EntitlementRecord{Plan: "free", Status: "active", TrialExpiresAt: &future, MaxStorageBytes: 10, MaxMachines: 2}
	storage, machines := trial.EffectiveLimits()
	if machines != -1 || storage != 600*1024*1024*1024 {
		t.Fatalf("trial limits = (%d, %d), want business-level (600 GiB, unlimited)", storage, machines)
	}
	active := EntitlementRecord{Plan: "pro", Status: "active", MaxStorageBytes: 42, MaxMachines: 7}
	if s, m := active.EffectiveLimits(); s != 42 || m != 7 {
		t.Fatalf("active limits = (%d, %d), want stored", s, m)
	}
	lapsed := EntitlementRecord{Plan: "pro", Status: "canceled", MaxStorageBytes: 42, MaxMachines: 7, TrialExpiresAt: &past}
	if s, m := lapsed.EffectiveLimits(); s != 10*1024*1024*1024 || m != 2 {
		t.Fatalf("lapsed limits = (%d, %d), want free fallback", s, m)
	}
}
