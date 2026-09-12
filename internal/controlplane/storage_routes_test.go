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
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
)

// storage_routes_test.go: the hosted-storage surface against an in-process
// control plane. The store and the STS endpoint are one fake HTTP server —
// the same shape MinIO presents (S3 and STS on one address) — so the
// production signing, listing, and AssumeRole code paths run unmodified.

// fakeStorageBackend is an S3-compatible bucket plus STS in one handler: GET
// with list-type answers ListObjectsV2 over the seeded objects, POST to the
// root answers AssumeRole. It records every policy it was asked to sign so
// tests can prove the credential scope is the organization's own prefix.
type fakeStorageBackend struct {
	mu             sync.Mutex
	objects        map[string]int64
	policies       []string
	authorizations []string
	issued         int
}

func (backend *fakeStorageBackend) seed(key string, size int64) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.objects[key] = size
}

func (backend *fakeStorageBackend) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && request.URL.Path == "/" {
		backend.handleAssumeRole(writer, request)
		return
	}
	if request.Method == http.MethodGet && request.URL.Query().Get("list-type") == "2" {
		backend.handleList(writer, request)
		return
	}
	writer.WriteHeader(http.StatusNotFound)
}

func (backend *fakeStorageBackend) handleAssumeRole(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil || request.Form.Get("Action") != "AssumeRole" {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	backend.mu.Lock()
	backend.policies = append(backend.policies, request.Form.Get("Policy"))
	backend.authorizations = append(backend.authorizations, request.Header.Get("Authorization"))
	backend.issued++
	index := backend.issued
	backend.mu.Unlock()
	writer.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(writer, `<?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>STSACCESS%d</AccessKeyId>
      <SecretAccessKey>sts-secret-%d</SecretAccessKey>
      <SessionToken>sts-token-%d</SessionToken>
      <Expiration>%s</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`, index, index, index, time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
}

func (backend *fakeStorageBackend) handleList(writer http.ResponseWriter, request *http.Request) {
	prefix := request.URL.Query().Get("prefix")
	backend.mu.Lock()
	defer backend.mu.Unlock()
	keys := make([]string, 0, len(backend.objects))
	for key := range backend.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sortStringsInPlace(keys)
	writer.Header().Set("Content-Type", "application/xml")
	writer.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`))
	for _, key := range keys {
		fmt.Fprintf(writer, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`, key, backend.objects[key])
	}
	writer.Write([]byte(`<IsTruncated>false</IsTruncated></ListBucketResult>`))
}

func sortStringsInPlace(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

func (backend *fakeStorageBackend) recordedPolicies() []string {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]string(nil), backend.policies...)
}

// storageCall invokes a storage handler directly with an authenticated
// principal, mirroring checkoutCall: the handler is reached past the
// authentication middleware (membership checks need a database), so the
// configuration-gated paths run without one. The PostgreSQL test covers the
// full routed flow.
func storageCall(t *testing.T, configuration config.ControlPlane, method, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{UserID: "user_1", Role: RoleAdmin}))
	request.SetPathValue("organizationID", "org_1")
	recorder := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/storage/credentials"):
		server.handleStorageCredentials(recorder, request)
	case strings.HasSuffix(path, "/storage/reconcile"):
		server.handleStorageReconcile(recorder, request)
	default:
		server.handleStorageStatus(recorder, request)
	}
	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	return recorder, decoded
}

func hostedStorageConfiguration(endpoint string) config.StorageConfig {
	return config.StorageConfig{
		Enabled:           true,
		Endpoint:          endpoint,
		Region:            "test-1",
		Bucket:            "shift-checkpoints",
		AccessKeyID:       "parent",
		SecretAccessKey:   "parent-secret",
		CredentialTTL:     time.Hour,
		ReconcileInterval: time.Minute,
	}
}

// A control plane that does not host storage answers the agent-facing and
// admin routes with STORAGE_NOT_CONFIGURED — never with a quota that does not
// exist — and the status read simply says hosting is off.
func TestStorageRoutesWithoutHosting(t *testing.T) {
	for _, path := range []string{
		"/v1/organizations/org_1/storage/credentials",
	} {
		recorder, body := storageCall(t, config.DefaultControlPlane(), http.MethodGet, path)
		if recorder.Code != http.StatusServiceUnavailable || body["code"] != "STORAGE_NOT_CONFIGURED" {
			t.Fatalf("GET %s: status %d body %v", path, recorder.Code, body)
		}
	}
	recorder, body := storageCall(t, config.DefaultControlPlane(), http.MethodPost, "/v1/organizations/org_1/storage/reconcile")
	if recorder.Code != http.StatusServiceUnavailable || body["code"] != "STORAGE_NOT_CONFIGURED" {
		t.Fatalf("POST reconcile: status %d body %v", recorder.Code, body)
	}
	recorder, body = storageCall(t, config.DefaultControlPlane(), http.MethodGet, "/v1/organizations/org_1/storage")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status: status %d body %v", recorder.Code, body)
	}
	if enabled, ok := body["enabled"].(bool); !ok || enabled {
		t.Fatalf("status must report hosting disabled: %v", body)
	}
	if body["endpoint"] != nil || body["bucket"] != nil || body["prefix"] != nil {
		t.Fatalf("disabled status must not invent a storage location: %v", body)
	}
}

