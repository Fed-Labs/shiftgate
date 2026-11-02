package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/model"
)

func registerMachineExpecting(ctx context.Context, t *testing.T, store *Store, organizationID, label string) error {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateMachine(ctx, MachineRecord{
		ID: id, OrganizationID: organizationID, MachineID: "mach-" + label, Name: label,
		AgentURL: "https://" + label + ".example:8443", Capabilities: []byte(`{}`),
	}, testAudit(organizationID, "machine.create", id))
	return err
}

// registerDomainOrg creates an organization whose owner email lives in the
// given domain — the referral self-claim rule distinguishes domains.
func registerDomainOrg(t *testing.T, store *Store, label, domain string) string {
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
	email := label + "-" + userID + "@" + domain
	audit := AuditInput{ID: "audit-" + organizationID, OrganizationID: organizationID, ActorUserID: userID, Action: "user.register", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{}}
	if _, _, err := store.Register(ctx, userID, email, "x", "Domain User", organizationID, "Domain Org "+label, audit); err != nil {
		t.Fatal(err)
	}
	return organizationID
}

func uniqueDomain(t *testing.T, label string) string {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return label + "-" + id + ".example.test"
}

// TestReleaseTrialLimitsRevertsExpiredTrialCapacity is the trial-expiry
// contract: a lapsed trial drops back to free-tier machine and storage
// capacity, while a running trial keeps Business capacity. PostgreSQL-gated.
func TestReleaseTrialLimitsRevertsExpiredTrialCapacity(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "trial-release")
	free, _ := billing.FindPlan("free")
	started := time.Now().UTC().AddDate(0, 0, -20)
	if _, err := store.StartTrial(ctx, organizationID, started, "fp-trial-release", "test"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseTrialLimits(ctx, organizationID, started.AddDate(0, 0, 13)); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseTrialLimits(ctx, organizationID, started.AddDate(0, 0, 13)); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"one", "two", "three"} {
		if err := registerMachineExpecting(ctx, t, store, organizationID, "trial-"+label); err != nil {
			t.Fatalf("running trial refused machine %s: %v", label, err)
		}
	}
	if err := store.ReleaseTrialLimits(ctx, organizationID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	released, err := store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if released.MaxMachines != free.MaxMachines || released.MaxStorageBytes != free.MaxStorageBytes || released.TrialExpiresAt != nil {
		t.Fatalf("released entitlement = (machines %d, storage %d, trial %v), want free-tier and no trial marker",
			released.MaxMachines, released.MaxStorageBytes, released.TrialExpiresAt)
	}
	if err := registerMachineExpecting(ctx, t, store, organizationID, "trial-four"); err == nil {
		t.Fatal("expired trial kept unlimited machine registration")
	} else if err != ErrEntitlementExceeded {
		t.Fatalf("want ErrEntitlementExceeded, got %v", err)
	}
	if released, err := store.ReleaseExpiredTrials(ctx, time.Now().UTC()); err != nil || released != 0 {
		t.Fatalf("second sweep = (%d, %v), want (0, nil)", released, err)
	}
}

