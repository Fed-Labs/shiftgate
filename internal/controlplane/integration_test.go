package controlplane

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/billing"
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
	// The dispatcher builds HTTPS agent clients with strict mTLS
	// (agentclient.NewWithTLS refuses to construct one without a control
	// identity), so the fixture carries a throwaway CA and client
	// certificate. The dispatch assertions then exercise the intended path:
	// the plan gate answers before contact, and the unreachable-agent errors
	// come from the dial, not from client construction.
	agentTLS := testAgentTLSMaterial(t)
	configuration.AgentTLS = config.AgentClientTLSConfig{CertificateFile: agentTLS.certificate, PrivateKeyFile: agentTLS.privateKey, PeerCAFile: agentTLS.certificateAuthority}
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

	// Remember-me: a login without the flag gets the short refresh lifetime,
	// a remembered login the full one. The session record itself is the
	// authority — rotation clamps to it, so this is the lifetime that binds.
	var shortLogin struct {
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/login", LoginRequest{Email: register.Email, Password: register.Password, Remember: false}, http.StatusOK, &shortLogin, "")
	shortRecord, err := store.AuthenticateAccess(ctx, tokenDigest(configuration.TokenPepper, shortLogin.Tokens.AccessToken), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var rememberedLogin struct {
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/login", LoginRequest{Email: register.Email, Password: register.Password, Remember: true}, http.StatusOK, &rememberedLogin, "")
	rememberedRecord, err := store.AuthenticateAccess(ctx, tokenDigest(configuration.TokenPepper, rememberedLogin.Tokens.AccessToken), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	maxShort := time.Now().UTC().Add(configuration.ShortRefreshTokenTTL + time.Minute)
	if shortRecord.RefreshExpiresAt.After(maxShort) {
		t.Fatalf("unremembered login refresh expiry %s exceeds the short TTL %s", shortRecord.RefreshExpiresAt, configuration.ShortRefreshTokenTTL)
	}
	minRemembered := time.Now().UTC().Add(configuration.RefreshTokenTTL - time.Minute)
	if rememberedRecord.RefreshExpiresAt.Before(minRemembered) {
		t.Fatalf("remembered login refresh expiry %s falls short of the full TTL %s", rememberedRecord.RefreshExpiresAt, configuration.RefreshTokenTTL)
	}

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
	postJSONWithHeaders(t, client, organizationPath+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: machine.MachineID, DestinationMachineID: destination.MachineID, Mode: "cold"}, http.StatusAccepted, &migration, authHeader, map[string]string{"Idempotency-Key": "migration-integration-1"})
	if migration.Status != "queued" || migration.WorkloadID != workload.ID {
		t.Fatalf("unexpected migration job: %#v", migration)
	}
	var replayed MigrationJob
	postJSONWithHeaders(t, client, organizationPath+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: machine.MachineID, DestinationMachineID: destination.MachineID, Mode: "cold"}, http.StatusAccepted, &replayed, authHeader, map[string]string{"Idempotency-Key": "migration-integration-1"})
	if replayed.ID != migration.ID {
		t.Fatalf("idempotent retry created a second migration: first=%s retry=%s", migration.ID, replayed.ID)
	}
	claimed, claimErr := store.ClaimNextManagedMigration(ctx, "integration-worker", time.Minute)
	if claimErr != nil {
		t.Fatalf("claim queued migration: %v", claimErr)
	}
	if claimed.AttemptCount < 1 || claimed.LeaseOwner != "integration-worker" || claimed.Status != "running" {
		t.Fatalf("unexpected claimed migration: %#v", claimed)
	}
	updated, updateErr := store.UpdateManagedMigration(ctx, claimed, "queued", claimed.Progress, "temporary test failure", "", time.Now().UTC().Add(time.Hour), time.Minute)
	if updateErr != nil {
		t.Fatalf("release migration lease for retry: %v", updateErr)
	}
	if updated.Status != "queued" || updated.LeaseOwner != "" || updated.AttemptCount < claimed.AttemptCount {
		t.Fatalf("migration lease was not released for retry: %#v", updated)
	}
	postJSONWithHeaders(t, client, organizationPath+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: machine.MachineID, DestinationMachineID: machine.MachineID, Mode: "cold"}, http.StatusConflict, &ErrorResponse{}, authHeader, map[string]string{"Idempotency-Key": "migration-integration-1"})

	// The free tier is cold-only: both platform-mediated migration paths —
	// the migration job route and the agent command dispatcher — refuse a
	// live mode before any work starts.
	var liveMigrationError ErrorResponse
	postJSON(t, client, organizationPath+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: machine.MachineID, DestinationMachineID: destination.MachineID, Mode: "live"}, http.StatusPaymentRequired, &liveMigrationError, authHeader)
	if liveMigrationError.Code != "LIVE_MIGRATION_PLAN_REQUIRED" {
		t.Fatalf("free plan live migration was not refused: %#v", liveMigrationError)
	}
	postJSON(t, client, organizationPath+"/machines/"+machine.MachineID+"/commands", AgentCommandRequest{
		Action: "migrate", WorkloadID: workload.ID, DestinationID: destination.MachineID, Mode: "live",
	}, http.StatusPaymentRequired, &liveMigrationError, authHeader)
	if liveMigrationError.Code != "LIVE_MIGRATION_PLAN_REQUIRED" {
		t.Fatalf("free plan live dispatch was not refused: %#v", liveMigrationError)
	}
	// The dispatcher's gate runs before the agent is even contacted, so this
	// ordering also proves the refusal is the plan check, not the
	// unreachable-agent error the cold command below reports.
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

	// The webhook's event id is unique per run: ApplyStripeSubscription is
	// idempotent by event id, so an id an earlier run already applied would
	// be recognized as a replay and this run's upgrade would never land.
	subscriptionEventID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	subscriptionPayload := `{"id":"evt_` + subscriptionEventID + `","type":"customer.subscription.created","data":{"object":{"id":"sub_1","customer":"cus_integration_1","status":"active","current_period_end":1893456000,"metadata":{"organization_id":"` + registration.Organization.ID + `","plan":"pro","max_storage_bytes":"999999999999","max_machines":"9999"}}}}`
	webhookRequest, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/webhooks/stripe", strings.NewReader(subscriptionPayload))
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
	// The asserted numbers come from the catalog itself so the contract is
	// "the catalog is enforced", not a frozen copy of its current values.
	proPlan, ok := billing.FindPlan("pro")
	if !ok {
		t.Fatal("pro plan missing from catalog")
	}
	if upgraded.Plan != "pro" || upgraded.MaxMachines != proPlan.MaxMachines || upgraded.MaxStorageBytes != billing.EffectiveStorageBytes(proPlan, 0) {
		t.Fatalf("subscription webhook did not apply catalog limits: %#v", upgraded)
	}
	if upgraded.StripeCustomerID != "cus_integration_1" {
		t.Fatalf("subscription webhook did not store the customer: %#v", upgraded)
	}
	// The paid plan unlocks live mode on both paths: the job route accepts
	// it, and the dispatcher proceeds past the plan gate to the (unreachable)
	// agent. One workload may only have one active migration, so the earlier
	// cold job is settled first — the queue contract, not a workaround.
	if _, settleErr := store.UpdateMigrationStatus(ctx, registration.Organization.ID, migration.ID, "completed", json.RawMessage(`{}`), "", database.AuditInput{ID: "audit-settle-" + migration.ID, OrganizationID: registration.Organization.ID, Action: "migration.complete", ResourceType: "migration", ResourceID: migration.ID, Metadata: map[string]any{}}); settleErr != nil {
		t.Fatalf("settle first migration: %v", settleErr)
	}
	var liveMigration MigrationJob
	postJSON(t, client, organizationPath+"/migrations", CreateMigrationRequest{WorkloadID: workload.ID, SourceMachineID: machine.MachineID, DestinationMachineID: destination.MachineID, Mode: "live"}, http.StatusAccepted, &liveMigration, authHeader)
	if liveMigration.Mode != "live" {
		t.Fatalf("pro plan live migration was refused: %#v", liveMigration)
	}
	// A second migrate for the same workload while the live job is active is
	// refused outright — the one-active-migration guard is the queue
	// contract, not a plan-gate result.
	var conflict ErrorResponse
	postJSON(t, client, organizationPath+"/machines/"+machine.MachineID+"/commands", AgentCommandRequest{
		Action: "migrate", WorkloadID: workload.ID, DestinationID: destination.MachineID, Mode: "live",
	}, http.StatusConflict, &conflict, authHeader)
	if conflict.Code != "MIGRATION_ALREADY_ACTIVE" {
		t.Fatalf("dispatch during an active migration did not refuse: %#v", conflict)
	}
	if _, settleErr := store.UpdateMigrationStatus(ctx, registration.Organization.ID, liveMigration.ID, "completed", json.RawMessage(`{}`), "", database.AuditInput{ID: "audit-settle-" + liveMigration.ID, OrganizationID: registration.Organization.ID, Action: "migration.complete", ResourceType: "migration", ResourceID: liveMigration.ID, Metadata: map[string]any{}}); settleErr != nil {
		t.Fatalf("settle live migration: %v", settleErr)
	}
	// With no active migration, the dispatcher proceeds past the plan gate
	// to the (unreachable) agent.
	var dispatchError ErrorResponse
	postJSON(t, client, organizationPath+"/machines/"+machine.MachineID+"/commands", AgentCommandRequest{
		Action: "migrate", WorkloadID: workload.ID, DestinationID: destination.MachineID, Mode: "live",
	}, http.StatusBadGateway, &dispatchError, authHeader)
	if dispatchError.Code != "AGENT_UNREACHABLE" {
		t.Fatalf("pro plan live dispatch did not pass the plan gate: %#v", dispatchError)
	}
	var portal map[string]string
	postJSON(t, client, organizationPath+"/billing/portal", map[string]any{"return_url": "https://app.example.com/billing"}, http.StatusOK, &portal, authHeader)
	if portal["url"] != "https://billing.stripe.com/session/portal_1" {
		t.Fatalf("unexpected portal response: %#v", portal)
	}
	// A second checkout while a subscription is active is refused — one
	// subscription per organization, changes go through the portal (the
	// double-billing guard).
	var repeatCheckoutError ErrorResponse
	postJSON(t, client, organizationPath+"/billing/checkout", map[string]any{
		"plan": "pro", "success_url": "https://app.example.com/billing?checkout=success",
		"cancel_url": "https://app.example.com/billing?checkout=cancel",
	}, http.StatusConflict, &repeatCheckoutError, authHeader)
	if repeatCheckoutError.Code != "SUBSCRIPTION_ACTIVE" {
		t.Fatalf("repeat checkout was not refused: %#v", repeatCheckoutError)
	}
	// Once the subscription is gone, a checkout reuses the stored customer
	// instead of the email.
	cancelPayload := `{"id":"evt_` + subscriptionEventID + `_cancel","type":"customer.subscription.deleted","data":{"object":{"id":"sub_1","customer":"cus_integration_1","status":"canceled","metadata":{"organization_id":"` + registration.Organization.ID + `","plan":"pro"}}}}`
	cancelRequest, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/webhooks/stripe", strings.NewReader(cancelPayload))
	cancelRequest.Header.Set("Content-Type", "application/json")
	cancelRequest.Header.Set("Stripe-Signature", stripeSignature("whsec_integration", cancelPayload, time.Now().Unix()))
	cancelResponse, err := client.Do(cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	cancelBody, _ := io.ReadAll(cancelResponse.Body)
	cancelResponse.Body.Close()
	if cancelResponse.StatusCode != http.StatusOK {
		t.Fatalf("cancel webhook status %d: %s", cancelResponse.StatusCode, cancelBody)
	}
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
	grantPlan(t, store, secondRegistration.Organization.ID, "business")
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+secondRegistration.Organization.ID+"/members", map[string]any{"email": register.Email, "role": "operator"}, http.StatusOK, nil, "Bearer "+secondRegistration.Tokens.AccessToken)
	expectStatus(t, client, http.MethodGet, httpServer.URL+"/v1/organizations/"+secondRegistration.Organization.ID+"/machines", http.StatusForbidden, apiKeyHeader, nil)
	expectStatus(t, client, http.MethodDelete, organizationPath+"/api-keys/"+apiKey.ID, http.StatusNoContent, authHeader, nil)
	expectStatus(t, client, http.MethodGet, organizationPath+"/machines", http.StatusUnauthorized, apiKeyHeader, nil)

	grantPlan(t, store, registration.Organization.ID, "business")
	var audit []AuditEvent
	getJSON(t, client, organizationPath+"/audit", http.StatusOK, &audit, authHeader)
	if len(audit) < 5 {
		t.Fatalf("expected audit events, got %d", len(audit))
	}

	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, httpServer.URL+"/ready", nil)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status %d", response.StatusCode)
	}
}

