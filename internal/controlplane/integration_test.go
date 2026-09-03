package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func TestPostgreSQLControlPlaneFlow(t *testing.T) {
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
	configuration := config.DefaultControlPlane()
	configuration.DatabaseURL = databaseURL
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	configuration.StripeSecretKey = "sk_test_integration"
	configuration.StripeWebhookKey = "whsec_integration"
	configuration.StripePrices = map[string]string{"pro": "price_pro_monthly"}
	stripeAPI := newFakeStripeAPI(t)
	server := New(configuration, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.stripe.SetBaseURL(stripeAPI.server.URL)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	register := RegisterRequest{Email: "integration-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test", Password: "correct horse battery staple", DisplayName: "Integration User", Organization: "Integration Organization"}
	var registration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	if registration.User.ID == "" || registration.Organization.ID == "" || registration.Tokens.AccessToken == "" {
		t.Fatalf("registration returned incomplete data: %#v", registration)
	}

	var login struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/login", LoginRequest{Email: register.Email, Password: register.Password}, http.StatusOK, &login, "")
	if login.Tokens.AccessToken == registration.Tokens.AccessToken {
		t.Fatal("login reused the registration access token")
	}
	authHeader := "Bearer " + login.Tokens.AccessToken

	var organizations []Organization
	getJSON(t, client, httpServer.URL+"/v1/organizations", http.StatusOK, &organizations, authHeader)
	if len(organizations) != 1 || organizations[0].ID != registration.Organization.ID || organizations[0].Role != RoleOwner {
		t.Fatalf("unexpected organizations: %#v", organizations)
	}

	organizationPath := httpServer.URL + "/v1/organizations/" + registration.Organization.ID
	var machine Machine
	postJSON(t, client, organizationPath+"/machines", CreateMachineRequest{MachineID: "machine-source", Name: "Source", AgentURL: "https://source.example:8443", Capabilities: map[string]any{"architecture": "amd64"}}, http.StatusCreated, &machine, authHeader)
	var destination Machine
	postJSON(t, client, organizationPath+"/machines", CreateMachineRequest{MachineID: "machine-destination", Name: "Destination", AgentURL: "https://destination.example:8443"}, http.StatusCreated, &destination, authHeader)
	if machine.MachineID == destination.MachineID {
		t.Fatal("machine identities collided")
	}

	var workload Workload
	postJSON(t, client, organizationPath+"/workloads", CreateWorkloadRequest{MachineID: machine.MachineID, Name: "integration-workload", Spec: map[string]any{"command": []any{"/bin/true"}}}, http.StatusCreated, &workload, authHeader)
	if workload.ID == "" || workload.MachineID != machine.MachineID {
		t.Fatalf("unexpected workload: %#v", workload)
	}

	var migration MigrationJob
	postJSON(t, client, organizationPath+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: machine.MachineID, DestinationMachineID: destination.MachineID, Mode: "cold"}, http.StatusAccepted, &migration, authHeader)
	if migration.Status != "queued" || migration.WorkloadID != workload.ID {
		t.Fatalf("unexpected migration job: %#v", migration)
	}

	var agentError ErrorResponse
	postJSON(t, client, organizationPath+"/machines/machine-source/commands", AgentCommandRequest{
		Action: "start", WorkloadID: workload.ID,
	}, http.StatusBadGateway, &agentError, authHeader)
	if agentError.Code != "AGENT_UNREACHABLE" {
		t.Fatalf("unexpected agent command error: %#v", agentError)
	}

	checkpointID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint Checkpoint
	postJSON(t, client, organizationPath+"/checkpoints", CreateCheckpointRequest{
		ID: checkpointID, WorkloadID: workload.ID, MachineID: machine.MachineID,
		Kind: "full", Manifest: map[string]any{"format": "shift-state", "version": "1.0.0"},
		PlainBytes: 4096, StoredBytes: 2048, ChunkCount: 2,
	}, http.StatusCreated, &checkpoint, authHeader)
	if checkpoint.ID != checkpointID || checkpoint.Status != "available" || checkpoint.StoredBytes != 2048 {
		t.Fatalf("unexpected checkpoint metadata: %#v", checkpoint)
	}
	var checkpoints []Checkpoint
	getJSON(t, client, organizationPath+"/checkpoints?workload_id="+workload.ID, http.StatusOK, &checkpoints, authHeader)
	if len(checkpoints) != 1 || checkpoints[0].ID != checkpointID {
		t.Fatalf("unexpected checkpoint list: %#v", checkpoints)
	}
	var entitlement Entitlement
	getJSON(t, client, organizationPath+"/entitlement", http.StatusOK, &entitlement, authHeader)
	if entitlement.UsedStorageBytes != checkpoint.StoredBytes {
		t.Fatalf("checkpoint storage was not accounted against entitlement: %#v", entitlement)
	}
	invalidCheckpointID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	postJSON(t, client, organizationPath+"/checkpoints", CreateCheckpointRequest{
		ID: invalidCheckpointID, WorkloadID: workload.ID, MachineID: destination.MachineID,
		Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1,
	}, http.StatusConflict, nil, authHeader)
	parentID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	invalidCheckpointID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	postJSON(t, client, organizationPath+"/checkpoints", CreateCheckpointRequest{
		ID: invalidCheckpointID, WorkloadID: workload.ID, MachineID: machine.MachineID,
		Kind: "incremental", ParentID: parentID, PlainBytes: 1, StoredBytes: 1, ChunkCount: 1,
	}, http.StatusUnprocessableEntity, nil, authHeader)
	if _, err := store.ApplyStripeSubscription(ctx, "evt-limit-"+checkpointID, "customer.subscription.updated", []byte(`{}`), registration.Organization.ID, "free", "active", "", "", checkpoint.StoredBytes, 2); err != nil {
		t.Fatal(err)
	}
	invalidCheckpointID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	postJSON(t, client, organizationPath+"/checkpoints", CreateCheckpointRequest{
		ID: invalidCheckpointID, WorkloadID: workload.ID, MachineID: machine.MachineID,
		Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1,
	}, http.StatusPaymentRequired, nil, authHeader)

	eventID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMigrationEvent(ctx, database.MigrationEventRecord{
		ID: eventID, MigrationID: migration.ID, Sequence: 2, Stage: "TRANSFER",
		Message: "transferring checkpoint", Progress: 0.5, BytesDone: 512, BytesTotal: 1024,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var events []MigrationEvent
	getJSON(t, client, organizationPath+"/migrations/"+migration.ID+"/events", http.StatusOK, &events, authHeader)
	if len(events) != 1 || events[0].ID != eventID || events[0].Progress != 0.5 {
		t.Fatalf("unexpected migration events: %#v", events)
	}

	usageID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	periodStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if err := store.AddUsage(ctx, database.UsageRecord{
		ID: usageID, OrganizationID: registration.Organization.ID, Kind: "checkpoint_storage", Quantity: 2048,
		PeriodStart: periodStart, PeriodEnd: periodEnd, Metadata: json.RawMessage(`{"checkpoint_id":"` + checkpointID + `"}`), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	var usage []UsageSummary
	getJSON(t, client, httpServer.URL+"/v1/organizations/"+registration.Organization.ID+"/usage?from=2026-08-01&to=2026-09-01", http.StatusOK, &usage, authHeader)
	if len(usage) != 1 || usage[0].Kind != "checkpoint_storage" || usage[0].Quantity != 2048 {
		t.Fatalf("unexpected usage response: %#v", usage)
	}

	// Billing: the public plan catalog, a checkout that only ever carries the
	// organization and plan to Stripe, a webhook that applies catalog limits,
	// and a portal that requires an existing customer.
	var publicPlans []map[string]any
	getJSON(t, client, httpServer.URL+"/v1/plans", http.StatusOK, &publicPlans, "")
	if len(publicPlans) != 4 {
		t.Fatalf("public plans response had %d entries", len(publicPlans))
	}
	var checkout map[string]string
	postJSON(t, client, organizationPath+"/billing/checkout", map[string]any{
		"plan": "pro", "success_url": "https://app.example.com/billing?checkout=success",
		"cancel_url": "https://app.example.com/billing?checkout=cancel",
	}, http.StatusOK, &checkout, authHeader)
	if checkout["url"] != "https://checkout.stripe.com/c/pay/cs_test_1" || checkout["plan"] != "pro" {
		t.Fatalf("unexpected checkout response: %#v", checkout)
	}
	form := stripeAPI.lastCheckoutForm()
	if form.Get("metadata[organization_id]") != registration.Organization.ID || form.Get("metadata[plan]") != "pro" {
		t.Fatalf("checkout metadata routed wrong: %v", form)
	}
	if form.Get("line_items[0][price]") != "price_pro_monthly" {
		t.Fatalf("checkout used wrong price: %v", form)
	}
	if form.Get("customer_email") != register.Email {
		t.Fatalf("checkout email = %q, want the caller's", form.Get("customer_email"))
	}
	var portalError ErrorResponse
	postJSON(t, client, organizationPath+"/billing/portal", map[string]any{"return_url": "https://app.example.com/billing"}, http.StatusBadRequest, &portalError, authHeader)
	if portalError.Code != "PORTAL_NO_CUSTOMER" {
		t.Fatalf("portal before any subscription: %#v", portalError)
	}

	subscriptionPayload := `{"id":"evt_sub_created","type":"customer.subscription.created","data":{"object":{"id":"sub_1","customer":"cus_integration_1","status":"active","current_period_end":1893456000,"metadata":{"organization_id":"` + registration.Organization.ID + `","plan":"pro","max_storage_bytes":"999999999999","max_machines":"9999"}}}}`
	webhookRequest, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/v1/webhooks/stripe", strings.NewReader(subscriptionPayload))
	webhookRequest.Header.Set("Content-Type", "application/json")
	webhookRequest.Header.Set("Stripe-Signature", stripeSignature("whsec_integration", subscriptionPayload, time.Now().Unix()))
	webhookResponse, err := client.Do(webhookRequest)
	if err != nil {
		t.Fatal(err)
	}
	webhookBody, _ := io.ReadAll(webhookResponse.Body)
	webhookResponse.Body.Close()
	if webhookResponse.StatusCode != http.StatusOK {
		t.Fatalf("stripe webhook status %d: %s", webhookResponse.StatusCode, webhookBody)
	}
	var upgraded Entitlement
	getJSON(t, client, organizationPath+"/entitlement", http.StatusOK, &upgraded, authHeader)
	// Catalog limits win over the webhook metadata's inflated numbers.
	if upgraded.Plan != "pro" || upgraded.MaxMachines != 10 || upgraded.MaxStorageBytes != 500*1024*1024*1024 {
		t.Fatalf("subscription webhook did not apply catalog limits: %#v", upgraded)
	}
	if upgraded.StripeCustomerID != "cus_integration_1" {
		t.Fatalf("subscription webhook did not store the customer: %#v", upgraded)
	}
	var portal map[string]string
	postJSON(t, client, organizationPath+"/billing/portal", map[string]any{"return_url": "https://app.example.com/billing"}, http.StatusOK, &portal, authHeader)
	if portal["url"] != "https://billing.stripe.com/session/portal_1" {
		t.Fatalf("unexpected portal response: %#v", portal)
	}
	// A later checkout reuses the stored customer instead of the email.
	postJSON(t, client, organizationPath+"/billing/checkout", map[string]any{
		"plan": "pro", "success_url": "https://app.example.com/billing?checkout=success",
		"cancel_url": "https://app.example.com/billing?checkout=cancel",
	}, http.StatusOK, &checkout, authHeader)
	form = stripeAPI.lastCheckoutForm()
	if form.Get("customer") != "cus_integration_1" || form.Get("customer_email") != "" {
		t.Fatalf("repeat checkout ignored stored customer: %v", form)
	}

	var apiKey APIKeyCreated
	postJSON(t, client, organizationPath+"/api-keys", CreateAPIKeyRequest{Name: "machine automation", Scopes: []string{"machines"}}, http.StatusCreated, &apiKey, authHeader)
	if apiKey.ID == "" || apiKey.Secret == "" || apiKey.Prefix == "" {
		t.Fatalf("API key response did not include one-time secret metadata: %#v", apiKey)
	}
	var apiKeys []APIKey
	getJSON(t, client, organizationPath+"/api-keys", http.StatusOK, &apiKeys, authHeader)
	if len(apiKeys) != 1 || apiKeys[0].ID != apiKey.ID {
		t.Fatalf("unexpected API key list: %#v", apiKeys)
	}
	apiKeyHeader := "Bearer " + apiKey.Secret
	var keyedMachines []Machine
	getJSON(t, client, organizationPath+"/machines", http.StatusOK, &keyedMachines, apiKeyHeader)
	if len(keyedMachines) != 2 {
		t.Fatalf("API key could not read machines: %#v", keyedMachines)
	}
	expectStatus(t, client, http.MethodGet, organizationPath+"/workloads", http.StatusForbidden, apiKeyHeader, nil)

	var secondRegistration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	secondRegister := RegisterRequest{Email: "integration-second-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test", Password: "correct horse battery staple", DisplayName: "Second User", Organization: "Second Organization"}
	postJSON(t, client, httpServer.URL+"/v1/auth/register", secondRegister, http.StatusCreated, &secondRegistration, "")
	invalidCheckpointID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+secondRegistration.Organization.ID+"/checkpoints", CreateCheckpointRequest{
		ID: invalidCheckpointID, WorkloadID: workload.ID, MachineID: machine.MachineID,
		Kind: "full", PlainBytes: 1, StoredBytes: 1, ChunkCount: 1,
	}, http.StatusNotFound, nil, "Bearer "+secondRegistration.Tokens.AccessToken)
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+secondRegistration.Organization.ID+"/members", map[string]any{"email": register.Email, "role": "operator"}, http.StatusOK, nil, "Bearer "+secondRegistration.Tokens.AccessToken)
	expectStatus(t, client, http.MethodGet, httpServer.URL+"/v1/organizations/"+secondRegistration.Organization.ID+"/machines", http.StatusForbidden, apiKeyHeader, nil)
	expectStatus(t, client, http.MethodDelete, organizationPath+"/api-keys/"+apiKey.ID, http.StatusNoContent, authHeader, nil)
	expectStatus(t, client, http.MethodGet, organizationPath+"/machines", http.StatusUnauthorized, apiKeyHeader, nil)

	var audit []AuditEvent
	getJSON(t, client, organizationPath+"/audit", http.StatusOK, &audit, authHeader)
	if len(audit) < 5 {
		t.Fatalf("expected audit events, got %d", len(audit))
	}

	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/ready", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status %d", response.StatusCode)
	}
}