// TestReleaseTrialLimitsKeepsSubscriptionLimits: an organization whose trial
// converted into a real subscription keeps its subscription capacity — the
// release predicate must never match a row carrying a Stripe subscription id.
// PostgreSQL-gated.
func TestReleaseTrialLimitsKeepsSubscriptionLimits(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "trial-converted")
	started := time.Now().UTC().AddDate(0, 0, -20)
	if _, err := store.StartTrial(ctx, organizationID, started, "fp-trial-converted", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "trial-converted"), "customer.subscription.created",
		[]byte(`{}`), organizationID, "pro", "active", "cus_converted", "sub_converted", 120*1024*1024*1024, 10); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseTrialLimits(ctx, organizationID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReleaseExpiredTrials(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	record, err := store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.MaxMachines != 10 || record.TrialExpiresAt == nil {
		t.Fatalf("subscription entitlement = (machines %d, trial %v), want (10, untouched trial marker)",
			record.MaxMachines, record.TrialExpiresAt)
	}
}

// TestStartTrialConcurrentRefusesWithTrialAlreadyUsed: a double-submitted
// start must surface ErrTrialAlreadyUsed (the 402 contract) to the loser —
// never a raw unique violation. PostgreSQL-gated.
func TestStartTrialConcurrentRefusesWithTrialAlreadyUsed(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "trial-race")
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := store.StartTrial(ctx, organizationID, time.Now().UTC(), "fp-trial-race", "test")
			errs <- err
		}()
	}
	winners := 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err == nil {
			winners++
		} else if !errors.Is(err, ErrTrialAlreadyUsed) {
			t.Fatalf("StartTrial race loser: err = %v, want ErrTrialAlreadyUsed", err)
		}
	}
	if winners != 1 {
		t.Fatalf("StartTrial winners = %d, want exactly 1", winners)
	}
	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM organization_trials WHERE organization_id=$1`, organizationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("organization_trials rows = %d, want 1", rows)
	}
}

// TestClaimReferralOneClaimPerReferredOrg: one referred organization claims
// one code, ever — retries are idempotent, second codes are refused, and the
// unique constraint multiplies nothing. PostgreSQL-gated.
func TestClaimReferralOneClaimPerReferredOrg(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	owner := registerDomainOrg(t, store, "ref-owner", uniqueDomain(t, "ref-owner"))
	codeA, err := store.EnsureReferralCode(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	referred := registerDomainOrg(t, store, "ref-referred", uniqueDomain(t, "ref-referred"))
	if err := store.ClaimReferral(ctx, codeA.Code, referred); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimReferral(ctx, codeA.Code, referred); err != nil {
		t.Fatalf("retried claim of the same code: %v", err)
	}
	other := registerDomainOrg(t, store, "ref-other", uniqueDomain(t, "ref-other"))
	codeB, err := store.EnsureReferralCode(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimReferral(ctx, codeB.Code, referred); !errors.Is(err, ErrReferralAlreadyClaimed) {
		t.Fatalf("second-code claim: err = %v, want ErrReferralAlreadyClaimed", err)
	}
	var events int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM referral_events WHERE referred_org_id=$1`, referred).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("referral_events rows = %d, want 1", events)
	}
	var referredBy string
	if err := store.pool.QueryRow(ctx, `SELECT referred_by FROM organizations WHERE id=$1`, referred).Scan(&referredBy); err != nil {
		t.Fatal(err)
	}
	if referredBy != codeA.Code {
		t.Fatalf("referred_by = %q, want the first claim's code %q", referredBy, codeA.Code)
	}
	if err := store.ClaimReferral(ctx, codeA.Code, owner); !errors.Is(err, ErrReferralSelfClaim) {
		t.Fatalf("same-org claim: err = %v, want ErrReferralSelfClaim", err)
	}
	domain := uniqueDomain(t, "ref-domain")
	sibling := registerDomainOrg(t, store, "ref-sibling", domain)
	otherOwner := registerDomainOrg(t, store, "ref-domain-owner", domain)
	codeC, err := store.EnsureReferralCode(ctx, otherOwner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimReferral(ctx, codeC.Code, sibling); !errors.Is(err, ErrReferralSelfClaim) {
		t.Fatalf("same-domain claim: err = %v, want ErrReferralSelfClaim", err)
	}
}