// testAgentTLSMaterial mints a throwaway certificate authority and a
// control-plane client certificate as inline PEM (the agent_tls config
// accepts PEM material directly). The hardened dispatcher refuses to build an
// HTTPS agent client without this identity, so integration fixtures need real
// material to reach the dispatch path at all.
func testAgentTLSMaterial(t *testing.T) (material struct{ certificate, privateKey, certificateAuthority string }) {
	t.Helper()
	notBefore := time.Now().Add(-time.Minute)
	notAfter := time.Now().Add(time.Hour)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "shift test CA"}, NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano() + 1), Subject: pkix.Name{CommonName: "control.example"}, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	material.certificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}))
	material.privateKey = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}))
	material.certificateAuthority = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return material
}

// grantPlan puts a test organization on a subscription that carries the
// catalog's tier features (team members, audit, SSO, SCIM), the way a paying
// customer's would be. Unique event ids keep it idempotent-safe across calls.
func grantPlan(t *testing.T, store *database.Store, organizationID, plan string) {
	t.Helper()
	eventID := "evt-test-" + plan + "-" + organizationID + "-" + time.Now().UTC().Format("150405.000000000")
	if _, err := store.ApplyStripeSubscription(context.Background(), eventID, "customer.subscription.updated", []byte(`{}`), organizationID, plan, "active", "cus_test", "sub_test", 2*1024*1024*1024*1024, -1); err != nil {
		t.Fatal(err)
	}
}

func postJSON(t *testing.T, client *http.Client, endpoint string, input any, expectedStatus int, output any, authorization string) {
	postJSONWithHeaders(t, client, endpoint, input, expectedStatus, output, authorization, nil)
}

func postJSONWithHeaders(t *testing.T, client *http.Client, endpoint string, input any, expectedStatus int, output any, authorization string, headers map[string]string) {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
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
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
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
	request, err := http.NewRequestWithContext(context.Background(), method, endpoint, body)
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
