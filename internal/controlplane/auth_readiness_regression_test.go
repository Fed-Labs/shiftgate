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
)

// auth_readiness_regression_test.go: regression guards for the readiness-fix
// wave's auth/middleware/limiter findings. Tests that need the database skip
// unless SHIFT_TEST_DATABASE_URL is set, matching integration_test.go.

func newAuthRegressionServer(t *testing.T) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set SHIFT_TEST_DATABASE_URL to run the PostgreSQL-backed auth regression tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		t.Fatal(err)
	}
	configuration := config.DefaultControlPlane()
	configuration.DatabaseURL = databaseURL
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	server := New(configuration, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The credential throttle is exercised in its own pure tests; here it must
	// never deny a legitimate registration just because several tests share
	// one test-runner IP inside the same minute.
	server.loginLimiter = NewRateLimiter(1000)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		httpServer.Close()
		store.Close()
	})
	return server, httpServer, httpServer.Client()
}

func uniqueRegressionEmail(t *testing.T) string {
	t.Helper()
	return "auth-regression-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test"
}

func registerRegressionUser(t *testing.T, client *http.Client, endpoint, email string) (Organization, SessionTokens) {
	t.Helper()
	var registration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, client, endpoint+"/v1/auth/register", RegisterRequest{
		Email: email, Password: "correct horse battery staple", DisplayName: "Regression User", Organization: "Regression Organization",
	}, http.StatusCreated, &registration, "")
	if registration.Tokens.AccessToken == "" || registration.Tokens.RefreshToken == "" {
		t.Fatalf("registration returned incomplete tokens: %#v", registration.Tokens)
	}
	return registration.Organization, registration.Tokens
}

// TestLogoutBearerRevokesTheSession: the bearer path of logout (the CLI's) must
// revoke the session server-side — before the fix the bare route never
// authenticated, so bearer-only logout answered 400 and revoked nothing.
func TestLogoutBearerRevokesTheSession(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	_, tokens := registerRegressionUser(t, client, httpServer.URL, uniqueRegressionEmail(t))
	authHeader := "Bearer " + tokens.AccessToken

	postJSON(t, client, httpServer.URL+"/v1/auth/logout", nil, http.StatusNoContent, nil, authHeader)

	var me ErrorResponse
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusUnauthorized, &me, authHeader)

	var refreshed struct {
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/refresh", RefreshRequest{RefreshToken: tokens.RefreshToken}, http.StatusUnauthorized, &refreshed, "")
}

