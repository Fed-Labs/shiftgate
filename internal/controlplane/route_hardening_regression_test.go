package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
)

func newRouteRegressionServer(t *testing.T) (*httptest.Server, *http.Client, *database.Store, context.Context) {
	t.Helper()
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	configuration := config.DefaultControlPlane()
	configuration.DatabaseURL = databaseURL
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	server := New(configuration, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, httpServer.Client(), store, ctx
}

type routeOrg struct {
	id     string
	auth   string
	path   string
	client *http.Client
}

func registerRouteOrg(t *testing.T, httpServer *httptest.Server, client *http.Client, label string) routeOrg {
	t.Helper()
	var registration struct {
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/register", RegisterRequest{
		Email:    label + "-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test",
		Password: "correct horse battery staple", DisplayName: label, Organization: label + " Organization",
	}, http.StatusCreated, &registration, "")
	return routeOrg{id: registration.Organization.ID, auth: "Bearer " + registration.Tokens.AccessToken, path: httpServer.URL + "/v1/organizations/" + registration.Organization.ID, client: client}
}

func (org routeOrg) createMachine(t *testing.T, machineID, agentURL string) Machine {
	t.Helper()
	var machine Machine
	postJSON(t, org.client, org.path+"/machines", CreateMachineRequest{MachineID: machineID, Name: machineID, AgentURL: agentURL}, http.StatusCreated, &machine, org.auth)
	return machine
}

func (org routeOrg) createWorkload(t *testing.T, machineID, name string) Workload {
	t.Helper()
	var workload Workload
	postJSON(t, org.client, org.path+"/workloads", CreateWorkloadRequest{MachineID: machineID, Name: name, Spec: map[string]any{"command": []any{"/bin/true"}}}, http.StatusCreated, &workload, org.auth)
	return workload
}

func routeExpectError(t *testing.T, client *http.Client, endpoint, authorization string, input any, wantStatus int, wantCode string) {
	t.Helper()
	var problem ErrorResponse
	postJSON(t, client, endpoint, input, wantStatus, &problem, authorization)
	if problem.Code != wantCode {
		t.Fatalf("POST %s answered %d with code %q, want %q (%s)", endpoint, wantStatus, problem.Code, wantCode, problem.Message)
	}
}

func TestMachineAgentURLValidationBlocksSSRF(t *testing.T) {
	rejected := []string{
		"",
		"http://169.254.169.254/",
		"http://127.0.0.1:8080",
		"unix:///var/run/docker.sock",
		"tcp://10.0.0.5:5432",
		"https://169.254.169.254/latest/meta-data/",
		"https://127.0.0.1:8443",
		"https://10.0.0.5:8443",
		"https://172.16.0.9:8443",
		"https://192.168.1.5:8443",
		"https://100.100.100.200",
		"https://[::1]:8443",
		"https://[fe80::1]:8443",
		"https://[fd00:ec2::254]",
		"https://[::ffff:127.0.0.1]:8443",
		"https://metadata.google.internal",
		"https://metadata",
		"https://user:pass@agent.example:8443",
		"https://agent.example:8443/path",
		"https://agent.example:8443?x=1",
		"https://agent.example:0",
		"https://agent.example:99999",
		"https://agent.example:443x",
	}
	for _, raw := range rejected {
		if normalized, err := normalizeAgentURL(raw); err == nil {
			t.Fatalf("normalizeAgentURL(%q) = %q, want rejection", raw, normalized)
		}
	}
	accepted := map[string]string{
		"https://agent.example:8443":          "https://agent.example:8443",
		" https://agent.example:8443/ ":       "https://agent.example:8443",
		"https://agent.example":               "https://agent.example",
		"https://8.8.8.8:8443":                "https://8.8.8.8:8443",
		"https://[2001:4860:4860::8888]:8443": "https://[2001:4860:4860::8888]:8443",
	}
	for raw, want := range accepted {
		normalized, err := normalizeAgentURL(raw)
		if err != nil {
			t.Fatalf("normalizeAgentURL(%q) failed: %v", raw, err)
		}
		if normalized != want {
			t.Fatalf("normalizeAgentURL(%q) = %q, want %q", raw, normalized, want)
		}
	}
	t.Setenv(agentHostAllowlistEnv, "127.0.0.1,10.0.0.0/8,agent.internal")
	for _, raw := range []string{"https://127.0.0.1:8443", "https://10.1.2.3:8443", "https://agent.internal:8443"} {
		if _, err := normalizeAgentURL(raw); err != nil {
			t.Fatalf("explicitly allowlisted %q was rejected: %v", raw, err)
		}
	}
	if _, err := normalizeAgentURL("https://169.254.169.254"); err == nil {
		t.Fatal("allowlist must not admit link-local metadata addresses")
	}
}