// TestEnsureReferralCodeSinglePerOrg: concurrent first-use calls converge on
// one code row per organization. PostgreSQL-gated.
func TestEnsureReferralCodeSinglePerOrg(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "ref-ensure")
	const callers = 8
	codes := make(chan string, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			record, err := store.EnsureReferralCode(ctx, organizationID)
			codes <- record.Code
			errs <- err
		}()
	}
	seen := ""
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		code := <-codes
		if seen == "" {
			seen = code
		}
		if code != seen {
			t.Fatalf("org owns codes %q and %q, want exactly one", seen, code)
		}
	}
	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM referral_codes WHERE organization_id=$1`, organizationID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("referral_codes rows = %d, want 1", rows)
	}
}

// TestGrantReferralRewardIdempotentAndCapped: rewards fire once per activated
// claim and stop at the advertised cap of 12 per code. PostgreSQL-gated.
func TestGrantReferralRewardIdempotentAndCapped(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	owner := registerDomainOrg(t, store, "ref-cap-owner", uniqueDomain(t, "ref-cap-owner"))
	code, err := store.EnsureReferralCode(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	activate := func(label string) string {
		t.Helper()
		organizationID := registerDomainOrg(t, store, label, uniqueDomain(t, label))
		if err := store.ClaimReferral(ctx, code.Code, organizationID); err != nil {
			t.Fatal(err)
		}
		if granted, err := store.GrantReferralReward(ctx, organizationID); err != nil || granted {
			t.Fatalf("reward before activation = (%v, %v), want (false, nil)", granted, err)
		}
		if activated, err := store.ActivateReferral(ctx, organizationID, time.Now().UTC()); err != nil || !activated {
			t.Fatalf("activation = (%v, %v), want (true, nil)", activated, err)
		}
		return organizationID
	}
	first := activate("ref-cap-1")
	if granted, err := store.GrantReferralReward(ctx, first); err != nil || !granted {
		t.Fatalf("first reward grant = (%v, %v), want (true, nil)", granted, err)
	}
	if granted, err := store.GrantReferralReward(ctx, first); err != nil || granted {
		t.Fatalf("second reward grant = (%v, %v), want (false, nil)", granted, err)
	}
	for i := 2; i <= ReferralRewardCap+1; i++ {
		organizationID := activate(fmt.Sprintf("ref-cap-%d", i))
		granted, err := store.GrantReferralReward(ctx, organizationID)
		if err != nil {
			t.Fatal(err)
		}
		if want := i <= ReferralRewardCap; granted != want {
			t.Fatalf("reward %d granted = %v, want %v", i, granted, want)
		}
	}
	stats, err := store.ReferralStatsFor(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rewards != ReferralRewardCap || stats.Activated != ReferralRewardCap+1 || stats.Invited != ReferralRewardCap+1 {
		t.Fatalf("stats = %+v, want %d rewards of %d activated", stats, ReferralRewardCap, ReferralRewardCap+1)
	}
}

// TestIncrementAPIRequestsConcurrentCountsEveryRequest: the day gauge must
// not lose concurrent increments to a read-then-upsert race.
// PostgreSQL-gated.
func TestIncrementAPIRequestsConcurrentCountsEveryRequest(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "api-meter")
	now := time.Now().UTC()
	const requests = 25
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.IncrementAPIRequests(ctx, organizationID, now)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	total, err := store.APIRequestsToday(ctx, organizationID, now)
	if err != nil {
		t.Fatal(err)
	}
	if total != requests {
		t.Fatalf("api requests metered = %d, want %d", total, requests)
	}
}

// TestRecordTransferBytesEventIsIdempotent: a retried bandwidth event with a
// caller-derived id is absorbed instead of double-counted. PostgreSQL-gated.
func TestRecordTransferBytesEventIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "transfer-event")
	event := "xf-" + organizationID
	for i := 0; i < 3; i++ {
		if err := store.RecordTransferBytesEvent(ctx, organizationID, TransferChannelRestore, event, 4096); err != nil {
			t.Fatal(err)
		}
	}
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC().AddDate(0, 0, 1)
	total, err := store.TransferBytes(ctx, organizationID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4096 {
		t.Fatalf("retried transfer event metered %d bytes, want 4096", total)
	}
	if err := store.RecordTransferBytesEvent(ctx, organizationID, TransferChannelRestore, "", 1); err == nil {
		t.Fatal("empty transfer event id accepted")
	}
}

// TestMigrationResultDurationFeedsPublicRestoreP50: completing a migration
// writes result.duration_ms and the public stats median reads it back
// instead of a permanently-zero field. PostgreSQL-gated.
func TestMigrationResultDurationFeedsPublicRestoreP50(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "result-p50")
	owner := testOrganizationOwner(t, store, organizationID)
	source := createRegressionMachine(t, store, organizationID, "result-source")
	destination := createRegressionMachine(t, store, organizationID, "result-destination")
	workloadID := mustRegressionID(t)
	if _, err := store.UpsertWorkload(ctx, WorkloadRecord{
		ID: workloadID, OrganizationID: organizationID, MachineID: source,
		Name: "result-workload", Spec: json.RawMessage(`{"name":"result-workload"}`),
	}, testAudit(organizationID, "workload.upsert", workloadID)); err != nil {
		t.Fatal(err)
	}
	migration, created, err := store.CreateMigration(ctx, MigrationRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, WorkloadID: workloadID,
		SourceMachineID: source, DestinationMachineID: destination,
		Mode: "cold", Route: "relay", RelayEndpoint: "wss://relay.example.test/leg",
		CreatedBy: owner, Managed: true,
	}, testAudit(organizationID, "migration.create", "result"))
	if err != nil || !created {
		t.Fatalf("migration create = (%v, %v)", created, err)
	}
	stored, err := store.Migration(ctx, organizationID, migration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Route != "relay" || stored.RelayEndpoint != "wss://relay.example.test/leg" {
		t.Fatalf("route fields = (%q, %q), want the created values", stored.Route, stored.RelayEndpoint)
	}
	if _, err := store.UpdateMigrationStatus(ctx, organizationID, migration.ID, "completed", json.RawMessage(`{}`), "",
		testAudit(organizationID, "migration.complete", migration.ID)); err != nil {
		t.Fatal(err)
	}
	var duration *int64
	if err := store.pool.QueryRow(ctx, `SELECT (result->>'duration_ms')::bigint FROM migration_jobs WHERE id=$1`, migration.ID).Scan(&duration); err != nil {
		t.Fatal(err)
	}
	if duration == nil {
		t.Fatal("completed migration has no result.duration_ms")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE migration_jobs SET result=jsonb_set(result,'{duration_ms}','1234'::jsonb) WHERE id=$1`, migration.ID); err != nil {
		t.Fatal(err)
	}
	stats, err := store.PublicStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.RestoreP50MS != 1234 {
		t.Fatalf("restore_p50_ms = %d, want 1234", stats.RestoreP50MS)
	}
}