func postJSON(t *testing.T, client *http.Client, endpoint string, input any, expectedStatus int, output any, authorization string) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
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
	if response.StatusCode != expectedStatus {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("POST %s status %d, want %d: %s", endpoint, response.StatusCode, expectedStatus, content)
	}
	if output != nil && json.NewDecoder(response.Body).Decode(output) != nil {
		t.Fatalf("decode POST %s response", endpoint)
	}
}

func getJSON(t *testing.T, client *http.Client, endpoint string, expectedStatus int, output any, authorization string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
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
	if response.StatusCode != expectedStatus {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("GET %s status %d, want %d: %s", endpoint, response.StatusCode, expectedStatus, content)
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		t.Fatalf("decode GET %s response: %v", endpoint, err)
	}
}

func expectStatus(t *testing.T, client *http.Client, method, endpoint string, expectedStatus int, authorization string, input any) {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status %d, want %d: %s", method, endpoint, response.StatusCode, expectedStatus, content)
	}
}

// fakeStripeAPI is a stand-in Stripe REST API recording checkout requests.
type fakeStripeAPI struct {
	server *httptest.Server
	mutex  sync.Mutex
	forms  []url.Values
}

func newFakeStripeAPI(t *testing.T) *fakeStripeAPI {
	t.Helper()
	api := &fakeStripeAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		form, _ := url.ParseQuery(string(body))
		api.mutex.Lock()
		api.forms = append(api.forms, form)
		api.mutex.Unlock()
		if request.URL.Path == "/v1/checkout/sessions" {
			_, _ = writer.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1"}`))
			return
		}
		if request.URL.Path == "/v1/billing_portal/sessions" {
			_, _ = writer.Write([]byte(`{"url":"https://billing.stripe.com/session/portal_1"}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (api *fakeStripeAPI) lastCheckoutForm() url.Values {
	api.mutex.Lock()
	defer api.mutex.Unlock()
	if len(api.forms) == 0 {
		return url.Values{}
	}
	return api.forms[len(api.forms)-1]
}

// stripeSignature produces the Stripe-Signature header value for a payload.
func stripeSignature(secret string, payload string, timestamp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strconv.FormatInt(timestamp, 10) + "." + payload))
	return "t=" + strconv.FormatInt(timestamp, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
