package database

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The migration advisory lock is what keeps two processes — the parallel
// package binaries of a go test run, or two control planes started at once —
// from racing the applied-check against the apply transaction. This test
// proves the serialization deterministically: while another session holds the
// lock, Migrate must block; once it is released, the same call must succeed.
func TestMigrateWaitsForAdvisoryLock(t *testing.T) {
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	blocker, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	// Closing the blocker's session releases the advisory lock even if an
	// assertion fails first, so the suite never deadlocks behind this test.
	t.Cleanup(func() { _ = blocker.Close(context.Background()) })
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		t.Fatal(err)
	}
	blocked, cancelBlocked := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancelBlocked()
	if err := store.Migrate(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Migrate must wait for the advisory lock another session holds, got %v", err)
	}
	if _, err := blocker.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationsUpDownUpRoundTrip reverts every applied migration, then
// re-applies the full chain: the down companions must cleanly mirror their
// ups in reverse order, and the re-applied schema must carry every launch
// artifact (result column, referral uniqueness, billing CHECK constraints,
// hot-query indexes) exactly once. PostgreSQL-gated.
func TestMigrationsUpDownUpRoundTrip(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	var applied int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Fatal("openTestStore applied no migrations")
	}
	if err := store.MigrateDown(ctx, applied); err != nil {
		t.Fatalf("full revert of %d migrations: %v", applied, err)
	}
	var remaining int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("full revert left %d migrations recorded as applied", remaining)
	}
	var exists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='migration_jobs')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("MigrateDown of every step left migration_jobs behind")
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("re-apply after full revert: %v", err)
	}
	var reapplied int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&reapplied); err != nil {
		t.Fatal(err)
	}
	if reapplied != applied {
		t.Fatalf("re-apply restored %d migrations, want %d", reapplied, applied)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second re-apply must be idempotent: %v", err)
	}
	assertReadinessSchema(t, store)
}

// assertReadinessSchema spot-checks the 018 readiness artifacts after a full
// up/down/up cycle: columns, unique indexes, CHECK constraints, and the
// removed dead tables and redundant indexes.
func assertReadinessSchema(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	uniqueIndexExists := func(name string) bool {
		var found bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid WHERE c.relname=$1 AND i.indisunique)`, name).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	plainIndexExists := func(name string) bool {
		var found bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid WHERE c.relname=$1)`, name).Scan(&found); err != nil {
			t.Fatal(err)
		}
		return found
	}
	constraintCount := func(names ...string) int {
		var count int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conname = ANY($1)`, names).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	var resultType string
	if err := store.pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns WHERE table_name='migration_jobs' AND column_name='result'`).Scan(&resultType); err != nil {
		t.Fatalf("migration_jobs.result missing after re-apply: %v", err)
	}
	if resultType != "jsonb" {
		t.Fatalf("migration_jobs.result type = %s, want jsonb", resultType)
	}
	for _, column := range []string{"route", "relay_endpoint"} {
		var found bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='migration_jobs' AND column_name=$1)`, column).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("migration_jobs.%s missing after re-apply", column)
		}
	}
	if !uniqueIndexExists("referral_codes_org_idx") {
		t.Fatal("referral_codes(organization_id) lost its uniqueness")
	}
	if !uniqueIndexExists("referral_events_referred_org_idx") {
		t.Fatal("referral_events(referred_org_id) lost its uniqueness")
	}
	if constraintCount("entitlements_seats_check", "entitlements_seats_included_check", "entitlements_billing_interval_check") != 3 {
		t.Fatal("entitlements CHECK constraints missing after re-apply")
	}
	for _, name := range []string{"migration_jobs_workload_idx", "audit_events_actor_user_idx", "audit_events_resource_id_idx", "api_keys_user_idx", "stripe_events_backlog_idx", "entitlements_trial_expiry_idx", "migration_jobs_result_duration_idx"} {
		if !plainIndexExists(name) {
			t.Fatalf("hot-query index %s missing after re-apply", name)
		}
	}
	if plainIndexExists("migration_events_migration_idx") {
		t.Fatal("redundant migration_events_migration_idx came back")
	}
	var pricingExists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name='pricing_experiments')`).Scan(&pricingExists); err != nil {
		t.Fatal(err)
	}
	if pricingExists {
		t.Fatal("unused pricing_experiments table came back")
	}
}

// TestMigrateStatusCountsPendingUnderLock: the schema-lag probe takes the
// migration advisory lock so its unlocked CREATE TABLE cannot race Migrate's
// catalog creation, and reports zero pending once the chain is applied.
// PostgreSQL-gated.
func TestMigrateStatusCountsPendingUnderLock(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	pending, err := store.MigrateStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("MigrateStatus pending = %d after Migrate, want 0", pending)
	}
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() {
			_, err := store.MigrateStatus(ctx)
			errs <- err
		}()
	}
	for i := 0; i < 5; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent MigrateStatus: %v", err)
		}
	}
}
