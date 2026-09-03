package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// openTestStore connects to the integration database and applies migrations.
// The whole file skips without SHIFT_TEST_DATABASE_URL: the projection and the
// retention sweep are SQL behavior, and SQL behavior needs a real PostgreSQL.
func openTestStore(t *testing.T) *Store {
	t.Helper()
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
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return store
}

// testOrganization registers a throwaway organization and returns its id.
func testOrganization(t *testing.T, store *Store, label string) string {
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
	email := fmt.Sprintf("dbtest-%s-%d@example.test", label, time.Now().UnixNano())
	audit := AuditInput{ID: "audit-" + organizationID, OrganizationID: organizationID, ActorUserID: userID, Action: "user.register", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{}}
	if _, _, err := store.Register(ctx, userID, email, "x", "Test User", organizationID, "Test Organization "+label, audit); err != nil {
		t.Fatal(err)
	}
	return organizationID
}

func testAudit(organizationID, action, resourceID string) AuditInput {
	return AuditInput{ID: "audit-" + resourceID, OrganizationID: organizationID, Action: action, ResourceType: "machine", ResourceID: resourceID, Metadata: map[string]any{}}
}

// TestMachineCapabilityProjection proves registration and heartbeat keep the
// relational projection in step with the JSONB document, including removals —
// a stale row would answer fleet queries with hardware the machine no longer
// reports.
func TestMachineCapabilityProjection(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "capabilities")

	register := func(machineID string, capabilities map[string]any) MachineRecord {
		t.Helper()
		document, err := json.Marshal(capabilities)
		if err != nil {
			t.Fatal(err)
		}
		id, err := model.NewID()
		if err != nil {
			t.Fatal(err)
		}
		record, err := store.CreateMachine(ctx, MachineRecord{ID: id, OrganizationID: organizationID, MachineID: machineID, Name: machineID, AgentURL: "https://" + machineID + ".example:8443", Capabilities: document}, testAudit(organizationID, "machine.create", id))
		if err != nil {
			t.Fatal(err)
		}
		return record
	}

	register("gpu-a", map[string]any{"cuda_version": "12.4", "gpu_count": 4.0, "supports_live_migration": true, "nested": map[string]any{"ignored": true}})
	register("gpu-b", map[string]any{"cuda_version": "11.8", "gpu_count": 1.0, "supports_live_migration": false})
	register("cpu-only", map[string]any{"architecture": "arm64"})

	// Numbers: every machine with gpu_count, best first, and a floor at 2.
	numbered, err := store.MachinesWithCapability(ctx, organizationID, "gpu_count", "number", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(numbered) != 2 || numbered[0].MachineID != "gpu-a" || numbered[1].MachineID != "gpu-b" {
		t.Fatalf("numeric capability lookup returned %v", machineIDs(numbered))
	}
	floor := 2.0
	numbered, err = store.MachinesWithCapability(ctx, organizationID, "gpu_count", "number", &floor)
	if err != nil {
		t.Fatal(err)
	}
	if len(numbered) != 1 || numbered[0].MachineID != "gpu-a" {
		t.Fatalf("numeric minimum filter returned %v", machineIDs(numbered))
	}

	// Text: exact match.
	textual, err := store.MachinesWithCapability(ctx, organizationID, "cuda_version", "text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(textual) != 2 {
		t.Fatalf("text capability lookup returned %v", machineIDs(textual))
	}

	// Booleans: only the machine that reports it set.
	flagged, err := store.MachinesWithCapability(ctx, organizationID, "supports_live_migration", "boolean", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(flagged) != 1 || flagged[0].MachineID != "gpu-a" {
		t.Fatalf("boolean capability lookup returned %v", machineIDs(flagged))
	}

	// Objects are not flattened: the nested document leaves no rows behind.
	if _, err := store.MachinesWithCapability(ctx, organizationID, "nested", "text", nil); err != nil {
		t.Fatalf("objects must stay JSONB-only, not error: %v", err)
	}

	// A heartbeat that drops a capability must remove its projection rows.
	updated, err := store.UpdateMachineHeartbeat(ctx, organizationID, "gpu-a", "", "", json.RawMessage(`{"gpu_count":2.0}`), "online")
	if err != nil {
		t.Fatal(err)
	}
	if string(updated.Capabilities) != `{"gpu_count":2.0}` {
		t.Fatalf("heartbeat did not replace the document: %s", updated.Capabilities)
	}
	flagged, err = store.MachinesWithCapability(ctx, organizationID, "cuda_version", "text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(flagged) != 1 || flagged[0].MachineID != "gpu-b" {
		t.Fatalf("dropped capability still answers queries: %v", machineIDs(flagged))
	}

	// A heartbeat with no document leaves the projection untouched.
	if _, err := store.UpdateMachineHeartbeat(ctx, organizationID, "gpu-a", "", "", nil, "online"); err != nil {
		t.Fatal(err)
	}
	numbered, err = store.MachinesWithCapability(ctx, organizationID, "gpu_count", "number", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(numbered) != 2 || numbered[0].MachineID != "gpu-a" {
		t.Fatalf("presence-only heartbeat changed the projection: %v", machineIDs(numbered))
	}

	// Unknown kind is a caller error, not an empty result.
	if _, err := store.MachinesWithCapability(ctx, organizationID, "gpu_count", "array", nil); err == nil {
		t.Fatal("an unknown capability kind must be rejected")
	}
}

func machineIDs(records []MachineRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.MachineID)
	}
	return ids
}