func TestWorkloadStatusDocumentKeysUnified(t *testing.T) {
	unified := unifyWorkloadStatusKeys(map[string]any{"status": "registered"})
	if unified["state"] != "registered" || unified["status"] != "registered" {
		t.Fatalf("single-key status was not unified: %#v", unified)
	}
	unified = unifyWorkloadStatusKeys(map[string]any{"state": "deleted", "agent": map[string]any{"id": "x"}})
	if unified["status"] != "deleted" || unified["state"] != "deleted" {
		t.Fatalf("single-key state was not unified: %#v", unified)
	}
	untouched := unifyWorkloadStatusKeys(map[string]any{"state": "running", "status": "healthy"})
	if untouched["status"] != "healthy" {
		t.Fatalf("unification must not clobber a distinct existing key: %#v", untouched)
	}
	if !workloadStatusDeleted(json.RawMessage(`{"state":"deleted"}`)) ||
		!workloadStatusDeleted(json.RawMessage(`{"status":"Deleted"}`)) {
		t.Fatal("deleted workload status was not detected")
	}
	if workloadStatusDeleted(json.RawMessage(`{"state":"running"}`)) {
		t.Fatal("live workload was flagged deleted")
	}
}

func TestMigrationTimeoutValidation(t *testing.T) {
	for _, seconds := range []int{0, 30, 300, 86400} {
		if err := validateMigrationTimeout(seconds); err != nil {
			t.Fatalf("validateMigrationTimeout(%d) = %v, want nil", seconds, err)
		}
	}
	for _, seconds := range []int{-1, -3600, 1, 29, 86401, 1 << 40} {
		if err := validateMigrationTimeout(seconds); err == nil {
			t.Fatalf("validateMigrationTimeout(%d) = nil, want rejection", seconds)
		}
	}
}

func TestMigrationJobResponseDerivesRoute(t *testing.T) {
	managed := migrationJobResponse(database.MigrationRecord{Managed: true}, "https://relay.example")
	if managed.Route != "direct" || managed.RelayEndpoint != "" {
		t.Fatalf("managed job response: %#v", managed)
	}
	relayed := migrationJobResponse(database.MigrationRecord{Managed: false}, " https://relay.example ")
	if relayed.Route != "relay" || relayed.RelayEndpoint != "https://relay.example" {
		t.Fatalf("relay job response: %#v", relayed)
	}
	if relayed.RelayTicket != "" {
		t.Fatalf("one-use relay ticket must never be re-exposed on read: %#v", relayed)
	}
}