// The session policy is the security boundary between organizations: it names
// the organization's own prefix and nothing else, and it is deterministic.
func TestStorageSessionPolicyScopesToOrganization(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.Storage = hostedStorageConfiguration("http://storage.test")
	service := &storageService{configuration: configuration.Storage}
	policy := service.sessionPolicy("org_1")
	if policy != service.sessionPolicy("org_1") {
		t.Fatal("session policy is not deterministic across issuances")
	}
	var parsed struct {
		Statement []struct {
			Action    []string `json:"Action"`
			Resource  []string `json:"Resource"`
			Condition struct {
				StringLike map[string][]string `json:"StringLike"`
			} `json:"Condition"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy), &parsed); err != nil {
		t.Fatalf("policy is not valid JSON: %v\n%s", err, policy)
	}
	if !strings.Contains(policy, "arn:aws:s3:::shift-checkpoints/org/org_1/*") {
		t.Fatalf("policy does not scope objects to the organization prefix: %s", policy)
	}
	if strings.Contains(policy, "org/org_2") {
		t.Fatalf("policy names a foreign organization: %s", policy)
	}
	if len(parsed.Statement) != 2 {
		t.Fatalf("policy has %d statements, want object and bucket statements: %s", len(parsed.Statement), policy)
	}
	var sawObjectActions, sawPrefixCondition bool
	for _, statement := range parsed.Statement {
		for _, resource := range statement.Resource {
			if resource == "arn:aws:s3:::shift-checkpoints/org/org_1/*" {
				sawObjectActions = len(statement.Action) > 0
			}
		}
		if statement.Condition.StringLike != nil {
			for _, allowed := range statement.Condition.StringLike["s3:prefix"] {
				if allowed == "org/org_1/" || allowed == "org/org_1/*" {
					sawPrefixCondition = true
				}
			}
		}
	}
	if !sawObjectActions {
		t.Fatalf("policy grants no object actions on the org prefix: %s", policy)
	}
	if !sawPrefixCondition {
		t.Fatalf("bucket listing is not conditioned on the org prefix: %s", policy)
	}
}

// The data-backed flow: status reconciles from the bucket, a machines-scope
// API key receives credentials scoped to its organization, a foreign scope is
// refused, quota blocks issuance fail-closed, and a forced reconcile restores
// usage from the bucket's truth.
func TestHostedStorageFlowWithDatabase(t *testing.T) {
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	backend := &fakeStorageBackend{objects: map[string]int64{}}
	storageServer := httptest.NewServer(backend)
	defer storageServer.Close()

	configuration := config.DefaultControlPlane()
	configuration.DatabaseURL = databaseURL
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	configuration.Storage = hostedStorageConfiguration(storageServer.URL)
	server := New(configuration, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	register := RegisterRequest{Email: "storage-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test", Password: "correct horse battery staple", DisplayName: "Storage User", Organization: "Storage Organization"}
	var registration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	authHeader := "Bearer " + registration.Tokens.AccessToken
	organizationID := registration.Organization.ID
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	// Seed the bucket: the organization's own objects plus a foreign prefix
	// that must never be counted or reachable.
	backend.seed("org/"+organizationID+"/chunks/c1", 100)
	backend.seed("org/"+organizationID+"/checkpoints/cp/manifest", 20)
	backend.seed("org/org_other/chunks/foreign", 999)

	// Status refreshes synchronously from the bucket on first read.
	var status StorageStatus
	getJSON(t, client, organizationPath+"/storage", http.StatusOK, &status, authHeader)
	if !status.Enabled || status.UsedStorageBytes != 120 {
		t.Fatalf("storage status after refresh: %+v", status)
	}
	if status.MaxStorageBytes <= 0 || status.OverQuota {
		t.Fatalf("storage status quota fields: %+v", status)
	}
	if status.Endpoint != storageServer.URL || status.Bucket != "shift-checkpoints" || status.Prefix != "org/"+organizationID+"/" {
		t.Fatalf("storage status location fields: %+v", status)
	}
	if status.LastReconciledAt == nil {
		t.Fatal("storage status carries no reconcile timestamp")
	}

	// A machines-scope API key is what agents carry; it may fetch credentials.
	var key APIKeyCreated
	postJSON(t, client, organizationPath+"/api-keys", CreateAPIKeyRequest{Name: "agent", Scopes: []string{"machines"}}, http.StatusCreated, &key, authHeader)
	machineAuth := "Bearer " + key.Secret

	var credentials StorageCredentials
	getJSON(t, client, organizationPath+"/storage/credentials", http.StatusOK, &credentials, machineAuth)
	if credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" || credentials.SessionToken == "" {
		t.Fatalf("credentials response is incomplete: %+v", credentials)
	}
	if credentials.Prefix != "org/"+organizationID || credentials.Bucket != "shift-checkpoints" || credentials.Region != "test-1" {
		t.Fatalf("credentials location fields: %+v", credentials)
	}
	if !credentials.Expiration.After(time.Now().UTC()) {
		t.Fatalf("credentials already expired: %v", credentials.Expiration)
	}
	if !credentials.ForcePathStyle {
		t.Fatal("hosted credentials must request path-style addressing")
	}
	policies := backend.recordedPolicies()
	if len(policies) != 1 {
		t.Fatalf("expected exactly one AssumeRole call, got %d", len(policies))
	}
	if !strings.Contains(policies[0], "org/"+organizationID+"/") || strings.Contains(policies[0], "org_other") {
		t.Fatalf("issued policy is not scoped to the organization: %s", policies[0])
	}

	// A workloads-scope key has the operator role but not the machines
	// resource family: the credential route refuses it.
	var workloadsKey APIKeyCreated
	postJSON(t, client, organizationPath+"/api-keys", CreateAPIKeyRequest{Name: "ci", Scopes: []string{"workloads"}}, http.StatusCreated, &workloadsKey, authHeader)
	var scopeError ErrorResponse
	getJSON(t, client, organizationPath+"/storage/credentials", http.StatusForbidden, &scopeError, "Bearer "+workloadsKey.Secret)
	if scopeError.Code != "API_KEY_SCOPE_FORBIDDEN" {
		t.Fatalf("wrong-scope key error: %#v", scopeError)
	}

	// Over quota: no credentials, and nothing reaches the STS service.
	if err := store.UpdateStorageUsage(ctx, organizationID, 1<<40); err != nil {
		t.Fatal(err)
	}
	var quotaError map[string]any
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, organizationPath+"/storage/credentials", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", machineAuth)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	_ = json.Unmarshal(body, &quotaError)
	if response.StatusCode != http.StatusForbidden || quotaError["code"] != "STORAGE_QUOTA_EXCEEDED" {
		t.Fatalf("over-quota credentials: status %d body %s", response.StatusCode, body)
	}
	if len(backend.recordedPolicies()) != 1 {
		t.Fatalf("quota did not stop issuance: %d AssumeRole calls", len(backend.recordedPolicies()))
	}

	// The admin-forced reconcile resets usage to the bucket's truth, which
	// unblocks issuance again — the bucket is the only usage authority.
	var reconciled StorageStatus
	postJSON(t, client, organizationPath+"/storage/reconcile", map[string]any{}, http.StatusOK, &reconciled, authHeader)
	if reconciled.UsedStorageBytes != 120 || reconciled.OverQuota {
		t.Fatalf("reconcile did not restore bucket truth: %+v", reconciled)
	}
	getJSON(t, client, organizationPath+"/storage/credentials", http.StatusOK, &credentials, machineAuth)
	if len(backend.recordedPolicies()) != 2 {
		t.Fatalf("issuance did not resume after reconcile: %d AssumeRole calls", len(backend.recordedPolicies()))
	}

	// Every issuance is audited with its principal — and without any secret.
	var auditEvents []AuditEvent
	getJSON(t, client, organizationPath+"/audit", http.StatusOK, &auditEvents, authHeader)
	issued := 0
	for _, event := range auditEvents {
		if event.Action != "storage.credentials_issued" {
			continue
		}
		issued++
		encoded, _ := json.Marshal(event.Metadata)
		if strings.Contains(string(encoded), "sts-secret") || strings.Contains(string(encoded), "sts-token") {
			t.Fatalf("audit record leaked a secret: %s", encoded)
		}
		if event.ResourceType != "organization" || event.ResourceID != organizationID {
			t.Fatalf("audit record names the wrong resource: %+v", event)
		}
	}
	if issued == 0 {
		t.Fatal("credential issuance was never audited")
	}
}
