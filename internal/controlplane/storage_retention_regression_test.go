package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
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
	"shift.dev/shift/internal/scheduler"
)

// storage_retention_regression_test.go: the report 16 guards on the retention
// policy surface (explicit create, checkpoint window floor, deterministic
// validation, plan caps) plus the usage-summary and compute-trust response
// contracts. DB-backed cases skip without SHIFT_TEST_DATABASE_URL.

// openRegressionServer boots a control plane against the integration database
// and registers one organization, returning the server, the store, and that
// organization's credentials. Like openRetentionTestServer, but the store is
// exposed for plan grants and entitlement pokes.
func openRegressionServer(t *testing.T) (*httptest.Server, *Server, *database.Store, string, string) {
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
	// The operator shared secret /metrics and /v1/admin accept; the
	// scoreboard tests exercise that gate with it.
	configuration.MetricsToken = "regression-metrics-token"
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
	return httpServer, server, store, registration.Organization.ID, "Bearer " + registration.Tokens.AccessToken
}

// registerOrgWith creates one more organization with the given owner email and
// returns its id and the owner's bearer token. The email is made unique per
// run so reruns against the same database do not collide on registration.
func registerOrgWith(t *testing.T, httpServer *httptest.Server, email, organization string) (string, string) {
	t.Helper()
	unique := fmt.Sprintf("run%d-", time.Now().UnixNano())
	register := RegisterRequest{Email: unique + email, Password: "correct horse battery staple", DisplayName: "Regression User", Organization: organization}
	var registration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, httpServer.Client(), httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	return registration.Organization.ID, "Bearer " + registration.Tokens.AccessToken
}

// TestRetentionPolicyFirstWriteRequiresAllWindows: creating a policy must
// name every window. A partial first write used to merge the displayed
// defaults into the new row, silently arming 90-day checkpoint deletion off
// an unrelated audit knob (report 16 CRITICAL). The refusal must also leave
// the organization untouched: still defaults, still no destructive window.
func TestRetentionPolicyFirstWriteRequiresAllWindows(t *testing.T) {
	httpServer, _, _, organizationID, authHeader := openRegressionServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	days := 60
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &days})
	// The partial write is refused, and reading still shows the documented
	// defaults rather than a half-materialized row.
	var policy RetentionPolicyResponse
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.AuditRetentionDays != 365 || policy.CheckpointRetentionDays != 90 || policy.DeletedStorageRetentionDays != 30 {
		t.Fatalf("partial first write changed the policy: %+v", policy)
	}
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, SetRetentionPolicyRequest{CheckpointRetentionDays: &days})
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, SetRetentionPolicyRequest{DeletedStorageRetentionDays: &days})

	// Naming all three creates the policy.
	auditDays, checkpointDays, deletedStorageDays := 60, 90, 30
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &auditDays, CheckpointRetentionDays: &checkpointDays, DeletedStorageRetentionDays: &deletedStorageDays})
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.AuditRetentionDays != 60 || policy.CheckpointRetentionDays != 90 || policy.DeletedStorageRetentionDays != 30 {
		t.Fatalf("explicit create did not round-trip: %+v", policy)
	}
}

// TestRetentionPolicyCheckpointWindowFloor: the checkpoint sweep destroys
// history, so a nonzero checkpoint window is at least one week — "delete
// everything older than a day" is a footgun, not a policy. Zero (keep
// forever) stays legal at the boundary.
func TestRetentionPolicyCheckpointWindowFloor(t *testing.T) {
	httpServer, _, _, organizationID, authHeader := openRegressionServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	auditDays, checkpointDays, deletedStorageDays := 60, 90, 30
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &auditDays, CheckpointRetentionDays: &checkpointDays, DeletedStorageRetentionDays: &deletedStorageDays})

	tooShort := 1
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, SetRetentionPolicyRequest{CheckpointRetentionDays: &tooShort})
	belowFloor := 6
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusBadRequest, authHeader, SetRetentionPolicyRequest{CheckpointRetentionDays: &belowFloor})
	forever := 0
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{CheckpointRetentionDays: &forever})
	atFloor := 7
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{CheckpointRetentionDays: &atFloor})

	var policy RetentionPolicyResponse
	getJSON(t, client, organizationPath+"/retention", http.StatusOK, &policy, authHeader)
	if policy.CheckpointRetentionDays != 7 {
		t.Fatalf("checkpoint window after floor checks: %+v", policy)
	}
}

