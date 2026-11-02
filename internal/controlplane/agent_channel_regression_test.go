package controlplane

import (
	"bytes"
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

// Regression cover for the undelivered agent-command channel (ship finding 02
// blocker 1): agents poll machines/{id}/commands/poll and settle commands at
// commands/{id}/complete. The channel must be registered, machine-bound — a
// key for one machine must never claim or settle another machine's queue —
// and single-settlement. PostgreSQL-gated like the rest of the flow tests.

type agentChannelHarness struct {
	store       *database.Store
	server      *Server
	httpServer  *httptest.Server
	client      *http.Client
	orgID       string
	sessionAuth string
}

func newAgentChannelHarness(t *testing.T) *agentChannelHarness {
	t.Helper()
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	register := RegisterRequest{Email: "agent-channel-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test", Password: "correct horse battery staple", DisplayName: "Channel User", Organization: "Channel Organization"}
	var registration struct {
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, httpServer.Client(), httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	return &agentChannelHarness{
		store: store, server: server, httpServer: httpServer, client: httpServer.Client(),
		orgID:       registration.Organization.ID,
		sessionAuth: "Bearer " + registration.Tokens.AccessToken,
	}
}

// machineKey mints an API key bound to one machine via its name.
func (harness *agentChannelHarness) machineKey(t *testing.T, machineID string) string {
	t.Helper()
	var apiKey APIKeyCreated
	postJSON(t, harness.client, harness.httpServer.URL+"/v1/organizations/"+harness.orgID+"/api-keys",
		CreateAPIKeyRequest{Name: "machine:" + machineID, Scopes: []string{"machines"}},
		http.StatusCreated, &apiKey, harness.sessionAuth)
	return "Bearer " + apiKey.Secret
}

func (harness *agentChannelHarness) pollURL(machineID string) string {
	return harness.httpServer.URL + "/v1/organizations/" + harness.orgID + "/machines/" + machineID + "/commands/poll"
}

func (harness *agentChannelHarness) enqueue(t *testing.T, machineID, action string, payload json.RawMessage) string {
	t.Helper()
	commandID := "agent-channel-command-" + time.Now().UTC().Format("150405.000000000")
	if err := harness.store.EnqueueAgentCommand(context.Background(), database.AgentCommandRecord{
		ID: commandID, OrganizationID: harness.orgID, MachineID: machineID,
		Action: action, Payload: payload,
	}, database.AuditInput{ID: "audit-" + commandID, OrganizationID: harness.orgID, Action: "agent_command.enqueue", ResourceType: "agent_command", ResourceID: commandID, Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	return commandID
}

func TestAgentCommandChannelRegisteredAndMachineGated(t *testing.T) {
	harness := newAgentChannelHarness(t)
	machineAuth := harness.machineKey(t, "agent-channel-machine")
	const machineID = "agent-channel-machine"
	pollURL := harness.pollURL(machineID)

	// Registration proof: an unauthenticated call must be refused by auth,
	// not by the router (404 would mean the route is missing again).
	{
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, pollURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := harness.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
			t.Fatalf("commands/poll answered %d: the agent command channel is not registered", response.StatusCode)
		}
	}

	// User sessions must not claim or settle agent commands.
	channelExpectError(t, harness.client, http.MethodGet, pollURL, harness.sessionAuth, http.StatusForbidden, "AGENT_COMMAND_FORBIDDEN")
	channelExpectError(t, harness.client, http.MethodPost, harness.httpServer.URL+"/v1/organizations/"+harness.orgID+"/machines/"+machineID+"/commands/unclaimed/complete",
		harness.sessionAuth, http.StatusForbidden, "AGENT_COMMAND_FORBIDDEN")

	// The queue is empty before anything is enqueued.
	channelExpectStatus(t, harness.client, http.MethodGet, pollURL, machineAuth, http.StatusNoContent)

	// Enqueue one command the way the dispatcher does, then claim it.
	commandID := harness.enqueue(t, machineID, "migrate", json.RawMessage(`{"mode":"live"}`))
	var claimed struct {
		ID         string          `json:"id"`
		Action     string          `json:"action"`
		ClaimToken string          `json:"claim_token"`
		Payload    json.RawMessage `json:"payload"`
	}
	getJSON(t, harness.client, pollURL, http.StatusOK, &claimed, machineAuth)
	if claimed.ID != commandID || claimed.Action != "migrate" || claimed.ClaimToken == "" {
		t.Fatalf("claim returned %+v, want the enqueued command with a claim token", claimed)
	}

	completeURL := harness.httpServer.URL + "/v1/organizations/" + harness.orgID + "/machines/" + machineID + "/commands/" + commandID + "/complete"
	postJSON(t, harness.client, completeURL, map[string]any{"failed": false, "claim_token": claimed.ClaimToken}, http.StatusOK, nil, machineAuth)

	// Settlement is single-shot: a replayed completion (same claim token) is
	// refused, and one without the token is refused outright.
	channelExpectError(t, harness.client, http.MethodPost, completeURL, machineAuth, http.StatusConflict, "AGENT_COMMAND_COMPLETE_FAILED")
	channelExpectError(t, harness.client, http.MethodPost, completeURL, machineAuth, http.StatusConflict, "AGENT_COMMAND_COMPLETE_FAILED")

	// Nothing is left to claim: claim-lease watchdog rows are control traffic
	// and are never delivered as commands.
	channelExpectStatus(t, harness.client, http.MethodGet, pollURL, machineAuth, http.StatusNoContent)
}

func TestAgentCommandChannelIsMachineBound(t *testing.T) {
	harness := newAgentChannelHarness(t)
	const machineOne = "agent-channel-machine-one"
	const machineTwo = "agent-channel-machine-two"
	machineOneAuth := harness.machineKey(t, machineOne)
	machineTwoAuth := harness.machineKey(t, machineTwo)
	pollURL := harness.pollURL(machineOne)

	// A second machine's key must not claim this machine's queue: the queue is
	// bound to machine identity, not just to the organization.
	commandID := harness.enqueue(t, machineOne, "relay.join", json.RawMessage(`{"migration_id":"migration-one"}`))
	channelExpectError(t, harness.client, http.MethodGet, pollURL, machineTwoAuth, http.StatusForbidden, "AGENT_COMMAND_MACHINE_FORBIDDEN")
	channelExpectError(t, harness.client, http.MethodPost, harness.httpServer.URL+"/v1/organizations/"+harness.orgID+"/machines/"+machineOne+"/commands/"+commandID+"/complete",
		machineTwoAuth, http.StatusForbidden, "AGENT_COMMAND_MACHINE_FORBIDDEN")

	// An organization-wide key with no machine binding is refused too: the
	// channel is machine identity by contract.
	var unbound APIKeyCreated
	postJSON(t, harness.client, harness.httpServer.URL+"/v1/organizations/"+harness.orgID+"/api-keys",
		CreateAPIKeyRequest{Name: "fleet automation key", Scopes: []string{"machines"}},
		http.StatusCreated, &unbound, harness.sessionAuth)
	channelExpectError(t, harness.client, http.MethodGet, pollURL, "Bearer "+unbound.Secret, http.StatusForbidden, "AGENT_COMMAND_MACHINE_FORBIDDEN")

	// The bound key still claims and settles its own machine's command.
	var claimed struct {
		ID         string `json:"id"`
		ClaimToken string `json:"claim_token"`
	}
	getJSON(t, harness.client, pollURL, http.StatusOK, &claimed, machineOneAuth)
	if claimed.ID != commandID {
		t.Fatalf("bound key claimed %q, want %q", claimed.ID, commandID)
	}
	postJSON(t, harness.client, harness.httpServer.URL+"/v1/organizations/"+harness.orgID+"/machines/"+machineOne+"/commands/"+commandID+"/complete",
		map[string]any{"failed": false, "claim_token": claimed.ClaimToken}, http.StatusOK, nil, machineOneAuth)
}

func TestAgentCommandClaimLeaseRequeuesStrandedClaims(t *testing.T) {
	harness := newAgentChannelHarness(t)
	// A tiny lease makes the watchdog fire on the next poll: the stranded
	// claim (claimed, never settled) must be requeued for redelivery.
	harness.server.commandClaimLease = 10 * time.Millisecond
	const machineID = "agent-channel-machine"
	machineAuth := harness.machineKey(t, machineID)
	pollURL := harness.pollURL(machineID)

	commandID := harness.enqueue(t, machineID, "relay.start", json.RawMessage(`{"idempotency_key":"migration-job-1"}`))
	var first struct {
		ID      string          `json:"id"`
		Action  string          `json:"action"`
		Payload json.RawMessage `json:"payload"`
	}
	getJSON(t, harness.client, pollURL, http.StatusOK, &first, machineAuth)
	if first.ID != commandID {
		t.Fatalf("first claim returned %q, want %q", first.ID, commandID)
	}

	// The agent crashes without settling. Past the lease, the next poll must
	// deliver a redelivery of the same work (same action and payload, fresh
	// identity) instead of leaving the claim stranded forever.
	time.Sleep(30 * time.Millisecond)
	var second struct {
		ID         string          `json:"id"`
		Action     string          `json:"action"`
		Payload    json.RawMessage `json:"payload"`
		ClaimToken string          `json:"claim_token"`
	}
	getJSON(t, harness.client, pollURL, http.StatusOK, &second, machineAuth)
	if second.ID == first.ID {
		t.Fatalf("stranded claim %q was redelivered under the same identity; want a fresh command id", second.ID)
	}
	if second.Action != first.Action || string(second.Payload) != string(first.Payload) {
		t.Fatalf("redelivery changed the command: got %s %s, want %s %s", second.Action, second.Payload, first.Action, first.Payload)
	}

	// A settled command is not redelivered when its lease expires.
	postJSON(t, harness.client, harness.httpServer.URL+"/v1/organizations/"+harness.orgID+"/machines/"+machineID+"/commands/"+second.ID+"/complete",
		map[string]any{"failed": false, "claim_token": second.ClaimToken}, http.StatusOK, nil, machineAuth)
	time.Sleep(30 * time.Millisecond)
	channelExpectStatus(t, harness.client, http.MethodGet, pollURL, machineAuth, http.StatusNoContent)
	channelExpectStatus(t, harness.client, http.MethodGet, pollURL, machineAuth, http.StatusNoContent)
}

// channelExpectStatus issues one request and demands an exact status code.
func channelExpectStatus(t *testing.T, client *http.Client, method, endpoint, authorization string, want int) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != want {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status %d, want %d: %s", method, endpoint, response.StatusCode, want, content)
	}
}

// channelExpectError issues one request and demands a status plus the stable
// error code in the body.
func channelExpectError(t *testing.T, client *http.Client, method, endpoint, authorization string, wantStatus int, wantCode string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, endpoint, bytes.NewReader([]byte(`{"failed":false}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(response.Body)
	if response.StatusCode != wantStatus {
		t.Fatalf("%s %s status %d, want %d: %s", method, endpoint, response.StatusCode, wantStatus, content)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(content, &body); err != nil || body.Code != wantCode {
		t.Fatalf("%s %s error = %s, want code %s", method, endpoint, content, wantCode)
	}
}