// TestLogoutCookieRevokesWithoutBearer: the cookie path keeps working for
// browsers that carry no bearer at all.
func TestLogoutCookieRevokesWithoutBearer(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	_, tokens := registerRegressionUser(t, client, httpServer.URL, uniqueRegressionEmail(t))

	postJSONWithHeaders(t, client, httpServer.URL+"/v1/auth/logout", nil, http.StatusNoContent, nil, "", map[string]string{
		"Cookie": refreshCookieName + "=" + tokens.RefreshToken,
	})

	var refreshed struct {
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/refresh", RefreshRequest{RefreshToken: tokens.RefreshToken}, http.StatusUnauthorized, &refreshed, "")
}

// TestLogoutBadBearerStillRevokesViaCookie: an expired or invalid bearer must
// not block the browser cookie logout path (the whole reason logout is
// registered without requireAuth).
func TestLogoutBadBearerStillRevokesViaCookie(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	_, tokens := registerRegressionUser(t, client, httpServer.URL, uniqueRegressionEmail(t))

	postJSONWithHeaders(t, client, httpServer.URL+"/v1/auth/logout", nil, http.StatusNoContent, nil,
		"Bearer shift_at_"+strings.Repeat("z", 43), map[string]string{
			"Cookie": refreshCookieName + "=" + tokens.RefreshToken,
		})

	var refreshed struct {
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/refresh", RefreshRequest{RefreshToken: tokens.RefreshToken}, http.StatusUnauthorized, &refreshed, "")
}

// TestLogoutWithoutAnyCredentialIsRejected: nothing presented, nothing
// revoked — 400 with the stable code. Pure handler test, no database.
func TestLogoutWithoutAnyCredentialIsRejected(t *testing.T) {
	server := &Server{config: config.DefaultControlPlane()}
	recorder := httptest.NewRecorder()
	server.handleLogout(recorder, httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	var body ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "SESSION_TOKEN_REQUIRED" {
		t.Fatalf("code = %q, want SESSION_TOKEN_REQUIRED", body.Code)
	}
}

// TestLogoutBadBearerWithoutCookieIsUnauthorized: a presented credential that
// resolves to nothing actionable answers 401 (the status clients use to clear
// stored tokens), never the old 400 that left them in place.
func TestLogoutBadBearerWithoutCookieIsUnauthorized(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	var body ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/auth/logout", nil, http.StatusUnauthorized, &body,
		"Bearer shift_at_"+strings.Repeat("z", 43))
	if body.Code != "AUTHENTICATION_REQUIRED" {
		t.Fatalf("code = %q, want AUTHENTICATION_REQUIRED", body.Code)
	}
}

// TestRefreshTokenReuseRevokesTheSession: replaying a rotated-out refresh
// token is rejected like an invalid token and kills the whole session
// (migration 012 semantics), so neither party keeps an authenticated chain.
func TestRefreshTokenReuseRevokesTheSession(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	_, tokens := registerRegressionUser(t, client, httpServer.URL, uniqueRegressionEmail(t))

	var rotated struct {
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/refresh", RefreshRequest{RefreshToken: tokens.RefreshToken}, http.StatusOK, &rotated, "")

	var replay ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/auth/refresh", RefreshRequest{RefreshToken: tokens.RefreshToken}, http.StatusUnauthorized, &replay, "")
	if replay.Code != "INVALID_REFRESH_TOKEN" {
		t.Fatalf("replay code = %q, want INVALID_REFRESH_TOKEN (no reuse oracle)", replay.Code)
	}

	var afterReuse ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/auth/refresh", RefreshRequest{RefreshToken: rotated.Tokens.RefreshToken}, http.StatusUnauthorized, &afterReuse, "")
}

// TestAuthRegisterConflictSuggestsLogin: the duplicate-email conflict must
// point at the recovery (log in), especially after the account was committed
// but session issuance failed on the first attempt.
func TestAuthRegisterConflictSuggestsLogin(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	email := uniqueRegressionEmail(t)
	registerRegressionUser(t, client, httpServer.URL, email)

	var conflict ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/auth/register", RegisterRequest{
		Email: email, Password: "correct horse battery staple", DisplayName: "Regression User", Organization: "Regression Organization",
	}, http.StatusConflict, &conflict, "")
	if conflict.Code != "EMAIL_EXISTS" {
		t.Fatalf("code = %q, want EMAIL_EXISTS", conflict.Code)
	}
	if !strings.Contains(conflict.Message, "log in") {
		t.Fatalf("conflict message %q must suggest logging in", conflict.Message)
	}
}

// TestAuthRegistrationThrottleIsPerClientIP: account creation is throttled per
// derived client IP — never per submitted email, which would let one client
// mint one throttle bucket per address and flood registrations. Pure test.
func TestAuthRegistrationThrottleIsPerClientIP(t *testing.T) {
	server := &Server{config: config.DefaultControlPlane(), loginLimiter: NewRateLimiter(3)}
	attacker := httptest.NewRequest(http.MethodPost, "/v1/auth/register", nil)
	attacker.RemoteAddr = "203.0.113.9:4444"
	for attempt := 0; attempt < 3; attempt++ {
		if !server.allowRegistrationAttempt(attacker) {
			t.Fatalf("attempt %d from one IP was denied too early", attempt+1)
		}
	}
	if server.allowRegistrationAttempt(attacker) {
		t.Fatal("a fourth registration inside the window from one IP must be throttled")
	}
	elsewhere := httptest.NewRequest(http.MethodPost, "/v1/auth/register", nil)
	elsewhere.RemoteAddr = "203.0.113.10:4444"
	if !server.allowRegistrationAttempt(elsewhere) {
		t.Fatal("a different client IP must keep its own registration budget")
	}
}

// TestLimiterCredentialBucketsRequireValidation: the global request limiter
// grants a per-credential bucket only after that credential authenticated.
// Rotating forged Authorization headers must always share the per-client-IP
// bucket instead of minting one bucket per forged value. Pure test.
func TestLimiterCredentialBucketsRequireValidation(t *testing.T) {
	limiter := NewRateLimiter(3)
	limiter.window = time.Hour
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }

	for attempt := 0; attempt < 3; attempt++ {
		if !limiter.Allow(fmt.Sprintf("ip:203.0.113.9|token:%08x", attempt)) {
			t.Fatalf("unvalidated credential %d was denied before the per-IP budget was spent", attempt+1)
		}
	}
	if limiter.Allow("ip:203.0.113.9|token:ffff") {
		t.Fatal("a forged credential minted a fresh bucket and bypassed the per-IP limit")
	}
	if !limiter.Allow("ip:203.0.113.10|token:0000000a") {
		t.Fatal("a different client IP must keep its own budget")
	}

	limiter.MarkValidated("0000000b")
	if !limiter.isValidated("0000000b") {
		t.Fatal("a credential that just authenticated must be marked validated")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if !limiter.Allow("ip:203.0.113.9|token:0000000b") {
			t.Fatalf("validated credential attempt %d was denied although the IP bucket is exhausted", attempt+1)
		}
	}
	if limiter.Allow("ip:203.0.113.9|token:0000000b") {
		t.Fatal("a validated credential must still respect its own bucket")
	}

	now = now.Add(validatedTTL + time.Minute)
	if limiter.isValidated("0000000b") {
		t.Fatal("a validation mark must expire with its TTL")
	}
	if limiter.Allow("ip:203.0.113.9|token:0000000b") {
		t.Fatal("an expired validation mark must fall back to the per-IP bucket")
	}
}

