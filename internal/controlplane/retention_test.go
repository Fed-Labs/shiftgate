package controlplane

import (
	"context"
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

// openRetentionTestServer boots a control plane against the integration
// database and registers one organization, returning the server and that
// organization's path. Serves the same guard as the integration test: the
// retention and capability routes are database behavior.
func openRetentionTestServer(t *testing.T) (*httptest.Server, string, string) {
	t.Helper()
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

	register := RegisterRequest{Email: "retention-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test", Password: "correct horse battery staple", DisplayName: "Retention User", Organization: "Retention Organization"}
	var registration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, httpServer.Client(), httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	return httpServer, registration.Organization.ID, "Bearer " + registration.Tokens.AccessToken
}

func TestRetentionPolicyRoutes(t *testing.T) {
	httpServer, organizationID, authHeader := openRetentionTestServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	// Defaults before any policy exists.
	var policy RetentionPolicyResponse
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.AuditRetentionDays != 365 || policy.CheckpointRetentionDays != 90 || policy.DeletedStorageRetentionDays != 30 {
		t.Fatalf("unexpected default policy: %+v", policy)
	}

	// A partial update names one window; the others keep their value.
	days := 60
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &days})
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.AuditRetentionDays != 60 || policy.CheckpointRetentionDays != 90 || policy.DeletedStorageRetentionDays != 30 {
		t.Fatalf("partial update did not leave unnamed windows alone: %+v", policy)
	}

	// Zero is keep-forever and must survive the round trip.
	keepForever := 0
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &keepForever})
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.AuditRetentionDays != 0 {
		t.Fatalf("keep-forever zero did not round-trip: %+v", policy)
	}

	// Negative windows are refused before anything is written.
	negative := -1
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &negative})
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.AuditRetentionDays != 0 {
		t.Fatalf("rejected update changed the policy: %+v", policy)
	}

	// Unknown fields fail loudly: a typo'd window name must not be silently
	// ignored while the caller believes their policy is in force.
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, map[string]any{"audit_retention_dayz": 30})

	// Viewers of the retention policy need the admin role: an anonymous read
	// is not authorized to learn how long records survive.
	expectStatus(t, client, http.MethodGet, organizationPath+"/retention", http.StatusUnauthorized, "", nil)
}

func TestMachineCapabilityRoute(t *testing.T) {
	httpServer, organizationID, authHeader := openRetentionTestServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	postJSON(t, client, organizationPath+"/machines", CreateMachineRequest{MachineID: "cap-heavy", Name: "Heavy", AgentURL: "https://heavy.example:8443", Capabilities: map[string]any{"gpu_count": 4.0, "cuda_version": "12.4", "supports_live_migration": true}}, http.StatusCreated, &Machine{}, authHeader)
	postJSON(t, client, organizationPath+"/machines", CreateMachineRequest{MachineID: "cap-light", Name: "Light", AgentURL: "https://light.example:8443", Capabilities: map[string]any{"gpu_count": 1.0, "cuda_version": "11.8"}}, http.StatusCreated, &Machine{}, authHeader)

	var machines []Machine
	getJSON(t, client, organizationPath+"/machines/capability?capability=gpu_count&kind=number", http.StatusOK, &machines, authHeader)
	if len(machines) != 2 || machines[0].MachineID != "cap-heavy" {
		t.Fatalf("numeric capability query returned %v", machines)
	}

	getJSON(t, client, organizationPath+"/machines/capability?capability=gpu_count&kind=number&minimum=2", http.StatusOK, &machines, authHeader)
	if len(machines) != 1 || machines[0].MachineID != "cap-heavy" {
		t.Fatalf("minimum filter returned %v", machines)
	}

	getJSON(t, client, organizationPath+"/machines/capability?capability=supports_live_migration&kind=boolean", http.StatusOK, &machines, authHeader)
	if len(machines) != 1 || machines[0].MachineID != "cap-heavy" {
		t.Fatalf("boolean capability query returned %v", machines)
	}

	// A heartbeat that changes the document is reflected in the projection.
	var heartbeat Machine
	postJSON(t, client, organizationPath+"/machines/cap-light/heartbeat", map[string]any{"capabilities": map[string]any{"gpu_count": 8.0}}, http.StatusOK, &heartbeat, authHeader)
	getJSON(t, client, organizationPath+"/machines/capability?capability=gpu_count&kind=number", http.StatusOK, &machines, authHeader)
	if len(machines) != 2 || machines[0].MachineID != "cap-light" {
		t.Fatalf("heartbeat did not update the projection: %v", machines)
	}

	// Malformed queries are rejected rather than guessed at.
	getJSON(t, client, organizationPath+"/machines/capability?capability=gpu_count&kind=array", http.StatusBadRequest, &ErrorResponse{}, authHeader)
	getJSON(t, client, organizationPath+"/machines/capability?kind=number", http.StatusBadRequest, &ErrorResponse{}, authHeader)
	getJSON(t, client, organizationPath+"/machines/capability?capability=gpu_count&kind=text&minimum=2", http.StatusBadRequest, &ErrorResponse{}, authHeader)

	// Empty result is a real answer, not an error.
	getJSON(t, client, organizationPath+"/machines/capability?capability=nope&kind=text", http.StatusOK, &machines, authHeader)
	if len(machines) != 0 {
		t.Fatalf("unknown capability returned %v", machines)
	}
}
