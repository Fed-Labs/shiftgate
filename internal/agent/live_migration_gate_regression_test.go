package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// Regression cover for the P2P live-migration paywall (ship finding F-01):
// live mode is a paid entitlement and the agent's local API is an admission
// point the product controls, so a live-mode create must be refused with the
// shared billing error contract when the agent holds no live-capable
// entitlement. Cold mode is never gated. The agent-side enforcement lives in
// liveMigrationAllowed — see the managed/learned tests below for how a
// managed agent's truth arrives from the heartbeat rather than a local file.
func TestLocalAPILiveMigrationRequiresEntitlement(t *testing.T) {
	service, workload := localAPIFixture(t)

	destination := model.Destination{MachineID: "detached-destination", AgentURL: "https://127.0.0.1:18443"}
	live := migrateRequest{WorkloadID: workload.Spec.ID, Destination: destination, Mode: model.MigrationLive, TimeoutSeconds: 60}
	response := localCall(t, service, http.MethodPost, "/v1/migrations", live, 0)
	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("unentitled live migration status %d, want 402: %s", response.Code, response.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "LIVE_MIGRATION_PLAN_REQUIRED" {
		t.Fatalf("unentitled live migration code %q, want LIVE_MIGRATION_PLAN_REQUIRED: %s", body.Code, response.Body.String())
	}

	// Cold mode is never gated: the same request with mode=cold must not be
	// answered with the paywall (any other outcome — admitted, or refused
	// for reachability — proves the gate is mode-specific).
	cold := migrateRequest{WorkloadID: workload.Spec.ID, Destination: destination, Mode: model.MigrationCold, TimeoutSeconds: 60}
	coldResponse := localCall(t, service, http.MethodPost, "/v1/migrations", cold, 0)
	if coldResponse.Code == http.StatusPaymentRequired {
		t.Fatalf("cold migration was paywalled: %s", coldResponse.Body.String())
	}

	// Preflight stays free in both modes: compatibility must always be
	// checkable before anything is frozen.
	preflight := migrateRequest{WorkloadID: workload.Spec.ID, Destination: destination, Mode: model.MigrationLive, TimeoutSeconds: 5}
	preflightResponse := localCall(t, service, http.MethodPost, "/v1/migrations/preflight", preflight, 0)
	if preflightResponse.Code == http.StatusPaymentRequired {
		t.Fatalf("live preflight was paywalled: %s", preflightResponse.Body.String())
	}
}

// The entitlement seam: a managed agent's cached heartbeat allowance is the
// licensing truth, it overrides the local config assertion, and once learned
// it stays authoritative (fail closed on cold-only, admit on live-capable).
func TestLiveMigrationGateFollowsLearnedEntitlement(t *testing.T) {
	service, workload := localAPIFixture(t)
	destination := model.Destination{MachineID: "detached-destination", AgentURL: "https://127.0.0.1:18443"}
	live := migrateRequest{WorkloadID: workload.Spec.ID, Destination: destination, Mode: model.MigrationLive, TimeoutSeconds: 60}

	// Learned cold-only: refused even when the config asserts live.
	service.config.Entitlement.LiveMigration = true
	service.noteEntitlement(false)
	response := localCall(t, service, http.MethodPost, "/v1/migrations", live, 0)
	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("cold-only entitlement admitted a live migration: %d %s", response.Code, response.Body.String())
	}

	// Learned live-capable: the paywall steps aside (any non-402 outcome —
	// admitted, or refused for reachability/state — proves the gate opened).
	service.noteEntitlement(true)
	response = localCall(t, service, http.MethodPost, "/v1/migrations", live, 0)
	if response.Code == http.StatusPaymentRequired {
		t.Fatalf("live-capable entitlement was paywalled: %s", response.Body.String())
	}
}

// A detached agent (no control-plane identity) is entitled only through the
// explicit configuration assertion, and that assertion is honored.
func TestDetachedAgentConfigAssertionEntitlesLive(t *testing.T) {
	service, workload := localAPIFixture(t)
	if service.liveMigrationAllowed() {
		t.Fatal("a detached agent without the config assertion must be cold-only")
	}
	service.config.Entitlement.LiveMigration = true
	if !service.liveMigrationAllowed() {
		t.Fatal("config assertion entitlement.live_migration must entitle a detached agent")
	}
	destination := model.Destination{MachineID: "detached-destination", AgentURL: "https://127.0.0.1:18443"}
	live := migrateRequest{WorkloadID: workload.Spec.ID, Destination: destination, Mode: model.MigrationLive, TimeoutSeconds: 60}
	response := localCall(t, service, http.MethodPost, "/v1/migrations", live, 0)
	if response.Code == http.StatusPaymentRequired {
		t.Fatalf("an entitled detached agent was paywalled: %s", response.Body.String())
	}
}