func TestCheckpointIDValidationAndScoping(t *testing.T) {
	for _, id := range []string{"abc", "0f7a3c1e-9d21-4a6b-8c3d-1e2f3a4b5c6d", "base_v2.final", strings.Repeat("a", 128)} {
		if !validCheckpointID(id) {
			t.Fatalf("validCheckpointID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"", strings.Repeat("a", 129), "a/b", "a\\b", "..", "a..b", "a:b", "a b", "héllo", "<script>", strings.Repeat("a", 2000)} {
		if validCheckpointID(id) {
			t.Fatalf("validCheckpointID(%q) = true, want false", id)
		}
	}
	const organizationID = "org-1234"
	stored := checkpointStorageID(organizationID, "base")
	if stored != "org-1234:base" {
		t.Fatalf("checkpointStorageID = %q", stored)
	}
	if got := checkpointClientID(organizationID, stored); got != "base" {
		t.Fatalf("checkpointClientID round trip = %q", got)
	}
	if got := checkpointClientID(organizationID, "legacy-raw-id"); got != "legacy-raw-id" {
		t.Fatalf("unscoped stored id must pass through unchanged, got %q", got)
	}
	if got := checkpointClientID("org-other", stored); got != stored {
		t.Fatalf("foreign org must not be able to unscope another tenant's id, got %q", got)
	}
}

func TestWorkloadListPageOffsetIsCapped(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/organizations/org-1/workloads?limit=500&offset=100000000", nil)
	page := pageFromCappedRequest(request)
	if page.Offset != maxPageOffset || page.Limit != 500 {
		t.Fatalf("pageFromCappedRequest = %+v, want offset %d", page, maxPageOffset)
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/organizations/org-1/workloads?offset=-5", nil)
	if page := pageFromCappedRequest(request); page.Offset != 0 {
		t.Fatalf("negative offset must floor to 0, got %+v", page)
	}
}

func TestMachineHeartbeatRejectsSSRFAndStoresNormalizedURL(t *testing.T) {
	httpServer, client, _, _ := newRouteRegressionServer(t)
	org := registerRouteOrg(t, httpServer, client, "heartbeat-ssrf")
	machine := org.createMachine(t, "hb-machine", " https://agent.example:8443/ ")
	if machine.AgentURL != "https://agent.example:8443" {
		t.Fatalf("create stored %q, want the normalized form", machine.AgentURL)
	}
	routeExpectError(t, client, org.path+"/machines", org.auth, CreateMachineRequest{MachineID: "hb-evil", Name: "Evil", AgentURL: "http://169.254.169.254/"}, http.StatusBadRequest, "MACHINE_INVALID")

	heartbeatURL := org.path + "/machines/hb-machine/heartbeat"
	attackerURLs := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://127.0.0.1:2375",
		"unix:///var/run/docker.sock",
		"tcp://10.0.0.5:5432",
		"https://169.254.169.254/",
		"https://metadata.google.internal/computeMetadata/v1/",
		"https://127.0.0.1:8443",
		"https://10.0.0.5:8443",
		"https://[::1]:8443",
		"https://[fd00:ec2::254]",
		"https://user:pass@attacker.example:8443",
		"https://attacker.example:8443/redirect",
	}
	for _, attackerURL := range attackerURLs {
		routeExpectError(t, client, heartbeatURL, org.auth, map[string]any{"agent_url": attackerURL, "status": "online"}, http.StatusBadRequest, "MACHINE_INVALID")
	}
	routeExpectError(t, client, heartbeatURL, org.auth, map[string]any{"name": strings.Repeat("n", 250), "status": "online"}, http.StatusBadRequest, "MACHINE_INVALID")

	var updated Machine
	postJSON(t, client, heartbeatURL, map[string]any{"agent_url": " https://agent2.example:8443/ ", "status": "online"}, http.StatusOK, &updated, org.auth)
	if updated.AgentURL != "https://agent2.example:8443" {
		t.Fatalf("heartbeat stored %q, want the normalized replacement", updated.AgentURL)
	}
	var machines []Machine
	getJSON(t, client, org.path+"/machines", http.StatusOK, &machines, org.auth)
	for _, record := range machines {
		if strings.Contains(record.AgentURL, "169.254") || strings.Contains(record.AgentURL, "docker.sock") {
			t.Fatalf("attacker-controlled agent_url persisted: %#v", record)
		}
	}
}

func TestMigrationCompletionSettlesWorkloadAssignment(t *testing.T) {
	httpServer, client, store, ctx := newRouteRegressionServer(t)
	org := registerRouteOrg(t, httpServer, client, "settle")
	org.createMachine(t, "settle-source", "https://source.example:8443")
	org.createMachine(t, "settle-destination", "https://destination.example:8443")
	workload := org.createWorkload(t, "settle-source", "settle-workload")

	var first MigrationJob
	postJSON(t, client, org.path+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "settle-source", DestinationMachineID: "settle-destination", Mode: "cold"}, http.StatusAccepted, &first, org.auth)
	if _, err := store.UpdateMigrationStatus(ctx, org.id, first.ID, "completed", json.RawMessage(`{"stage":"COMPLETED","progress":1}`), "", database.AuditInput{ID: "audit-settle-" + first.ID, OrganizationID: org.id, Action: "migration.complete", ResourceType: "migration", ResourceID: first.ID}); err != nil {
		t.Fatal(err)
	}
	var jobs []MigrationJob
	getJSON(t, client, org.path+"/migrations", http.StatusOK, &jobs, org.auth)
	if len(jobs) != 1 || jobs[0].Route != "direct" {
		t.Fatalf("unexpected migration list: %#v", jobs)
	}
	stored, err := store.Workload(ctx, org.id, workload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MachineID != "settle-destination" {
		t.Fatalf("workload assignment is %q after a completed migration, want settle-destination", stored.MachineID)
	}

	var second MigrationJob
	postJSON(t, client, org.path+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "settle-destination", DestinationMachineID: "settle-source", Mode: "cold"}, http.StatusAccepted, &second, org.auth)
	if second.ID == first.ID {
		t.Fatalf("second move replayed the first job: %#v", second)
	}
	routeExpectError(t, client, org.path+"/migrations", org.auth, CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "settle-source", DestinationMachineID: "settle-destination", Mode: "cold"}, http.StatusBadRequest, "MIGRATION_SOURCE_MISMATCH")
}