// TestRetentionPolicyValidationOrderIsDeterministic: with several offending
// fields, the named field is stable (audit first), never map-random.
func TestRetentionPolicyValidationOrderIsDeterministic(t *testing.T) {
	httpServer, _, _, organizationID, authHeader := openRegressionServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	auditDays, checkpointDays, deletedStorageDays := 60, 90, 30
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &auditDays, CheckpointRetentionDays: &checkpointDays, DeletedStorageRetentionDays: &deletedStorageDays})

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, organizationPath+"/retention", strings.NewReader(`{"audit_retention_days":-1,"checkpoint_retention_days":-1,"deleted_storage_retention_days":-1}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", authHeader)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "audit_retention_days") {
		t.Fatalf("multi-field rejection must name audit_retention_days first: status %d body %s", response.StatusCode, body)
	}
}

// TestRetentionPolicyPlanCaps: the plan cap is checked per named field and a
// lookup failure never masquerades as a plan limit.
func TestRetentionPolicyPlanCaps(t *testing.T) {
	httpServer, _, store, organizationID, authHeader := openRegressionServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	auditDays, checkpointDays, deletedStorageDays := 60, 90, 30
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &auditDays, CheckpointRetentionDays: &checkpointDays, DeletedStorageRetentionDays: &deletedStorageDays})

	// Free tier caps at 365 days.
	long := 400
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusPaymentRequired, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &long})

	// Business — and negotiated keys the catalog does not know — may run to
	// the hard ceiling.
	grantPlan(t, store, organizationID, "business")
	expectStatus(t, client, http.MethodPut, organizationPath+"/retention", http.StatusOK, authHeader, SetRetentionPolicyRequest{AuditRetentionDays: &long})
}

// TestUsageSummaryMissingEntitlementIs404: a missing entitlement is a clean
// 404 on the usage summary, matching the storage routes — not a 500
// (report 16 MEDIUM).
func TestUsageSummaryMissingEntitlementIs404(t *testing.T) {
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

	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/organizations/org_without_entitlement/usage/summary", nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{UserID: "user_1", Role: RoleAdmin}))
	request.SetPathValue("organizationID", "org_without_entitlement")
	recorder := httptest.NewRecorder()
	server.handleUsageSummary(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("missing entitlement status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	if decoded["code"] != "ENTITLEMENT_NOT_FOUND" {
		t.Fatalf("missing entitlement code = %v", decoded["code"])
	}
}

// TestUsageSummaryUnknownPlanIsUnlimited: a negotiated plan key outside the
// catalog reports -1 (unlimited/unknown), never 0 budgets and 0 seats
// (report 16 LOW).
func TestUsageSummaryUnknownPlanIsUnlimited(t *testing.T) {
	httpServer, _, store, organizationID, authHeader := openRegressionServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	grantPlan(t, store, organizationID, "negotiated-vip")
	var summary UsageSummaryResponse
	getJSON(t, client, organizationPath+"/usage/summary", http.StatusOK, &summary, authHeader)
	if summary.APIRequests.DailyBudget != -1 {
		t.Fatalf("unknown plan daily budget = %d, want -1 (unlimited/unknown)", summary.APIRequests.DailyBudget)
	}
	if summary.Seats.Included != -1 {
		t.Fatalf("unknown plan included seats = %d, want -1 (unlimited/unknown)", summary.Seats.Included)
	}
}

// TestComputeOfferResponseClampsClaimedTrust: the inventory shows exactly the
// trust tier placement would honor — a publisher's "verified" claim is
// clamped for foreign viewers and for same-org viewers without verified
// identity, so the marketplace can never advertise a badge the scheduler
// ignores (report 16 LOW).
func TestComputeOfferResponseClampsClaimedTrust(t *testing.T) {
	offer := scheduler.Offer{ID: "offer-trust", OrganizationID: "org_owner", Trust: scheduler.TrustVerified}

	// A foreign organization sees the community ceiling.
	response := computeOfferResponse(offer, nil, "org_other")
	if response.Trust != scheduler.TrustCommunity {
		t.Fatalf("cross-org trust = %q, want %q", response.Trust, scheduler.TrustCommunity)
	}
	// Same organization without verified identity sees the organization
	// ceiling.
	response = computeOfferResponse(offer, nil, "org_owner")
	if response.Trust != scheduler.TrustOrganization {
		t.Fatalf("same-org trust = %q, want %q", response.Trust, scheduler.TrustOrganization)
	}
	// Same organization with verified identity keeps "verified".
	offer.IdentityVerified = true
	response = computeOfferResponse(offer, nil, "org_owner")
	if response.Trust != scheduler.TrustVerified {
		t.Fatalf("verified same-org trust = %q, want %q", response.Trust, scheduler.TrustVerified)
	}
	// Invalid claims degrade to unverified rather than pass through.
	offer.Trust = scheduler.TrustTier("made-up")
	response = computeOfferResponse(offer, nil, "org_other")
	if response.Trust != scheduler.TrustUnverified {
		t.Fatalf("bogus trust claim = %q, want %q", response.Trust, scheduler.TrustUnverified)
	}
}