// entitlementPlane is a control plane whose heartbeat response carries the
// live-migration allowance, exactly as machines_routes does in production.
type entitlementPlane struct {
	mu     sync.Mutex
	allows *bool
	hearts int
	server *httptest.Server
}

func newEntitlementPlane(t *testing.T, allows bool) *entitlementPlane {
	t.Helper()
	plane := &entitlementPlane{allows: &allows}
	plane.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+standbyTestAPIKey {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/v1/organizations/"+standbyTestOrganization+"/machines/machine-standby/heartbeat" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		plane.mu.Lock()
		plane.hearts++
		allows := plane.allows
		plane.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if allows == nil {
			_, _ = writer.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"live_migration_allowed": *allows})
	}))
	t.Cleanup(plane.server.Close)
	return plane
}

func (p *entitlementPlane) heartbeats() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hearts
}

// awaitHeartbeat starts the reporter and waits for one real heartbeat round
// trip to the fake control plane — the production path the gate must learn
// from, not the seam the unit tests drive directly.
func awaitHeartbeat(t *testing.T, service *Service, plane *entitlementPlane) {
	t.Helper()
	service.reporter.Start(context.Background())
	t.Cleanup(service.reporter.Stop)
	deadline := time.Now().Add(5 * time.Second)
	for plane.heartbeats() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the reporter never heartbeated the control plane")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitGate waits for the gate to reach the wanted answer. Only opening is
// racy: the listener runs on the heartbeat goroutine after the response is
// read, and a closed gate is the fail-closed default as well as the learned
// cold-only answer.
func awaitGate(t *testing.T, service *Service, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for service.liveMigrationAllowed() != want {
		if time.Now().After(deadline) {
			t.Fatalf("the gate never reached %v", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestManagedAgentGateConsumesTheHeartbeatEntitlement: for an agent with a
// control-plane identity, the licensing truth is what the heartbeat reports
// — learned value open admits live mode even with the local assertion off.
func TestManagedAgentGateConsumesTheHeartbeatEntitlement(t *testing.T) {
	plane := newEntitlementPlane(t, true)
	service := openStandbyTestService(t, plane.server.URL)
	service.config.Entitlement.LiveMigration = false
	if service.liveMigrationAllowed() {
		t.Fatal("a managed agent must not admit live mode before the entitlement is learned")
	}
	awaitHeartbeat(t, service, plane)
	awaitGate(t, service, true)
}

// TestManagedAgentLearnedEntitlementOverridesTheLocalAssertion: the
// configuration assertion is a forgeable local file and is never the
// licensing truth for a managed agent — a heartbeat that says cold-only
// closes the gate even when the assertion claims live.
func TestManagedAgentLearnedEntitlementOverridesTheLocalAssertion(t *testing.T) {
	plane := newEntitlementPlane(t, false)
	service := openStandbyTestService(t, plane.server.URL)
	service.config.Entitlement.LiveMigration = true
	awaitHeartbeat(t, service, plane)
	if service.liveMigrationAllowed() {
		t.Fatal("the local assertion must not override the control plane's cold-only answer")
	}
}

// TestManagedAgentFailsClosedWithoutAControlPlaneAnswer: before the first
// successful heartbeat — and on a control plane too old to speak the field —
// a managed agent is cold-only. Fail closed, never open on missing data.
func TestManagedAgentFailsClosedWithoutAControlPlaneAnswer(t *testing.T) {
	plane := newEntitlementPlane(t, false)
	plane.mu.Lock()
	plane.allows = nil
	plane.mu.Unlock()
	service := openStandbyTestService(t, plane.server.URL)
	service.config.Entitlement.LiveMigration = true
	awaitHeartbeat(t, service, plane)
	if service.liveMigrationAllowed() {
		t.Fatal("a heartbeat without the entitlement field must leave the gate closed")
	}
}
