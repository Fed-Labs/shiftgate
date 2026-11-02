package database

import (
	"context"
	"os"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

func TestOwnershipEpochRejectsConcurrentOwner(t *testing.T) {
	url := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL ownership integration test")
	}
	ctx := context.Background()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	org, _ := model.NewID()
	user, _ := model.NewID()
	workload, _ := model.NewID()
	if _, _, err := store.Register(ctx, user, "ownership-"+org+"@example.test", "x", "Owner", org, "Ownership", AuditInput{ID: "audit-" + org, Action: "test.register", ResourceType: "test", ResourceID: org}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkload(ctx, WorkloadRecord{ID: workload, OrganizationID: org, Name: "owned", Spec: []byte(`{"command":["/bin/true"]}`)}, AuditInput{ID: "audit-w-" + workload, OrganizationID: org, Action: "test.workload", ResourceType: "workload", ResourceID: workload}); err != nil {
		t.Fatal(err)
	}
	first, err := store.AcquireOwnership(ctx, org, workload, "machine-a", "token-a", time.Minute, AuditInput{ID: "audit-o-" + workload, OrganizationID: org, Action: "ownership.acquire", ResourceType: "workload", ResourceID: workload})
	if err != nil || first.Epoch != 1 {
		t.Fatalf("first ownership = %#v, %v", first, err)
	}
	if _, err := store.AcquireOwnership(ctx, org, workload, "machine-b", "token-b", time.Minute, AuditInput{ID: "audit-o2-" + workload, OrganizationID: org, Action: "ownership.acquire", ResourceType: "workload", ResourceID: workload}); err != ErrOwnershipConflict {
		t.Fatalf("second owner error = %v, want ErrOwnershipConflict", err)
	}
	if err := store.RenewOwnership(ctx, workload, "machine-b", "token-b", first.Epoch, time.Minute); err != ErrOwnershipConflict {
		t.Fatalf("stale renew error = %v, want ErrOwnershipConflict", err)
	}
}