// TestScoreboardMRRUsesSeatOverageFormulaAndEntitledStatuses: a 15-seat
// Business org bills $289 (9900 + 1900×10), never 15×9900, and trialing
// subscriptions count as entitled. PostgreSQL-gated.
func TestScoreboardMRRUsesSeatOverageFormulaAndEntitledStatuses(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "mrr-business")
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "mrr-business"), "customer.subscription.created",
		[]byte(`{}`), organizationID, "business", "active", "cus_mrr", "sub_mrr", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEntitlementSeats(ctx, organizationID, 15, "si_mrr"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEntitlementExtras(ctx, organizationID, "month", 5, false, false, 0, false); err != nil {
		t.Fatal(err)
	}
	trialOrg := testOrganization(t, store, "mrr-trialing")
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "mrr-trialing"), "customer.subscription.created",
		[]byte(`{}`), trialOrg, "pro", "trialing", "cus_mrr2", "sub_mrr2", 1, 1); err != nil {
		t.Fatal(err)
	}
	counters, err := store.Scoreboard(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if counters.MRRCents != 28900+2900 {
		t.Fatalf("mrr_cents = %d, want %d (business 15 seats + trialing pro)", counters.MRRCents, 28900+2900)
	}
	if counters.ActiveSubscriptions["business"] != 1 || counters.ActiveSubscriptions["pro"] != 1 {
		t.Fatalf("active_subscriptions = %v, want business=1 pro=1 with trialing included", counters.ActiveSubscriptions)
	}
}

