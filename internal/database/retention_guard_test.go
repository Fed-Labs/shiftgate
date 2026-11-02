package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/model"
)

// retention_guard_test.go: the data-loss invariants of the retention sweep
// (report 16 CRITICAL) and the timezone independence of the usage windows
// (report 16 MEDIUM). PostgreSQL-gated like the rest of the database suite.

// seedWorkload creates one machine and one workload for an organization and
// returns the machine name and workload id for seeding checkpoints.
func seedWorkload(t *testing.T, store *Store, organizationID, label string) (string, string) {
	t.Helper()
	ctx := context.Background()
	machineID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	machineName := label + "-machine"
	if _, err := store.CreateMachine(ctx, MachineRecord{ID: machineID, OrganizationID: organizationID, MachineID: machineName, Name: label, AgentURL: "https://" + label + ".example:8443", Capabilities: json.RawMessage(`{}`)}, testAudit(organizationID, "machine.create", machineID)); err != nil {
		t.Fatal(err)
	}
	workloadID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkload(ctx, WorkloadRecord{ID: workloadID, OrganizationID: organizationID, MachineID: machineName, Name: label + "-workload", Spec: json.RawMessage(`{}`)}, testAudit(organizationID, "workload.create", workloadID)); err != nil {
		t.Fatal(err)
	}
	return machineName, workloadID
}

// addCheckpoint stores one checkpoint; an empty parentID makes it a
// standalone full, a named parent makes it an incremental chained to that
// checkpoint (which must already exist and be available).
func addCheckpoint(t *testing.T, store *Store, organizationID, machineName, workloadID, checkpointID, parentID string) {
	t.Helper()
	kind := "full"
	if parentID != "" {
		kind = "incremental"
	}
	if _, err := store.CreateCheckpoint(context.Background(), CheckpointRecord{ID: checkpointID, OrganizationID: organizationID, WorkloadID: workloadID, MachineID: machineName, Kind: kind, ParentID: parentID, Manifest: json.RawMessage(`{}`), StoredBytes: 1024, Status: "available"}, testAudit(organizationID, "checkpoint.create", checkpointID)); err != nil {
		t.Fatal(err)
	}
}

// ageCheckpoints backdates checkpoints oldest-first: the first id gets the
// largest age, so creation order (and therefore "newest") is deterministic.
func ageCheckpoints(t *testing.T, store *Store, baseDays int, checkpointIDs ...string) {
	t.Helper()
	ctx := context.Background()
	for index, checkpointID := range checkpointIDs {
		if _, err := store.pool.Exec(ctx, `UPDATE checkpoints SET created_at=now()-make_interval(days => $2) WHERE id=$1`, checkpointID, baseDays+len(checkpointIDs)-index); err != nil {
			t.Fatal(err)
		}
	}
}

func checkpointStatuses(t *testing.T, store *Store, checkpointIDs ...string) map[string]string {
	t.Helper()
	ctx := context.Background()
	statuses := make(map[string]string, len(checkpointIDs))
	for _, checkpointID := range checkpointIDs {
		var status string
		if err := store.pool.QueryRow(ctx, `SELECT status FROM checkpoints WHERE id=$1`, checkpointID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		statuses[checkpointID] = status
	}
	return statuses
}

// TestEnforceRetentionNeverDeletesSoleCheckpoint: the report 16 CRITICAL
// invariant in its purest form — a workload's only checkpoint survives the
// sweep no matter how old it is. Losing the sole copy destroys the workload's
// restore point and incremental lineage at once.
func TestEnforceRetentionNeverDeletesSoleCheckpoint(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "solecopy")
	sole := "cp-sole-" + organizationID
	machineName, workloadID := seedWorkload(t, store, organizationID, "sole")
	addCheckpoint(t, store, organizationID, machineName, workloadID, sole, "")
	ageCheckpoints(t, store, 100, sole)

	if err := store.SetRetentionPolicy(ctx, RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 365, CheckpointRetentionDays: 7, DeletedStorageRetentionDays: 30}, testAudit(organizationID, "retention.update", organizationID)); err != nil {
		t.Fatal(err)
	}
	sweep, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.CheckpointsMarked != 0 {
		t.Fatalf("sweep marked %d checkpoints, the sole copy must never be one of them", sweep.CheckpointsMarked)
	}
	if statuses := checkpointStatuses(t, store, sole); statuses[sole] != "available" {
		t.Fatalf("sole checkpoint of the workload is %q, want available", statuses[sole])
	}
}

