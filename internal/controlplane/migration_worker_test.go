package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func TestAllowedLocalAgentURLStaysInsideConfiguredSocketRoot(t *testing.T) {
	server := &Server{config: config.DefaultControlPlane(), logger: slog.Default()}
	server.config.LocalAgentSocketRoot = "/run/shift"
	for _, test := range []struct {
		name string
		url  string
		want bool
	}{
		{name: "agent socket", url: "unix:///run/shift/agent.sock", want: true},
		{name: "nested socket", url: "unix:///run/shift/tenant-a/agent.sock", want: true},
		{name: "other root", url: "unix:///run/other/agent.sock", want: false},
		{name: "traversal", url: "unix:///run/shift/../other.sock", want: false},
		{name: "tcp", url: "tcp://127.0.0.1:8443", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := server.allowedLocalAgentURL(test.url); got != test.want {
				t.Fatalf("allowedLocalAgentURL(%q) = %v, want %v", test.url, got, test.want)
			}
		})
	}
}

func TestMigrationWorkerCompletesAndCorrelatesAgentOperation(t *testing.T) {
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL worker integration test")
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

	stateRoot := t.TempDir()
	socketPath := filepath.Join(stateRoot, "agent.sock")
	fake := newWorkerFakeAgent()
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	serverHTTP := &http.Server{Handler: fake}
	go func() { _ = serverHTTP.Serve(listener) }()
	defer func() {
		_ = serverHTTP.Shutdown(context.Background())
		_ = listener.Close()
	}()

	userID, _ := model.NewID()
	orgID, _ := model.NewID()
	if _, _, err := store.Register(ctx, userID, "worker-test-"+orgID+"@example.test", "x", "Worker Test", orgID, "Worker Test", database.AuditInput{ID: "audit-" + orgID, Action: "test.register", ResourceType: "test", ResourceID: orgID}); err != nil {
		t.Fatal(err)
	}
	sourceID, _ := model.NewID()
	destinationID, _ := model.NewID()
	if _, err := store.CreateMachine(ctx, database.MachineRecord{ID: sourceID, OrganizationID: orgID, MachineID: "source-" + sourceID, Name: "source", AgentURL: "https://destination.example:8443", LocalAgentURL: "unix://" + socketPath, Capabilities: json.RawMessage(`{}`)}, database.AuditInput{ID: "audit-machine-" + sourceID, OrganizationID: orgID, Action: "test.machine", ResourceType: "machine", ResourceID: sourceID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMachine(ctx, database.MachineRecord{ID: destinationID, OrganizationID: orgID, MachineID: "destination-" + destinationID, Name: "destination", AgentURL: "https://destination.example:9443", Capabilities: json.RawMessage(`{}`)}, database.AuditInput{ID: "audit-machine-" + destinationID, OrganizationID: orgID, Action: "test.machine", ResourceType: "machine", ResourceID: destinationID}); err != nil {
		t.Fatal(err)
	}
	workloadID, _ := model.NewID()
	if _, err := store.CreateWorkload(ctx, database.WorkloadRecord{ID: workloadID, OrganizationID: orgID, MachineID: "source-" + sourceID, Name: "worker workload", Spec: json.RawMessage(`{"command":["/bin/true"]}`)}, database.AuditInput{ID: "audit-workload-" + workloadID, OrganizationID: orgID, Action: "test.workload", ResourceType: "workload", ResourceID: workloadID}); err != nil {
		t.Fatal(err)
	}
	jobID, _ := model.NewID()
	job, _, err := store.CreateMigration(ctx, database.MigrationRecord{ID: jobID, OrganizationID: orgID, WorkloadID: workloadID, SourceMachineID: "source-" + sourceID, DestinationMachineID: "destination-" + destinationID, Mode: "cold", CreatedBy: userID, IdempotencyKey: "worker-test-" + jobID, Managed: true}, database.AuditInput{ID: "audit-migration-" + jobID, OrganizationID: orgID, Action: "test.migration", ResourceType: "migration", ResourceID: jobID})
	if err != nil {
		t.Fatal(err)
	}
	if !job.Managed || job.Status != "queued" {
		t.Fatalf("unexpected queued job: %#v", job)
	}

	control := New(config.DefaultControlPlane(), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	control.config.LocalAgentSocketRoot = stateRoot
	for attempt := 0; attempt < 20; attempt++ {
		_ = control.processOneMigration(ctx, "worker-test")
		stored, readErr := store.Migration(ctx, orgID, jobID)
		if readErr == nil && stored.Status == "completed" {
			break
		}
	}
	stored, err := store.Migration(ctx, orgID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "completed" || stored.AgentMigrationID != fake.operationID || stored.AttemptCount != 1 {
		t.Fatalf("worker did not persist completed operation: %#v", stored)
	}
	if fake.createCount() != 1 {
		t.Fatalf("worker dispatched %d agent operations, want exactly one", fake.createCount())
	}
}

type workerFakeAgent struct {
	mu          sync.Mutex
	operationID string
	creates     int
}

func newWorkerFakeAgent() *workerFakeAgent {
	return &workerFakeAgent{operationID: "agent-migration-worker-test"}
}

func (fake *workerFakeAgent) createCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.creates
}

func (fake *workerFakeAgent) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/migrations":
		var input map[string]any
		_ = json.NewDecoder(request.Body).Decode(&input)
		fake.creates++
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": fake.operationID, "workload_id": input["workload_id"], "stage": "COMPLETED", "source_machine_id": "source", "mode": "cold", "source_preserved": false, "events": []any{}, "metrics": map[string]any{}})
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/migrations/"):
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": fake.operationID, "stage": "COMPLETED", "source_preserved": false, "events": []any{}, "metrics": map[string]any{}})
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/cancel"):
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "cancelled"})
	default:
		http.NotFound(writer, request)
	}
}
