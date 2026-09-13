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