// TestMiddlewarePlatformAdminGate: platform-admin surfaces accept an admin
// scope API key or the configured metrics scrape token — never a session
// principal, whatever its organization role. Pure test of the gate predicate.
func TestMiddlewarePlatformAdminGate(t *testing.T) {
	configuration := config.DefaultControlPlane()
	server := &Server{config: configuration}
	request := httptest.NewRequest(http.MethodGet, "/v1/admin/scoreboard", nil)

	adminKey := Principal{APIKeyID: "key-1", UserID: "user-1", Scopes: []string{"admin"}}
	if !server.platformAdmin(request, adminKey) {
		t.Fatal("an API key with the admin scope must pass the platform-admin gate")
	}
	machinesKey := Principal{APIKeyID: "key-2", UserID: "user-1", Scopes: []string{"machines"}}
	if server.platformAdmin(request, machinesKey) {
		t.Fatal("a resource-scoped API key must not pass the platform-admin gate")
	}
	session := Principal{SessionID: "session-1", UserID: "user-1"}
	if server.platformAdmin(request, session) {
		t.Fatal("a session principal must not pass the platform-admin gate")
	}
	emptyKey := Principal{APIKeyID: "key-3", UserID: "user-1"}
	if server.platformAdmin(request, emptyKey) {
		t.Fatal("an API key without the admin scope must not pass the platform-admin gate")
	}

	spoofed := httptest.NewRequest(http.MethodGet, "/v1/admin/scoreboard", nil)
	spoofed.Header.Set("X-Metrics-Token", "guess")
	if server.platformAdmin(spoofed, session) {
		t.Fatal("with no metrics token configured, a presented secret must not match")
	}

	server.config.MetricsToken = strings.Repeat("metrics-secret-", 2)
	bearer := httptest.NewRequest(http.MethodGet, "/v1/admin/scoreboard", nil)
	bearer.Header.Set("Authorization", "Bearer "+server.config.MetricsToken)
	if !server.platformAdmin(bearer, session) {
		t.Fatal("the configured metrics scrape token must pass the platform-admin gate")
	}
	header := httptest.NewRequest(http.MethodGet, "/v1/admin/scoreboard", nil)
	header.Header.Set("X-Metrics-Token", server.config.MetricsToken)
	if !server.platformAdmin(header, session) {
		t.Fatal("the configured metrics scrape token must pass via X-Metrics-Token")
	}
	wrong := httptest.NewRequest(http.MethodGet, "/v1/admin/scoreboard", nil)
	wrong.Header.Set("X-Metrics-Token", strings.Repeat("x", 32))
	if server.platformAdmin(wrong, session) {
		t.Fatal("a wrong metrics token must not pass the platform-admin gate")
	}

	if !isPlatformAdminPath("/v1/admin/scoreboard") || isPlatformAdminPath("/v1/organizations") {
		t.Fatal("platform-admin path matching must cover /v1/admin/ and nothing else")
	}
}