func TestCheckpointCreateSelfHealsAfterMigration(t *testing.T) {
	httpServer, client, store, ctx := newRouteRegressionServer(t)
	org := registerRouteOrg(t, httpServer, client, "ckpt-settle")
	org.createMachine(t, "ckpt-source", "https://source.example:8443")
	org.createMachine(t, "ckpt-destination", "https://destination.example:8443")
	workload := org.createWorkload(t, "ckpt-source", "ckpt-workload")

	var job MigrationJob
	postJSON(t, client, org.path+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "ckpt-source", DestinationMachineID: "ckpt-destination", Mode: "cold"}, http.StatusAccepted, &job, org.auth)
	if _, err := store.UpdateMigrationStatus(ctx, org.id, job.ID, "completed", json.RawMessage(`{"stage":"COMPLETED","progress":1}`), "", database.AuditInput{ID: "audit-settle-" + job.ID, OrganizationID: org.id, Action: "migration.complete", ResourceType: "migration", ResourceID: job.ID}); err != nil {
		t.Fatal(err)
	}

	var checkpoint Checkpoint
	postJSON(t, client, org.path+"/checkpoints", CreateCheckpointRequest{ID: "post-move-checkpoint", WorkloadID: workload.ID, MachineID: "ckpt-destination", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusCreated, &checkpoint, org.auth)
	if checkpoint.ID != "post-move-checkpoint" {
		t.Fatalf("unexpected checkpoint: %#v", checkpoint)
	}
	stored, err := store.Workload(ctx, org.id, workload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MachineID != "ckpt-destination" {
		t.Fatalf("post-move checkpoint did not settle the assignment: %#v", stored)
	}
}

func TestCheckpointRegistrationIsScopedPerTenant(t *testing.T) {
	httpServer, client, _, _ := newRouteRegressionServer(t)
	first := registerRouteOrg(t, httpServer, client, "tenant-a")
	second := registerRouteOrg(t, httpServer, client, "tenant-b")
	first.createMachine(t, "tenant-a-machine", "https://a.example:8443")
	second.createMachine(t, "tenant-b-machine", "https://b.example:8443")
	firstWorkload := first.createWorkload(t, "tenant-a-machine", "tenant-a-workload")
	secondWorkload := second.createWorkload(t, "tenant-b-machine", "tenant-b-workload")

	shared := "shared-checkpoint-001"
	var firstCheckpoint Checkpoint
	postJSON(t, client, first.path+"/checkpoints", CreateCheckpointRequest{ID: shared, WorkloadID: firstWorkload.ID, MachineID: "tenant-a-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusCreated, &firstCheckpoint, first.auth)
	if firstCheckpoint.ID != shared {
		t.Fatalf("checkpoint id must round-trip unscoped: %#v", firstCheckpoint)
	}
	var secondCheckpoint Checkpoint
	postJSON(t, client, second.path+"/checkpoints", CreateCheckpointRequest{ID: shared, WorkloadID: secondWorkload.ID, MachineID: "tenant-b-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusCreated, &secondCheckpoint, second.auth)
	if secondCheckpoint.ID != shared {
		t.Fatalf("second tenant must be able to register the same id: %#v", secondCheckpoint)
	}

	routeExpectError(t, client, first.path+"/checkpoints", first.auth, CreateCheckpointRequest{ID: shared, WorkloadID: firstWorkload.ID, MachineID: "tenant-a-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusConflict, "CHECKPOINT_EXISTS")
	routeExpectError(t, client, first.path+"/checkpoints", first.auth, CreateCheckpointRequest{ID: "bad/id", WorkloadID: firstWorkload.ID, MachineID: "tenant-a-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusBadRequest, "CHECKPOINT_INVALID")
	routeExpectError(t, client, first.path+"/checkpoints", first.auth, CreateCheckpointRequest{ID: strings.Repeat("a", 300), WorkloadID: firstWorkload.ID, MachineID: "tenant-a-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusBadRequest, "CHECKPOINT_INVALID")

	var listed []Checkpoint
	getJSON(t, client, first.path+"/checkpoints", http.StatusOK, &listed, first.auth)
	if len(listed) != 1 || listed[0].ID != shared {
		t.Fatalf("first tenant list leaked other tenants: %#v", listed)
	}
	getJSON(t, client, second.path+"/checkpoints", http.StatusOK, &listed, second.auth)
	if len(listed) != 1 || listed[0].ID != shared {
		t.Fatalf("second tenant list leaked other tenants: %#v", listed)
	}

	var base, incremental Checkpoint
	postJSON(t, client, first.path+"/checkpoints", CreateCheckpointRequest{ID: "chain-base", WorkloadID: firstWorkload.ID, MachineID: "tenant-a-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusCreated, &base, first.auth)
	postJSON(t, client, first.path+"/checkpoints", CreateCheckpointRequest{ID: "chain-next", WorkloadID: firstWorkload.ID, MachineID: "tenant-a-machine", Kind: "incremental", ParentID: "chain-base", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusCreated, &incremental, first.auth)
	if incremental.ParentID != "chain-base" {
		t.Fatalf("parent id must round-trip unscoped: %#v", incremental)
	}
}

func TestOwnershipAcquireAnswersNotFoundForMissingAndForeignWorkloads(t *testing.T) {
	httpServer, client, _, _ := newRouteRegressionServer(t)
	owner := registerRouteOrg(t, httpServer, client, "owner")
	stranger := registerRouteOrg(t, httpServer, client, "stranger")
	owner.createMachine(t, "owner-machine", "https://owner.example:8443")
	stranger.createMachine(t, "stranger-machine", "https://stranger.example:8443")
	workload := owner.createWorkload(t, "owner-machine", "owner-workload")

	routeExpectError(t, client, owner.path+"/ownership/acquire", owner.auth, AcquireOwnershipRequest{WorkloadID: "missing-workload", MachineID: "owner-machine", LeaseSeconds: 60}, http.StatusNotFound, "WORKLOAD_NOT_FOUND")
	routeExpectError(t, client, stranger.path+"/ownership/acquire", stranger.auth, AcquireOwnershipRequest{WorkloadID: workload.ID, MachineID: "stranger-machine", LeaseSeconds: 60}, http.StatusNotFound, "WORKLOAD_NOT_FOUND")
	routeExpectError(t, client, stranger.path+"/ownership/fence", stranger.auth, FenceOwnershipRequest{WorkloadID: workload.ID, MachineID: "stranger-machine", Epoch: 1, Token: "shift_own_x"}, http.StatusNotFound, "WORKLOAD_NOT_FOUND")

	var ownership Ownership
	postJSON(t, client, owner.path+"/ownership/acquire", AcquireOwnershipRequest{WorkloadID: workload.ID, MachineID: "owner-machine", LeaseSeconds: 60}, http.StatusOK, &ownership, owner.auth)
	if ownership.Token == "" || ownership.OwnerMachineID != "owner-machine" {
		t.Fatalf("unexpected ownership: %#v", ownership)
	}
}

func TestMigrationRejectsInvalidTimeoutAndDeletedWorkload(t *testing.T) {
	httpServer, client, _, _ := newRouteRegressionServer(t)
	org := registerRouteOrg(t, httpServer, client, "mig-guard")
	org.createMachine(t, "guard-source", "https://source.example:8443")
	org.createMachine(t, "guard-destination", "https://destination.example:8443")
	workload := org.createWorkload(t, "guard-source", "guard-workload")

	for _, seconds := range []int{-5, 5, 90000} {
		routeExpectError(t, client, org.path+"/migrations", org.auth, CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "guard-source", DestinationMachineID: "guard-destination", Mode: "cold", TimeoutSeconds: seconds}, http.StatusBadRequest, "MIGRATION_TIMEOUT_INVALID")
	}

	var updated Workload
	postJSON(t, client, org.path+"/workloads/"+workload.ID+"/status", map[string]any{"state": "deleted"}, http.StatusOK, &updated, org.auth)
	if updated.Status["state"] != "deleted" || updated.Status["status"] != "deleted" {
		t.Fatalf("status document was not unified on write: %#v", updated.Status)
	}
	routeExpectError(t, client, org.path+"/migrations", org.auth, CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "guard-source", DestinationMachineID: "guard-destination", Mode: "cold"}, http.StatusConflict, "WORKLOAD_DELETED")
}

func TestWorkloadStatusChangesBlockedDuringMigration(t *testing.T) {
	httpServer, client, store, ctx := newRouteRegressionServer(t)
	org := registerRouteOrg(t, httpServer, client, "status-guard")
	org.createMachine(t, "status-source", "https://source.example:8443")
	org.createMachine(t, "status-destination", "https://destination.example:8443")
	workload := org.createWorkload(t, "status-source", "status-workload")

	var job MigrationJob
	postJSON(t, client, org.path+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: "status-source", DestinationMachineID: "status-destination", Mode: "cold"}, http.StatusAccepted, &job, org.auth)
	routeExpectError(t, client, org.path+"/workloads/"+workload.ID+"/status", org.auth, map[string]any{"state": "deleted"}, http.StatusConflict, "MIGRATION_ACTIVE")
	routeExpectError(t, client, org.path+"/workloads/"+workload.ID+"/status", org.auth, map[string]any{"state": "stopped"}, http.StatusConflict, "MIGRATION_ACTIVE")

	if _, err := store.UpdateMigrationStatus(ctx, org.id, job.ID, "cancelled", json.RawMessage(`{}`), "", database.AuditInput{ID: "audit-cancel-" + job.ID, OrganizationID: org.id, Action: "migration.cancel", ResourceType: "migration", ResourceID: job.ID}); err != nil {
		t.Fatal(err)
	}
	var updated Workload
	postJSON(t, client, org.path+"/workloads/"+workload.ID+"/status", map[string]any{"state": "stopped"}, http.StatusOK, &updated, org.auth)
	if updated.Status["state"] != "stopped" {
		t.Fatalf("status change after migration settle was refused or rewritten: %#v", updated.Status)
	}
}

func TestCheckpointListHidesDeletedCheckpoints(t *testing.T) {
	httpServer, client, store, ctx := newRouteRegressionServer(t)
	org := registerRouteOrg(t, httpServer, client, "ckpt-softdel")
	org.createMachine(t, "softdel-machine", "https://softdel.example:8443")
	workload := org.createWorkload(t, "softdel-machine", "softdel-workload")

	var live Checkpoint
	postJSON(t, client, org.path+"/checkpoints", CreateCheckpointRequest{ID: "live-checkpoint", WorkloadID: workload.ID, MachineID: "softdel-machine", Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1}, http.StatusCreated, &live, org.auth)
	if _, err := store.CreateCheckpoint(ctx, database.CheckpointRecord{ID: checkpointStorageID(org.id, "gone-checkpoint"), OrganizationID: org.id, WorkloadID: workload.ID, MachineID: "softdel-machine", Kind: "full", Manifest: json.RawMessage(`{}`), PlainBytes: 1, StoredBytes: 1, ChunkCount: 1, Status: "deleted"}, database.AuditInput{ID: "audit-gone-" + org.id, OrganizationID: org.id, Action: "test.checkpoint", ResourceType: "checkpoint", ResourceID: "gone-checkpoint"}); err != nil {
		t.Fatal(err)
	}

	var listed []Checkpoint
	getJSON(t, client, org.path+"/checkpoints", http.StatusOK, &listed, org.auth)
	if len(listed) != 1 || listed[0].ID != "live-checkpoint" {
		t.Fatalf("default listing must hide soft-deleted checkpoints: %#v", listed)
	}
	getJSON(t, client, org.path+"/checkpoints?include_deleted=true", http.StatusOK, &listed, org.auth)
	if len(listed) != 2 {
		t.Fatalf("include_deleted=true must show soft-deleted checkpoints: %#v", listed)
	}
}