// TestEnforceRetentionKeepsLineageOfSurvivingCheckpoints: an incremental
// chain that a kept checkpoint depends on is never broken — parents (and
// grandparents) of a surviving checkpoint stay available even when every
// member is past its window.
func TestEnforceRetentionKeepsLineageOfSurvivingCheckpoints(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "lineage")
	oldest := "cp-old-" + organizationID
	middle := "cp-mid-" + organizationID
	newest := "cp-new-" + organizationID
	machineName, workloadID := seedWorkload(t, store, organizationID, "lineage")
	addCheckpoint(t, store, organizationID, machineName, workloadID, oldest, "")
	addCheckpoint(t, store, organizationID, machineName, workloadID, middle, oldest)
	addCheckpoint(t, store, organizationID, machineName, workloadID, newest, middle)
	ageCheckpoints(t, store, 30, oldest, middle, newest)

	if err := store.SetRetentionPolicy(ctx, RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 365, CheckpointRetentionDays: 7, DeletedStorageRetentionDays: 30}, testAudit(organizationID, "retention.update", organizationID)); err != nil {
		t.Fatal(err)
	}
	sweep, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.CheckpointsMarked != 0 {
		t.Fatalf("sweep marked %d checkpoints of an incremental chain the newest copy needs: %v", sweep.CheckpointsMarked, checkpointStatuses(t, store, oldest, middle, newest))
	}
	for checkpointID, status := range checkpointStatuses(t, store, oldest, middle, newest) {
		if status != "available" {
			t.Fatalf("chain member %s is %q, want available", checkpointID, status)
		}
	}
}

// TestEnforceRetentionZeroWindowsKeepForever: zero is the deliberate
// keep-forever policy, not "delete immediately" — with every window zero the
// sweep touches no audit rows, checkpoints, or storage objects of the
// organization, however old they are. The zero-retention-days boundary
// (report 16) is exactly this contract.
func TestEnforceRetentionZeroWindowsKeepForever(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "zerowindow")
	old := "cp-z-old-" + organizationID
	newest := "cp-z-new-" + organizationID
	machineName, workloadID := seedWorkload(t, store, organizationID, "zerowindow")
	addCheckpoint(t, store, organizationID, machineName, workloadID, old, "")
	addCheckpoint(t, store, organizationID, machineName, workloadID, newest, "")
	ageCheckpoints(t, store, 1000, old, newest)
	if _, err := store.pool.Exec(ctx, `UPDATE audit_events SET created_at=now()-interval '10 years' WHERE organization_id=$1`, organizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO storage_objects(id,organization_id,bytes,object_count,state,deleted_at)
		VALUES($1,$2,1024,1,'deleted',now()-interval '10 years')`, "storage-"+organizationID, organizationID); err != nil {
		t.Fatal(err)
	}

	if err := store.SetRetentionPolicy(ctx, RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 0, CheckpointRetentionDays: 0, DeletedStorageRetentionDays: 0}, testAudit(organizationID, "retention.update", organizationID)); err != nil {
		t.Fatal(err)
	}
	sweep, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.CheckpointsMarked != 0 || sweep.StorageObjectsPurged != 0 {
		t.Fatalf("zero windows must sweep nothing: %+v", sweep)
	}
	var auditRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE organization_id=$1`, organizationID).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows == 0 {
		t.Fatal("zero audit window deleted audit rows")
	}
	for checkpointID, status := range checkpointStatuses(t, store, old, newest) {
		if status != "available" {
			t.Fatalf("zero checkpoint window deleted %s (%q)", checkpointID, status)
		}
	}
}