// TestSCIMCreateUserStopsAtSeatCeiling: SCIM provisioning enforces the same
// IncludedSeats-aware ceiling the invite route computes, with or without a
// recorded seat purchase. PostgreSQL-gated.
func TestSCIMCreateUserStopsAtSeatCeiling(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "scim-seats")
	if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "scim-seats"), "customer.subscription.created",
		[]byte(`{}`), organizationID, "business", "active", "cus_scim", "sub_scim", 1, -1); err != nil {
		t.Fatal(err)
	}
	run := time.Now().UnixNano()
	add := func(label string) error {
		t.Helper()
		_, err := store.SCIMCreateUser(ctx, organizationID, SCIMUser{
			Email: fmt.Sprintf("scim-seat-%s-%d@example.test", label, run), DisplayName: label,
			ExternalID: fmt.Sprintf("ext-seat-%s-%d", label, run),
		}, testAudit(organizationID, "scim.user.create", label))
		return err
	}
	for _, label := range []string{"one", "two", "three", "four"} {
		if err := add(label); err != nil {
			t.Fatalf("SCIM user within included seats rejected: %v", err)
		}
	}
	if err := add("five"); !errors.Is(err, ErrSeatCeilingExceeded) {
		t.Fatalf("SCIM user past included seats: err = %v, want ErrSeatCeilingExceeded", err)
	}
	if err := store.SetEntitlementSeats(ctx, organizationID, 7, "si_scim_extra"); err != nil {
		t.Fatal(err)
	}
	if err := add("five"); err != nil {
		t.Fatalf("SCIM user within purchased seats rejected: %v", err)
	}
	if err := add("six"); err != nil {
		t.Fatalf("SCIM user within purchased seats rejected: %v", err)
	}
	if err := add("seven"); !errors.Is(err, ErrSeatCeilingExceeded) {
		t.Fatalf("SCIM user past purchased seats: err = %v, want ErrSeatCeilingExceeded", err)
	}
}

// TestEnsureSSOMembershipsRespectsSeatCeiling: SSO auto-join runs the seat
// gate and never over-admits — a full organization is skipped while a
// separate login still joins an organization that has room. PostgreSQL-gated.
func TestEnsureSSOMembershipsRespectsSeatCeiling(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	run := time.Now().UnixNano()
	full := testOrganization(t, store, "sso-full")
	roomy := testOrganization(t, store, "sso-roomy")
	fullDomain := fmt.Sprintf("sso-full-%d.example.test", run)
	roomyDomain := fmt.Sprintf("sso-roomy-%d.example.test", run)
	for organizationID, domain := range map[string]string{full: fullDomain, roomy: roomyDomain} {
		if _, err := store.ApplyStripeSubscription(ctx, uniqueEventID(t, "sso-"+organizationID), "customer.subscription.created",
			[]byte(`{}`), organizationID, "business", "active", "cus_sso", "sub_sso_"+organizationID, 1, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE organizations SET sso_enforced=true, sso_email_domain=$2 WHERE id=$1`, organizationID, domain); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		member := registerSeatUser(t, store, fmt.Sprintf("sso-full-%d", i))
		if err := store.AddMember(ctx, full, member, "viewer", 5, testAudit(full, "organization.member_upsert", member)); err != nil {
			t.Fatalf("filling member %d: %v", i, err)
		}
	}
	join := func(label, domain string) string {
		t.Helper()
		userID, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		email := fmt.Sprintf("sso-joiner-%s-%d@%s", label, run, domain)
		joinerOrg, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		audit := AuditInput{ID: "audit-" + userID, OrganizationID: joinerOrg, ActorUserID: userID, Action: "user.register", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{}}
		if _, _, err := store.Register(ctx, userID, email, "x", "SSO Joiner", joinerOrg, "SSO Joiner Org", audit); err != nil {
			t.Fatal(err)
		}
		tx, err := store.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := ensureSSOMemberships(ctx, tx, UserRecord{ID: userID, Email: email}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return userID
	}
	fullJoiner := join("full", fullDomain)
	roomyJoiner := join("roomy", roomyDomain)
	memberOf := func(organizationID, userID string) bool {
		t.Helper()
		var found bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_members WHERE organization_id=$1 AND user_id=$2)`, organizationID, userID).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	if memberOf(full, fullJoiner) {
		t.Fatal("SSO auto-join admitted a member past the seat ceiling")
	}
	if !memberOf(roomy, roomyJoiner) {
		t.Fatal("SSO auto-join skipped an organization with seats left")
	}
}