// TestRetentionPolicyLifecycle covers the admin surface: defaults when nothing
// was configured, a partial update that leaves unnamed windows alone, and the
// keep-forever zero passing through as a real value.
func TestRetentionPolicyLifecycle(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "retention")

	policy, err := store.RetentionPolicy(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.AuditRetentionDays != 365 || policy.CheckpointRetentionDays != 90 || policy.DeletedStorageRetentionDays != 30 {
		t.Fatalf("unexpected defaults: %+v", policy)
	}

	// Partial update: only the audit window is named.
	if err := store.SetRetentionPolicy(ctx, RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 30, CheckpointRetentionDays: 90, DeletedStorageRetentionDays: 30}, testAudit(organizationID, "retention.update", organizationID)); err != nil {
		t.Fatal(err)
	}
	policy, err = store.RetentionPolicy(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.AuditRetentionDays != 30 {
		t.Fatalf("audit window not stored: %+v", policy)
	}

	// Keep-forever is a stored zero, not an unset field.
	if err := store.SetRetentionPolicy(ctx, RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 0, CheckpointRetentionDays: 90, DeletedStorageRetentionDays: 30}, testAudit(organizationID, "retention.update", organizationID)); err != nil {
		t.Fatal(err)
	}
	policy, err = store.RetentionPolicy(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.AuditRetentionDays != 0 {
		t.Fatalf("keep-forever not stored: %+v", policy)
	}

	// The policy change is audited alongside its effect.
	events, err := store.AuditEvents(ctx, organizationID, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Action == "retention.update" {
			found = true
		}
	}
	if !found {
		t.Fatal("retention policy change was not audited")
	}
}

// TestEnforceRetentionSweep proves one sweep applies every window: audit rows
// past theirs are deleted, checkpoints past theirs are marked deleted (the
// row stays — lineage survives), and storage objects already soft-deleted
// past their grace window are purged.
func TestEnforceRetentionSweep(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "sweep")

	// A machine, workload, and checkpoint to age out.
	machineID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMachine(ctx, MachineRecord{ID: machineID, OrganizationID: organizationID, MachineID: "sweep-machine", Name: "Sweep", AgentURL: "https://sweep.example:8443", Capabilities: json.RawMessage(`{}`)}, testAudit(organizationID, "machine.create", machineID)); err != nil {
		t.Fatal(err)
	}
	workloadID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkload(ctx, WorkloadRecord{ID: workloadID, OrganizationID: organizationID, MachineID: "sweep-machine", Name: "sweep-workload", Spec: json.RawMessage(`{}`)}, testAudit(organizationID, "workload.create", workloadID)); err != nil {
		t.Fatal(err)
	}
	checkpointID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateCheckpoint(ctx, CheckpointRecord{ID: checkpointID, OrganizationID: organizationID, WorkloadID: workloadID, MachineID: "sweep-machine", Kind: "full", Manifest: json.RawMessage(`{}`), StoredBytes: 1024, Status: "available"}, testAudit(organizationID, "checkpoint.create", checkpointID)); err != nil {
		t.Fatal(err)
	}
	// A storage object already soft-deleted, waiting out its grace window.
	if _, err := store.pool.Exec(ctx, `INSERT INTO storage_objects(id,organization_id,workload_id,checkpoint_id,bytes,object_count,state,deleted_at)
		VALUES($1,$2,$3,$4,1024,1,'deleted',now())`, "storage-"+checkpointID, organizationID, workloadID, checkpointID); err != nil {
		t.Fatal(err)
	}

	// Windows of zero days: everything this test created is already past them.
	if err := store.SetRetentionPolicy(ctx, RetentionPolicy{OrganizationID: organizationID, AuditRetentionDays: 0, CheckpointRetentionDays: 0, DeletedStorageRetentionDays: 0}, testAudit(organizationID, "retention.update", organizationID)); err != nil {
		t.Fatal(err)
	}
	sweep, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.AuditEventsDeleted == 0 {
		t.Fatal("sweep deleted no audit rows")
	}
	if sweep.CheckpointsMarked == 0 {
		t.Fatal("sweep marked no checkpoints")
	}
	if sweep.StorageObjectsPurged == 0 {
		t.Fatal("sweep purged no storage objects")
	}

	// The checkpoint row survives, marked; the storage object row is gone.
	checkpoints, err := store.Checkpoints(ctx, organizationID, workloadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 || checkpoints[0].Status != "deleted" || checkpoints[0].DeletedAt == nil {
		t.Fatalf("checkpoint not marked deleted: %+v", checkpoints)
	}
	var remaining int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM storage_objects WHERE organization_id=$1`, organizationID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d storage objects survived the purge", remaining)
	}

	// A second sweep is idempotent: nothing is marked or purged twice.
	again, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.CheckpointsMarked != 0 || again.StorageObjectsPurged != 0 {
		t.Fatalf("sweep repeated work: %+v", again)
	}
}