// TestEnforceRetentionDefaultsApplyWithoutPolicyRow: an organization that
// never touched the retention API is held to the documented defaults (audit
// 365, checkpoints 90, deleted storage 30) — the windows GET advertises are
// the windows that run (report 16 MEDIUM) — and the keep-newest guard still
// holds under the defaults.
func TestEnforceRetentionDefaultsApplyWithoutPolicyRow(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "defaultsweep")
	solo := "cp-d-solo-" + organizationID
	soloMachine, soloWorkload := seedWorkload(t, store, organizationID, "defaultsolo")
	addCheckpoint(t, store, organizationID, soloMachine, soloWorkload, solo, "")
	old := "cp-d-old-" + organizationID
	newest := "cp-d-new-" + organizationID
	pairMachine, pairWorkload := seedWorkload(t, store, organizationID, "defaultpair")
	addCheckpoint(t, store, organizationID, pairMachine, pairWorkload, old, "")
	addCheckpoint(t, store, organizationID, pairMachine, pairWorkload, newest, "")
	ageCheckpoints(t, store, 100, solo)
	ageCheckpoints(t, store, 100, old, newest)
	if _, err := store.pool.Exec(ctx, `UPDATE audit_events SET created_at=now()-interval '400 days' WHERE organization_id=$1`, organizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO storage_objects(id,organization_id,bytes,object_count,state,deleted_at)
		VALUES($1,$2,1024,1,'deleted',now()-interval '40 days')`, "storage-"+organizationID, organizationID); err != nil {
		t.Fatal(err)
	}

	sweep, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.CheckpointsMarked != 1 {
		t.Fatalf("default 90-day window should mark exactly the aged non-newest checkpoint, got %d", sweep.CheckpointsMarked)
	}
	if sweep.AuditEventsDeleted == 0 {
		t.Fatal("documented 365-day audit default did not run for a policy-less organization")
	}
	if sweep.StorageObjectsPurged == 0 {
		t.Fatal("documented 30-day deleted-storage default did not run for a policy-less organization")
	}
	statuses := checkpointStatuses(t, store, solo, old, newest)
	if statuses[solo] != "available" {
		t.Fatalf("defaults deleted a workload's sole checkpoint: %q", statuses[solo])
	}
	if statuses[old] != "deleted" {
		t.Fatalf("aged non-newest checkpoint not swept under defaults: %q", statuses[old])
	}
	if statuses[newest] != "available" {
		t.Fatalf("defaults deleted the workload's newest checkpoint: %q", statuses[newest])
	}
}

// openStoreInZone opens an isolated test database whose sessions run in the
// given PostgreSQL TimeZone, for proving the usage windows do not drift with
// the session clock.
func openStoreInZone(t *testing.T, zone string) *Store {
	t.Helper()
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	adminConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	name := "shift_test_" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(suffix))
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		_, _ = admin.Exec(dropCtx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	config.ConnConfig.RuntimeParams = map[string]string{"TimeZone": zone}
	store, err := OpenWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var seen string
	if err := store.pool.QueryRow(context.Background(), `SHOW timezone`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	if seen != zone {
		t.Fatalf("test store session TimeZone = %q, want %q", seen, zone)
	}
	return store
}

// TestTransferWindowIsTimezoneIndependent: the fair-use and usage windows are
// UTC-day aligned no matter what TimeZone the sessions run in. A session east
// of UTC (Pacific/Kiritimati, UTC+14) used to drag the `::date` casts a day
// forward, counting the April 1 event into the March window — boundary events
// landing in the wrong month (report 16 MEDIUM "clock skew"). The window is
// half-open [from, to): the March 31 event is in, February 28 and April 1
// are out.
func TestTransferWindowIsTimezoneIndependent(t *testing.T) {
	store := openStoreInZone(t, "Pacific/Kiritimati")
	ctx := context.Background()
	organizationID := testOrganization(t, store, "tzwindow")
	quantities := map[string]int64{"before": 100, "edge": 200, "after": 400}
	days := map[string]string{"before": "2026-02-28", "edge": "2026-03-31", "after": "2026-04-01"}
	for label, quantity := range quantities {
		id := fmt.Sprintf("tz-%s-%s", label, organizationID)
		if err := store.AddUsage(ctx, UsageRecord{ID: id, OrganizationID: organizationID, Kind: UsageKindTransferBytes, Quantity: quantity, Metadata: json.RawMessage(`{"channel":"egress"}`)}); err != nil {
			t.Fatal(err)
		}
		// The day columns are set from DATE literals so the fixture itself
		// cannot drift with the session clock.
		if _, err := store.pool.Exec(ctx, `UPDATE usage_records SET period_start=$2::date, period_end=$2::date WHERE id=$1`, id, days[label]); err != nil {
			t.Fatal(err)
		}
	}

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	total, err := store.TransferBytes(ctx, organizationID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if total != 200 {
		t.Fatalf("window [2026-03-01, 2026-04-01) counted %d bytes, want 200 (the March 31 event only)", total)
	}
	byChannel, err := store.TransferBytesByChannel(ctx, organizationID, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if byChannel["egress"] != 200 || len(byChannel) != 1 {
		t.Fatalf("by-channel window counted %v, want only the March 31 event", byChannel)
	}
}

// TestCreditReferralMonthGrantsBothParties: the give-a-month-get-a-month
// reward actually credits a month of Pro to both the referring and the
// referred organization — capacity only ever grows (a running trial is never
// downgraded), and the existing release pass returns both parties to their
// earned tier when the month runs out. PostgreSQL-gated.
func TestCreditReferralMonthGrantsBothParties(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Distinct email domains: the self-claim rule refuses same-domain claims,
	// and this fixture is about the reward, not the claim check.
	referrer := registerDomainOrg(t, store, "rewardreferrer", "reward-referrer.test")
	referred := registerDomainOrg(t, store, "rewardreferred", "reward-referred.test")

	// The referring organization runs a Business trial — a reward must never
	// downgrade it to Pro capacity.
	trial, err := store.StartTrial(ctx, referrer, now, "fp-reward", "test")
	if err != nil {
		t.Fatal(err)
	}

	code, err := store.EnsureReferralCode(ctx, referrer)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimReferral(ctx, code.Code, referred); err != nil {
		t.Fatal(err)
	}
	reward, err := store.ActivateReferral(ctx, referred, now)
	if err != nil || !reward {
		t.Fatalf("activation did not report a reward: %v %v", reward, err)
	}
	granted, err := store.GrantReferralReward(ctx, referred)
	if err != nil || !granted {
		t.Fatalf("grant refused: %v %v", granted, err)
	}
	if err := store.CreditReferralMonth(ctx, referred, now); err != nil {
		t.Fatal(err)
	}

	pro, ok := billing.FindPlan("pro")
	if !ok {
		t.Fatal("pro plan missing")
	}
	for label, organizationID := range map[string]string{"referrer": referrer, "referred": referred} {
		var maxStorage, maxMachines int64
		var trialExpires *time.Time
		if err := store.pool.QueryRow(ctx, `SELECT max_storage_bytes,max_machines,trial_expires_at FROM entitlements WHERE organization_id=$1`, organizationID).Scan(&maxStorage, &maxMachines, &trialExpires); err != nil {
			t.Fatal(err)
		}
		if maxStorage < billing.EffectiveStorageBytes(pro, 1) {
			t.Fatalf("%s storage after reward = %d, want at least Pro", label, maxStorage)
		}
		if maxMachines < int64(pro.MaxMachines) {
			t.Fatalf("%s machines after reward = %d, want at least Pro", label, maxMachines)
		}
		if trialExpires == nil || trialExpires.Before(now.AddDate(0, 0, 29)) {
			t.Fatalf("%s reward window = %v, want roughly a month out", label, trialExpires)
		}
	}

	// The Business-trial referrer kept its larger trial capacity.
	business, _ := billing.FindPlan("business")
	var referrerStorage int64
	if err := store.pool.QueryRow(ctx, `SELECT max_storage_bytes FROM entitlements WHERE organization_id=$1`, referrer).Scan(&referrerStorage); err != nil {
		t.Fatal(err)
	}
	if referrerStorage < billing.EffectiveStorageBytes(business, 1) {
		t.Fatalf("reward downgraded the Business-trial organization: %d", referrerStorage)
	}
	if trial.ExpiresAt.IsZero() {
		t.Fatal("trial fixture broken")
	}

	// When the month runs out the release pass returns both parties to the
	// free tier — the reward is a window, not a permanent upgrade.
	released, err := store.ReleaseExpiredTrials(ctx, now.AddDate(0, 0, 31))
	if err != nil {
		t.Fatal(err)
	}
	if released < 2 {
		t.Fatalf("released %d rows, want both reward windows to expire", released)
	}
	free, _ := billing.FindPlan("free")
	for label, organizationID := range map[string]string{"referrer": referrer, "referred": referred} {
		var maxStorage int64
		var trialExpires *time.Time
		if err := store.pool.QueryRow(ctx, `SELECT max_storage_bytes,trial_expires_at FROM entitlements WHERE organization_id=$1`, organizationID).Scan(&maxStorage, &trialExpires); err != nil {
			t.Fatal(err)
		}
		if maxStorage != free.MaxStorageBytes || trialExpires != nil {
			t.Fatalf("%s not released to free tier: storage=%d expiry=%v", label, maxStorage, trialExpires)
		}
	}
}
