package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// unmanagedFakeAgent reports one migration for the source agent: created out
// of band (agent-command dispatch or relay route), observable by id and by
// the job's idempotency key.
type unmanagedFakeAgent struct {
	mu         sync.Mutex
	migration  model.Migration
	listCalls  int
	pollCalls  int
	neverStart bool
}

func (fake *unmanagedFakeAgent) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/v1/migrations":
		fake.listCalls++
		if fake.neverStart {
			_ = json.NewEncoder(writer).Encode([]model.Migration{})
			return
		}
		_ = json.NewEncoder(writer).Encode([]model.Migration{fake.migration})
	case request.Method == http.MethodGet && request.URL.Path == "/v1/migrations/"+fake.migration.ID:
		fake.pollCalls++
		_ = json.NewEncoder(writer).Encode(fake.migration)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/migrations/"+fake.migration.ID+"/cancel":
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "cancelled"})
	default:
		http.NotFound(writer, request)
	}
}

func TestUnmanagedMigrationJobsSettle(t *testing.T) {
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	agentMigrationID := "agent-migration-unmanaged-" + time.Now().UTC().Format("150405.000000000")
	jobID, _ := model.NewID()
	fake := &unmanagedFakeAgent{migration: model.Migration{
		ID: agentMigrationID,
		// The relay/dispatch flow creates the agent migration under the job's
		// id as the idempotency key: that is how the settler adopts it when
		// the dispatch response was lost.
		IdempotencyKey:  jobID,
		WorkloadID:      "workload-1",
		SourceMachineID: "source",
		Stage:           model.MigrationCompleted,
		Events: []model.MigrationEvent{
			{Sequence: 1, Stage: model.MigrationTransfer, Message: "state transferred", Progress: 0.9},
			{Sequence: 2, Stage: model.MigrationCompleted, Message: "migration complete", Progress: 1},
		},
		Metrics: model.MigrationMetrics{TotalStateBytes: 4096, TransferredBytes: 2048, DeduplicatedBytes: 256, Downtime: 2 * time.Second, TransferBytesPerSec: 512},
	}}
	agentServer := httptest.NewServer(fake)
	defer agentServer.Close()

	userID, _ := model.NewID()
	orgID, _ := model.NewID()
	if _, _, err := store.Register(ctx, userID, "unmanaged-settle-"+orgID+"@example.test", "x", "Settle Test", orgID, "Settle Test", database.AuditInput{ID: "audit-" + orgID, Action: "test.register", ResourceType: "test", ResourceID: orgID}); err != nil {
		t.Fatal(err)
	}
	sourceID, _ := model.NewID()
	destinationID, _ := model.NewID()
	if _, err := store.CreateMachine(ctx, database.MachineRecord{ID: sourceID, OrganizationID: orgID, MachineID: "source-" + sourceID, Name: "source", AgentURL: agentServer.URL, Capabilities: json.RawMessage(`{}`)}, database.AuditInput{ID: "audit-machine-" + sourceID, OrganizationID: orgID, Action: "test.machine", ResourceType: "machine", ResourceID: sourceID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMachine(ctx, database.MachineRecord{ID: destinationID, OrganizationID: orgID, MachineID: "destination-" + destinationID, Name: "destination", AgentURL: "https://destination.example:8443", Capabilities: json.RawMessage(`{}`)}, database.AuditInput{ID: "audit-machine-" + destinationID, OrganizationID: orgID, Action: "test.machine", ResourceType: "machine", ResourceID: destinationID}); err != nil {
		t.Fatal(err)
	}
	workloadID, _ := model.NewID()
	if _, err := store.CreateWorkload(ctx, database.WorkloadRecord{ID: workloadID, OrganizationID: orgID, MachineID: "source-" + sourceID, Name: "worker workload", Spec: json.RawMessage(`{"command":["/bin/true"]}`)}, database.AuditInput{ID: "audit-workload-" + workloadID, OrganizationID: orgID, Action: "test.workload", ResourceType: "workload", ResourceID: workloadID}); err != nil {
		t.Fatal(err)
	}
	job, err := store.CreateMigrationForAgent(ctx, database.MigrationRecord{
		ID: jobID, OrganizationID: orgID, WorkloadID: workloadID,
		SourceMachineID: "source-" + sourceID, DestinationMachineID: "destination-" + destinationID,
		Mode: "cold", CreatedBy: userID, IdempotencyKey: jobID, TimeoutSeconds: 30,
	}, database.AuditInput{ID: "audit-migration-" + jobID, OrganizationID: orgID, Action: "test.migration", ResourceType: "migration", ResourceID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	if job.Managed {
		t.Fatal("the job under test must be unmanaged")
	}

	control := New(config.DefaultControlPlane(), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Registration is what the dispatch/relay paths do at creation: without
	// the worker (which only claims managed jobs) nothing settles this row.
	control.watchUnmanagedMigration(orgID, jobID)
	control.settleUnmanagedMigrations(ctx)

	stored, err := store.Migration(ctx, orgID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "completed" {
		t.Fatalf("unmanaged job status = %q, want completed (it was frozen at %q before settlement)", stored.Status, "queued")
	}
	var progress map[string]any
	if err := json.Unmarshal(stored.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"current_stage", "percent", "downtime_ms", "transfer_speed", "deduplicated_bytes", "transferred_bytes", "total_state_bytes", "agent_id"} {
		if _, ok := progress[key]; !ok {
			t.Fatalf("settled progress is missing contract key %q: %v", key, progress)
		}
	}
	if progress["agent_id"] != agentMigrationID {
		t.Fatalf("progress agent_id = %v, want %q (the cancel path keys off it)", progress["agent_id"], agentMigrationID)
	}
	if progress["downtime_ms"] != float64(2000) {
		t.Fatalf("progress downtime_ms = %v, want 2000", progress["downtime_ms"])
	}
	events, err := store.MigrationEvents(ctx, orgID, jobID, database.Page{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("migration_events rows = %d, want the agent's 2 events (the stream was empty before)", len(events))
	}
	if events[0].Stage != string(model.MigrationTransfer) || events[1].Stage != string(model.MigrationCompleted) {
		t.Fatalf("migration_events stages = %q/%q", events[0].Stage, events[1].Stage)
	}
}

func TestUnmanagedMigrationAdoptsAgentOperationByIdempotencyKey(t *testing.T) {
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	agentMigrationID := "agent-migration-adopted-" + time.Now().UTC().Format("150405.000000000")
	fake := &unmanagedFakeAgent{migration: model.Migration{
		ID: agentMigrationID, Stage: model.MigrationCompleted,
		Metrics: model.MigrationMetrics{TotalStateBytes: 1, TransferredBytes: 1},
	}}
	agentServer := httptest.NewServer(fake)
	defer agentServer.Close()

	userID, _ := model.NewID()
	orgID, _ := model.NewID()
	if _, _, err := store.Register(ctx, userID, "unmanaged-adopt-"+orgID+"@example.test", "x", "Adopt Test", orgID, "Adopt Test", database.AuditInput{ID: "audit-" + orgID, Action: "test.register", ResourceType: "test", ResourceID: orgID}); err != nil {
		t.Fatal(err)
	}
	sourceID, _ := model.NewID()
	destinationID, _ := model.NewID()
	if _, err := store.CreateMachine(ctx, database.MachineRecord{ID: sourceID, OrganizationID: orgID, MachineID: "source-" + sourceID, Name: "source", AgentURL: agentServer.URL, Capabilities: json.RawMessage(`{}`)}, database.AuditInput{ID: "audit-machine-" + sourceID, OrganizationID: orgID, Action: "test.machine", ResourceType: "machine", ResourceID: sourceID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMachine(ctx, database.MachineRecord{ID: destinationID, OrganizationID: orgID, MachineID: "destination-" + destinationID, Name: "destination", AgentURL: "https://destination.example:8443", Capabilities: json.RawMessage(`{}`)}, database.AuditInput{ID: "audit-machine-" + destinationID, OrganizationID: orgID, Action: "test.machine", ResourceType: "machine", ResourceID: destinationID}); err != nil {
		t.Fatal(err)
	}
	workloadID, _ := model.NewID()
	if _, err := store.CreateWorkload(ctx, database.WorkloadRecord{ID: workloadID, OrganizationID: orgID, MachineID: "source-" + sourceID, Name: "worker workload", Spec: json.RawMessage(`{"command":["/bin/true"]}`)}, database.AuditInput{ID: "audit-workload-" + workloadID, OrganizationID: orgID, Action: "test.workload", ResourceType: "workload", ResourceID: workloadID}); err != nil {
		t.Fatal(err)
	}
	// The job id doubles as the agent's idempotency key; the agent-side id
	// was never recorded (dispatch response lost). The settler must adopt the
	// operation from the agent's list and settle the job.
	jobID := "adopted-job-" + agentMigrationID
	if _, err := store.CreateMigrationForAgent(ctx, database.MigrationRecord{
		ID: jobID, OrganizationID: orgID, WorkloadID: workloadID,
		SourceMachineID: "source-" + sourceID, DestinationMachineID: "destination-" + destinationID,
		Mode: "cold", CreatedBy: userID, IdempotencyKey: jobID, TimeoutSeconds: 30,
	}, database.AuditInput{ID: "audit-migration-" + jobID, OrganizationID: orgID, Action: "test.migration", ResourceType: "migration", ResourceID: jobID}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.migration.IdempotencyKey = jobID
	fake.mu.Unlock()

	control := New(config.DefaultControlPlane(), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	control.watchUnmanagedMigration(orgID, jobID)
	control.settleUnmanagedMigrations(ctx)

	stored, err := store.Migration(ctx, orgID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "completed" {
		t.Fatalf("adopted job status = %q, want completed", stored.Status)
	}
	var progress map[string]any
	if err := json.Unmarshal(stored.Progress, &progress); err != nil {
		t.Fatal(err)
	}
	if progress["agent_id"] != agentMigrationID {
		t.Fatalf("progress agent_id = %v, want %q", progress["agent_id"], agentMigrationID)
	}
}