// TestMiddlewareAdminScoreboardRequiresAdminScope: the MRR/subscription
// scoreboard is platform-operator material. Any authenticated tenant session
// (here: the owner of a fresh organization) must get 403, and only an admin
// scope API key may read it. This is the leak regression.
func TestMiddlewareAdminScoreboardRequiresAdminScope(t *testing.T) {
	_, httpServer, client := newAuthRegressionServer(t)
	organization, tokens := registerRegressionUser(t, client, httpServer.URL, uniqueRegressionEmail(t))
	authHeader := "Bearer " + tokens.AccessToken

	var denied ErrorResponse
	getJSON(t, client, httpServer.URL+"/v1/admin/scoreboard", http.StatusForbidden, &denied, authHeader)
	if denied.Code != "ADMIN_SCOPE_REQUIRED" {
		t.Fatalf("code = %q, want ADMIN_SCOPE_REQUIRED", denied.Code)
	}

	var created APIKeyCreated
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+organization.ID+"/api-keys",
		CreateAPIKeyRequest{Name: "scoreboard-reader", Scopes: []string{"admin"}}, http.StatusCreated, &created, authHeader)
	if created.Secret == "" {
		t.Fatal("api key creation must return the secret once")
	}

	var counters map[string]any
	getJSON(t, client, httpServer.URL+"/v1/admin/scoreboard", http.StatusOK, &counters, "Bearer "+created.Secret)
	if len(counters) == 0 {
		t.Fatal("an admin scope key must receive the scoreboard payload")
	}

	var scopedDenied ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+organization.ID+"/api-keys",
		CreateAPIKeyRequest{Name: "machines-only", Scopes: []string{"machines"}}, http.StatusCreated, &created, authHeader)
	getJSON(t, client, httpServer.URL+"/v1/admin/scoreboard", http.StatusForbidden, &scopedDenied, "Bearer "+created.Secret)
	if scopedDenied.Code != "ADMIN_SCOPE_REQUIRED" {
		t.Fatalf("machines-scope key code = %q, want ADMIN_SCOPE_REQUIRED", scopedDenied.Code)
	}
}

// TestMiddlewareBudgetFallbackSetsRetryAfter: the entitlement-error fallback
// 429 must carry Retry-After like the plan-based 429, so budget-throttled
// clients get one consistent retry signal.
func TestMiddlewareBudgetFallbackSetsRetryAfter(t *testing.T) {
	server, _, _ := newAuthRegressionServer(t)
	organizationID := "org-missing-" + time.Now().UTC().Format("20060102150405.000000000")
	for attempt := 0; attempt < 1000; attempt++ {
		server.apiBudgets.Allow(organizationID, 1000, 600)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/organizations/x/usage", nil)
	if server.consumeAPIBudget(recorder, request, organizationID) {
		t.Fatal("an exhausted free-tier fallback budget must refuse the request")
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if recorder.Header().Get("Retry-After") == "" {
		t.Fatal("the fallback budget 429 must set Retry-After like the plan-based one")
	}
}

// TestMiddlewareRequestIDReachesErrorBodies: error envelopes carry the
// correlation id the observability middleware stamped on the response, so
// dashboard error surfaces can show clients the id the logs carry.
func TestMiddlewareRequestIDReachesErrorBodies(t *testing.T) {
	recorder := httptest.NewRecorder()
	recorder.Header().Set("X-Request-ID", "req-correlation-1")
	writeError(recorder, http.StatusBadRequest, "INVALID_JSON", "bad request")
	var body ErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestID != "req-correlation-1" {
		t.Fatalf("request_id = %q, want the stamped correlation id", body.RequestID)
	}

	plain := httptest.NewRecorder()
	writeError(plain, http.StatusBadRequest, "INVALID_JSON", "bad request")
	if strings.Contains(plain.Body.String(), "request_id") {
		t.Fatalf("without a correlation id the field must stay omitted, got %s", plain.Body.String())
	}
}
