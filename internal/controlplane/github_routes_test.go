package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// github_routes_test.go: the "Sign in with GitHub" contract. Pure tests pin
// the state binding and the fail-closed 503; the PostgreSQL-gated ones walk
// the full redirect chain against a local GitHub double and pin the
// sign-in / sign-up / link semantics, the one-time handoff rule, the typed
// callback errors, and the organization gate.

// fakeGitHub is a GitHub OAuth + API double. It answers the token exchange,
// the profile and email reads, and the organization listing, with per-test
// knobs for the failure modes the flow must type honestly.
type fakeGitHub struct {
	server *httptest.Server
	mutex  sync.Mutex
	forms  []url.Values

	failExchange bool
	profile      fakeGitHubProfile
	emails       []fakeGitHubEmail
	orgs         []string
	orgsFail     bool
}

type fakeGitHubProfile struct {
	ID    int64
	Login string
	Name  string
}

type fakeGitHubEmail struct {
	Email    string
	Primary  bool
	Verified bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	fake := &fakeGitHub{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fake.mutex.Lock()
		defer fake.mutex.Unlock()
		switch {
		case request.URL.Path == "/login/oauth/access_token":
			body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
			form, _ := url.ParseQuery(string(body))
			fake.forms = append(fake.forms, form)
			writer.Header().Set("Content-Type", "application/json")
			if fake.failExchange {
				_, _ = writer.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect"}`))
				return
			}
			_, _ = writer.Write([]byte(`{"access_token":"gho_test_token","token_type":"bearer","scope":"read:user,user:email"}`))
		case request.URL.Path == "/user":
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": fake.profile.ID, "login": fake.profile.Login, "name": fake.profile.Name})
		case request.URL.Path == "/user/emails":
			addresses := make([]map[string]any, 0, len(fake.emails))
			for _, address := range fake.emails {
				addresses = append(addresses, map[string]any{"email": address.Email, "primary": address.Primary, "verified": address.Verified})
			}
			_ = json.NewEncoder(writer).Encode(addresses)
		case request.URL.Path == "/user/orgs":
			if fake.orgsFail {
				writer.WriteHeader(http.StatusBadGateway)
				return
			}
			listed := make([]map[string]any, 0, len(fake.orgs))
			for _, organization := range fake.orgs {
				listed = append(listed, map[string]any{"login": organization})
			}
			_ = json.NewEncoder(writer).Encode(listed)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

// installFakeGitHub points the flow's GitHub endpoints at the double and
// restores the production values afterwards.
func installFakeGitHub(t *testing.T, fake *fakeGitHub) {
	t.Helper()
	previousAuthorize, previousToken, previousAPI, previousClient := githubAuthorizeEndpoint, githubTokenEndpoint, githubAPIEndpoint, githubHTTPClient
	githubAuthorizeEndpoint = fake.server.URL + "/login/oauth/authorize"
	githubTokenEndpoint = fake.server.URL + "/login/oauth/access_token"
	githubAPIEndpoint = fake.server.URL
	githubHTTPClient = fake.server.Client()
	t.Cleanup(func() {
		githubAuthorizeEndpoint, githubTokenEndpoint, githubAPIEndpoint, githubHTTPClient = previousAuthorize, previousToken, previousAPI, previousClient
	})
}

// githubFixture is one control plane under test: the live server, the store
// behind it (for direct assertions and row surgery), and the GitHub double.
type githubFixture struct {
	server     *Server
	httpServer *httptest.Server
	store      *database.Store
	fake       *fakeGitHub
}

// openGitHubTestServer boots a control plane configured for GitHub sign-in.
// The shared-database note from the SSO fixture applies: every email this
// file uses carries a per-instance domain so no run can trip an SSO claim a
// previous run left behind.
func openGitHubTestServer(t *testing.T, githubConfig config.GitHub) githubFixture {
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
	configuration.GitHub = githubConfig
	server := New(configuration, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	fake := newFakeGitHub(t)
	installFakeGitHub(t, fake)
	return githubFixture{server: server, httpServer: httpServer, store: store, fake: fake}
}

// githubRunID returns a run-unique suffix for emails and logins (shared
// database).
func githubRunID(t *testing.T) string {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// githubNumericID mints a run-unique numeric GitHub identity. The control
// plane tests share one database across runs, and github_id is unique when
// present, so every test must draw fresh ids (the SSO fixtures' per-instance
// email rule for the numeric column).
func githubNumericID(t *testing.T) int64 {
	t.Helper()
	var buffer [8]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		t.Fatal(err)
	}
	return int64(binary.BigEndian.Uint64(buffer[:])%9_000_000_000) + 1_000_000_000
}

// githubJSONString encodes a Go string as a JSON string literal, for building
// request bodies around awkward test inputs.
func githubJSONString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// githubAuthorize mints the flow the way the web dashboard does and returns
// the parsed GitHub URL plus the state.
func githubAuthorize(t *testing.T, fixture githubFixture, redirectURI string) (authorizeURL *url.URL, state string) {
	t.Helper()
	var response GitHubAuthorizeResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/authorize",
		map[string]string{"redirect_uri": redirectURI}, http.StatusOK, &response, "")
	authorizeURL, err := url.Parse(response.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize_url %q does not parse: %v", response.AuthorizeURL, err)
	}
	if response.State == "" {
		t.Fatal("authorize response carried no state")
	}
	return authorizeURL, response.State
}

// githubCallback performs the browser leg — GET the callback as the redirect
// from GitHub — without following the redirect, so the test can inspect the
// bound target. Returns the response and the parsed Location.
func githubCallback(t *testing.T, fixture githubFixture, query url.Values) (*http.Response, *url.URL) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(fixture.httpServer.URL + "/v1/auth/github/callback?" + query.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("callback status %d, want 302: %s", response.StatusCode, content)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("callback Location %q does not parse: %v", response.Header.Get("Location"), err)
	}
	return response, location
}

// githubRedeem exchanges the one-time handoff code for a session.
func githubRedeem(t *testing.T, fixture githubFixture, code string, expectedStatus int) (User, SessionTokens) {
	t.Helper()
	var body struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": code}, expectedStatus, &body, "")
	return body.User, body.Tokens
}

// githubWalk runs the full browser round trip and returns the redeemed
// session: authorize, callback, redeem.
func githubWalk(t *testing.T, fixture githubFixture, redirectURI string) (User, SessionTokens) {
	t.Helper()
	_, state := githubAuthorize(t, fixture, redirectURI)
	query := url.Values{}
	query.Set("code", "oauth-code-"+githubRunID(t))
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	code := location.Query().Get("github_code")
	if code == "" {
		t.Fatalf("callback Location %q carried no github_code", location)
	}
	user, tokens := githubRedeem(t, fixture, code, http.StatusOK)
	return user, tokens
}

// --- pure tests (no database) ---

// TestGitHubStateBindsRedirectURIAndNonce: the signed state round-trips the
// caller's redirect target and refuses anything that is not byte-exactly the
// blob this control plane minted — wrong pepper, edited payload, missing
// nonce, and expiry all fail.
func TestGitHubStateBindsRedirectURIAndNonce(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.TokenPepper = strings.Repeat("token", 12)
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	redirectURI := "https://web.example.test/login?tab=sso"
	state, err := server.mintGitHubState(redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	got, err := server.verifyGitHubState(state)
	if err != nil {
		t.Fatalf("freshly minted state must verify: %v", err)
	}
	if got != redirectURI {
		t.Fatalf("state bound %q, want %q", got, redirectURI)
	}
	// A state minted with another pepper must not verify here.
	otherConfiguration := config.DefaultControlPlane()
	otherConfiguration.TokenPepper = strings.Repeat("pepper", 8)
	other := New(otherConfiguration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	foreign, err := other.mintGitHubState(redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.verifyGitHubState(foreign); err == nil {
		t.Fatal("a state signed with another pepper must be refused")
	}
	// Tampering with the payload breaks the signature.
	encoded, signature, _ := strings.Cut(state, ".")
	if _, err := server.verifyGitHubState(encoded + "x." + signature); err == nil {
		t.Fatal("an edited state payload must be refused")
	}
	if _, err := server.verifyGitHubState(encoded + "." + "forged"); err == nil {
		t.Fatal("a forged state signature must be refused")
	}
	if _, err := server.verifyGitHubState(""); err == nil {
		t.Fatal("an empty state must be refused")
	}
	sign := func(blob []byte) string {
		encoded := base64.RawURLEncoding.EncodeToString(blob)
		return encoded + "." + base64.RawURLEncoding.EncodeToString(hmacSHA256([]byte(configuration.TokenPepper), []byte(encoded)))
	}
	// An expired state is refused even though its signature is valid.
	expired, err := json.Marshal(githubState{RedirectURI: redirectURI, Nonce: "nonce", Expiry: time.Now().UTC().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.verifyGitHubState(sign(expired)); err == nil {
		t.Fatal("an expired state must be refused")
	}
	// A state without its nonce is malformed, signature or not.
	nonceless, err := json.Marshal(githubState{RedirectURI: redirectURI, Expiry: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.verifyGitHubState(sign(nonceless)); err == nil {
		t.Fatal("a state without its nonce must be refused")
	}
}

// TestGitHubAuthorizeValidatesRedirectURI: only absolute http(s) URLs are
// accepted — the same rule the billing portal's return_url obeys — and the
// authorize URL targets GitHub with the pinned scope and the control plane's
// own callback, never the caller's URL.
func TestGitHubAuthorizeValidatesRedirectURI(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.GitHub = config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"}
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, bad := range []string{"", "/login", "javascript:alert(1)", "ftp://example.test/login", "https:///login", "not a url at all"} {
		recorder := httptest.NewRecorder()
		server.handleGitHubAuthorize(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/github/authorize",
			strings.NewReader(`{"redirect_uri":`+githubJSONString(t, bad)+`}`)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("redirect_uri %q must be rejected with 400, got %d: %s", bad, recorder.Code, recorder.Body.String())
		}
		var failure ErrorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatal(err)
		}
		if failure.Code != "REDIRECT_INVALID" {
			t.Fatalf("redirect_uri %q must fail with REDIRECT_INVALID, got %q", bad, failure.Code)
		}
	}
	recorder := httptest.NewRecorder()
	server.handleGitHubAuthorize(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/github/authorize",
		strings.NewReader(`{"redirect_uri":"https://web.example.test/login"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a valid redirect_uri must be accepted, got %d: %s", recorder.Code, recorder.Body.String())
	}
	var response GitHubAuthorizeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	authorizeURL, err := url.Parse(response.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	if authorizeURL.Scheme != "https" || authorizeURL.Host != "github.com" || authorizeURL.Path != "/login/oauth/authorize" {
		t.Fatalf("authorize_url must target github.com/login/oauth/authorize, got %q", response.AuthorizeURL)
	}
	query := authorizeURL.Query()
	if query.Get("client_id") != "client-id" {
		t.Fatalf("authorize_url client_id = %q", query.Get("client_id"))
	}
	if query.Get("scope") != "read:user user:email" {
		t.Fatalf("authorize_url scope = %q", query.Get("scope"))
	}
	if query.Get("redirect_uri") != "http://example.com/v1/auth/github/callback" {
		// httptest.NewRequest sets Host: example.com and no TLS.
		t.Fatalf("authorize_url redirect_uri = %q, want the control callback", query.Get("redirect_uri"))
	}
	if query.Get("state") != response.State {
		t.Fatal("authorize_url state must equal the response state")
	}
	bound, err := server.verifyGitHubState(response.State)
	if err != nil {
		t.Fatal(err)
	}
	if bound != "https://web.example.test/login" {
		t.Fatalf("state bound %q, want the caller's redirect_uri", bound)
	}
}

// TestGitHubNotConfiguredFailsClosed: without the OAuth credentials every
// endpoint answers the typed 503 the contract documents — clean, like
// BILLING_NOT_CONFIGURED — and nothing leaks a stack or a generic 500.
func TestGitHubNotConfiguredFailsClosed(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.TokenPepper = strings.Repeat("token", 12)
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	probes := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		target  string
		body    string
	}{
		{"authorize", server.handleGitHubAuthorize, http.MethodPost, "/v1/auth/github/authorize", `{"redirect_uri":"https://web.example.test/login"}`},
		{"callback", server.handleGitHubCallback, http.MethodGet, "/v1/auth/github/callback?code=x&state=y", ""},
		{"token", server.handleGitHubToken, http.MethodPost, "/v1/auth/github/token", `{"code":"shift_ghc_x"}`},
	}
	for _, probe := range probes {
		recorder := httptest.NewRecorder()
		probe.handler(recorder, httptest.NewRequestWithContext(context.Background(), probe.method, probe.target, strings.NewReader(probe.body)))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s must answer 503, got %d: %s", probe.name, recorder.Code, recorder.Body.String())
		}
		var failure ErrorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatalf("%s: %v", probe.name, err)
		}
		if failure.Code != "GITHUB_NOT_CONFIGURED" {
			t.Fatalf("%s must answer GITHUB_NOT_CONFIGURED, got %q", probe.name, failure.Code)
		}
	}
}

// --- PostgreSQL-gated integration tests ---

// TestGitHubFlowSignUpSignInAndLink pins the three-way resolution: a new
// verified email creates the account and its initial organization exactly
// like register, the same GitHub identity signs back in without minting a
// second user or org, and a verified email that matches an existing unlinked
// account attaches the identity and signs that account in.
func TestGitHubFlowSignUpSignInAndLink(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"})
	client := fixture.httpServer.Client()
	redirectURI := fixture.httpServer.URL + "/landing"

	// Sign up: unknown verified email → new user + initial organization.
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "octo-" + runID, Name: "Octo " + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: "octo-" + runID + "@gh-" + runID + ".test", Primary: true, Verified: true}}

	// The authorize leg pins the contract fields on the GitHub URL.
	authorizeURL, state := githubAuthorize(t, fixture, redirectURI)
	query := authorizeURL.Query()
	if query.Get("scope") != "read:user user:email" || query.Get("client_id") != "client-id" {
		t.Fatalf("unexpected authorize URL query: %v", query)
	}
	if query.Get("redirect_uri") != fixture.httpServer.URL+"/v1/auth/github/callback" {
		t.Fatalf("authorize redirect_uri = %q, want the control callback", query.Get("redirect_uri"))
	}

	// The callback leg resolves the identity and hands off a one-time code.
	callbackQuery := url.Values{}
	callbackQuery.Set("code", "oauth-code-"+runID)
	callbackQuery.Set("state", state)
	_, location := githubCallback(t, fixture, callbackQuery)
	bound, err := url.Parse(redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	if location.Scheme != bound.Scheme || location.Host != bound.Host || location.Path != bound.Path {
		t.Fatalf("callback landed on %q, want the bound %q", location, redirectURI)
	}
	handoff := location.Query().Get("github_code")
	if handoff == "" {
		t.Fatal("callback redirect carried no github_code")
	}
	if strings.Contains(location.String(), "gho_") {
		t.Fatalf("callback redirect must never carry GitHub tokens: %q", location)
	}

	signUpUser, signUpTokens := githubRedeem(t, fixture, handoff, http.StatusOK)
	if signUpUser.Email != "octo-"+runID+"@gh-"+runID+".test" {
		t.Fatalf("sign-up user email = %q", signUpUser.Email)
	}
	if signUpTokens.AccessToken == "" {
		t.Fatal("sign-up returned no access token")
	}
	// The fresh account owns exactly one organization with the owner role —
	// registration's initial-org contract.
	var organizations []Organization
	getJSON(t, client, fixture.httpServer.URL+"/v1/organizations", http.StatusOK, &organizations, "Bearer "+signUpTokens.AccessToken)
	if len(organizations) != 1 || organizations[0].Role != RoleOwner {
		t.Fatalf("github sign-up organizations = %#v, want one owner org", organizations)
	}

	// Sign in: the same GitHub identity comes back → same user, no new org.
	signInUser, _ := githubWalk(t, fixture, redirectURI)
	if signInUser.ID != signUpUser.ID {
		t.Fatalf("sign-in resolved user %s, want the sign-up user %s", signInUser.ID, signUpUser.ID)
	}

	// Link: an existing password account's verified email attaches the
	// GitHub identity and signs that account in — no new user, no new org.
	register := RegisterRequest{Email: "link-" + runID + "@org-" + runID + ".test", Password: "correct horse battery staple", DisplayName: "Link User", Organization: "Link Organization"}
	var registration struct {
		User User `json:"user"`
	}
	postJSON(t, client, fixture.httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	fixture.fake.profile = fakeGitHubProfile{ID: base + 1, Login: "linker-" + runID, Name: "Linker"}
	fixture.fake.emails = []fakeGitHubEmail{{Email: register.Email, Primary: true, Verified: true}}
	linkUser, linkTokens := githubWalk(t, fixture, redirectURI)
	if linkUser.ID != registration.User.ID {
		t.Fatalf("link resolved user %s, want the registered user %s", linkUser.ID, registration.User.ID)
	}
	if linkTokens.AccessToken == "" {
		t.Fatal("link returned no access token")
	}
}

// TestGitHubCodeReplayRejected: the handoff code is single use and short
// lived. Replayed, expired, and never-issued codes are refused with distinct
// 401-class typed errors — a leaked redirect URL cannot mint a second
// session.
func TestGitHubCodeReplayRejected(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"})
	redirectURI := fixture.httpServer.URL + "/landing"
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "replay-" + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: "replay-" + runID + "@gh-" + runID + ".test", Primary: true, Verified: true}}

	_, state := githubAuthorize(t, fixture, redirectURI)
	query := url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	handoff := location.Query().Get("github_code")

	githubRedeem(t, fixture, handoff, http.StatusOK)

	var replay ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": handoff}, http.StatusUnauthorized, &replay, "")
	if replay.Code != "GITHUB_CODE_REPLAYED" {
		t.Fatalf("replayed code must answer GITHUB_CODE_REPLAYED, got %q", replay.Code)
	}

	var unknown ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": "shift_ghc_never_issued"}, http.StatusUnauthorized, &unknown, "")
	if unknown.Code != "GITHUB_CODE_INVALID" {
		t.Fatalf("unknown code must answer GITHUB_CODE_INVALID, got %q", unknown.Code)
	}

	// Expired: an unconsumed code past its TTL is refused as expired.
	expiredDigest := tokenDigest(fixture.server.config.TokenPepper, "shift_ghc_expired_"+runID)
	record := database.GitHubAuthCodeRecord{GitHubID: base + 1, Login: "stale-" + runID, Email: "stale-" + runID + "@gh-" + runID + ".test"}
	if err := fixture.store.CreateGitHubAuthCode(context.Background(), expiredDigest, record, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var expired ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": "shift_ghc_expired_" + runID}, http.StatusUnauthorized, &expired, "")
	if expired.Code != "GITHUB_CODE_EXPIRED" {
		t.Fatalf("expired code must answer GITHUB_CODE_EXPIRED, got %q", expired.Code)
	}
}

// TestGitHubCallbackTypedErrors pins the honest failure redirects: every
// failure after a valid state lands on the bound redirect_uri with a
// github_-prefixed error code (the web UI keys its copy off that prefix),
// while an unverifiable state answers in place with 400 and no redirect —
// the anti-open-redirect rule.
func TestGitHubCallbackTypedErrors(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"})
	redirectURI := fixture.httpServer.URL + "/landing"
	_, state := githubAuthorize(t, fixture, redirectURI)

	// A forged state must not redirect anywhere.
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	forged, err := client.Get(fixture.httpServer.URL + "/v1/auth/github/callback?code=x&state=forged.state")
	if err != nil {
		t.Fatal(err)
	}
	defer forged.Body.Close()
	if forged.StatusCode != http.StatusBadRequest || forged.Header.Get("Location") != "" {
		t.Fatalf("forged state must answer 400 in place, got %d Location=%q", forged.StatusCode, forged.Header.Get("Location"))
	}
	var failure ErrorResponse
	if err := json.NewDecoder(forged.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "GITHUB_STATE_INVALID" {
		t.Fatalf("forged state must answer GITHUB_STATE_INVALID, got %q", failure.Code)
	}

	// The user denied the grant at GitHub.
	query := url.Values{}
	query.Set("error", "access_denied")
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	if location.Query().Get("error") != "github_access_denied" {
		t.Fatalf("user denial must redirect with github_access_denied, got %q", location)
	}

	// The token exchange fails (bad code, GitHub down, ...).
	fixture.fake.failExchange = true
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	if location.Query().Get("error") != "github_auth_failed" {
		t.Fatalf("exchange failure must redirect with github_auth_failed, got %q", location)
	}
	fixture.fake.failExchange = false

	// No primary verified email.
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "unverified-" + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: "unverified-" + runID + "@gh-" + runID + ".test", Primary: true, Verified: false}}
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	if location.Query().Get("error") != "github_email_not_verified" {
		t.Fatalf("unverified email must redirect with github_email_not_verified, got %q", location)
	}

	// A missing code with a valid state is a plain failure.
	query = url.Values{}
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	if location.Query().Get("error") != "github_auth_failed" {
		t.Fatalf("missing code must redirect with github_auth_failed, got %q", location)
	}
}

// TestGitHubOrganizationGating: with an allowlist configured, only members of
// the listed GitHub organizations may authenticate; everyone else is turned
// away at the callback with github_org_denied. The allowlist is
// case-insensitive and the gate fails closed when the org lookup itself
// breaks.
func TestGitHubOrganizationGating(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{
		ClientID:             "client-id",
		ClientSecret:         "client-secret",
		AllowedOrganizations: []string{"Acme-Corp", "shift-internal"},
	})
	redirectURI := fixture.httpServer.URL + "/landing"
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "gated-" + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: "gated-" + runID + "@gh-" + runID + ".test", Primary: true, Verified: true}}

	// A member of a listed organization (different case) proceeds.
	fixture.fake.orgs = []string{"some-other-org", "acme-corp"}
	_, state := githubAuthorize(t, fixture, redirectURI)
	query := url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	if location.Query().Get("github_code") == "" {
		t.Fatalf("an allowlisted member must receive a github_code, got %q", location)
	}

	// A non-member is denied with the typed redirect error.
	fixture.fake.orgs = []string{"some-other-org"}
	_, state = githubAuthorize(t, fixture, redirectURI)
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	if location.Query().Get("error") != "github_org_denied" {
		t.Fatalf("a non-member must redirect with github_org_denied, got %q", location)
	}

	// An unreadable org listing fails closed: denied, never admitted.
	fixture.fake.orgsFail = true
	_, state = githubAuthorize(t, fixture, redirectURI)
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	if location.Query().Get("error") != "github_auth_failed" {
		t.Fatalf("a broken org lookup must fail closed, got %q", location)
	}
}

// TestGitHubTokenTypedRefusals pins the redeem-side refusals: an account
// whose stored email no longer matches the verified GitHub email is refused
// (401 GITHUB_EMAIL_MISMATCH), a verified email that would swap one GitHub
// identity for another is refused (409 GITHUB_ACCOUNT_CONFLICT), and a
// disabled account cannot sign in (403 ACCOUNT_DISABLED).
func TestGitHubTokenTypedRefusals(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"})
	redirectURI := fixture.httpServer.URL + "/landing"
	email := "refuse-" + runID + "@gh-" + runID + ".test"

	// Sign up once so the account exists.
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "refuse-" + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: email, Primary: true, Verified: true}}
	signUpUser, signUpTokens := githubWalk(t, fixture, redirectURI)

	// Mismatch: the GitHub account now verifies a different address. The
	// callback still hands off (the identity is real), but the redeem is
	// refused — the two sides disagree about who is signing in.
	fixture.fake.emails = []fakeGitHubEmail{{Email: "elsewhere-" + runID + "@gh-" + runID + ".test", Primary: true, Verified: true}}
	_, state := githubAuthorize(t, fixture, redirectURI)
	query := url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	var mismatch ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": location.Query().Get("github_code")}, http.StatusUnauthorized, &mismatch, "")
	if mismatch.Code != "GITHUB_EMAIL_MISMATCH" {
		t.Fatalf("mismatched email must answer GITHUB_EMAIL_MISMATCH, got %q", mismatch.Code)
	}

	// Conflict: the account's email belongs to a user already linked to a
	// different GitHub identity; the link must not be swapped.
	fixture.fake.emails = []fakeGitHubEmail{{Email: email, Primary: true, Verified: true}}
	fixture.fake.profile = fakeGitHubProfile{ID: base + 1, Login: "impostor-" + runID}
	_, state = githubAuthorize(t, fixture, redirectURI)
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	var conflict ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": location.Query().Get("github_code")}, http.StatusConflict, &conflict, "")
	if conflict.Code != "GITHUB_ACCOUNT_CONFLICT" {
		t.Fatalf("identity swap must answer GITHUB_ACCOUNT_CONFLICT, got %q", conflict.Code)
	}

	// Disabled: the linked account is disabled and cannot sign in.
	var organizations []Organization
	getJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/organizations", http.StatusOK, &organizations, "Bearer "+signUpTokens.AccessToken)
	if len(organizations) != 1 {
		t.Fatalf("expected the sign-up organization, got %#v", organizations)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := fixture.store.SCIMSetActive(ctx, organizations[0].ID, signUpUser.ID, false, database.AuditInput{}); err != nil {
		t.Fatal(err)
	}
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "refuse-" + runID}
	_, state = githubAuthorize(t, fixture, redirectURI)
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	var disabled ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": location.Query().Get("github_code")}, http.StatusForbidden, &disabled, "")
	if disabled.Code != "ACCOUNT_DISABLED" {
		t.Fatalf("a disabled account must answer ACCOUNT_DISABLED, got %q", disabled.Code)
	}
}

// TestGitHubSessionCookieSemantics: redeem honors login's session_cookie
// rule — a browser session gets the rotating refresh token as the httpOnly
// shift_rt cookie with the body copy removed, a plain client keeps the body
// token.
func TestGitHubSessionCookieSemantics(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"})
	redirectURI := fixture.httpServer.URL + "/landing"
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "cookie-" + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: "cookie-" + runID + "@gh-" + runID + ".test", Primary: true, Verified: true}}

	_, state := githubAuthorize(t, fixture, redirectURI)
	query := url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	handoff := location.Query().Get("github_code")

	// session_cookie: true → Set-Cookie shift_rt, no body refresh token.
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fixture.httpServer.URL+"/v1/auth/github/token",
		strings.NewReader(`{"code":"`+handoff+`","session_cookie":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := fixture.httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("cookie redeem status %d: %s", response.StatusCode, content)
	}
	var body struct {
		Tokens SessionTokens `json:"tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Tokens.RefreshToken != "" {
		t.Fatal("cookie session must not carry the refresh token in the body")
	}
	foundCookie := false
	for _, cookie := range response.Cookies() {
		if cookie.Name == refreshCookieName && cookie.HttpOnly && cookie.Value != "" {
			foundCookie = true
		}
	}
	if !foundCookie {
		t.Fatal("cookie session must set the httpOnly shift_rt cookie")
	}

	// A second walk without session_cookie keeps the body refresh token.
	_, state = githubAuthorize(t, fixture, redirectURI)
	query = url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location = githubCallback(t, fixture, query)
	_, tokens := githubRedeem(t, fixture, location.Query().Get("github_code"), http.StatusOK)
	if tokens.RefreshToken == "" {
		t.Fatal("non-cookie session must carry the refresh token in the body")
	}
}

// TestGitHubSSOEnforcedDomainRefused: an organization that enforces SSO for
// an email domain owns how that domain's accounts authenticate — GitHub
// sign-in must not open a second door (403 SSO_REQUIRED), consistent with
// the password register/login refusals.
func TestGitHubSSOEnforcedDomainRefused(t *testing.T) {
	runID := githubRunID(t)
	base := githubNumericID(t)
	fixture := openGitHubTestServer(t, config.GitHub{ClientID: "client-id", ClientSecret: "client-secret"})
	// Enforcement binds only with the OIDC block configured; point it at a
	// throwaway issuer this test never contacts.
	fixture.server.config.OIDC = config.OIDC{Issuer: "https://issuer.example.test", ClientID: "issuer-client", ClientSecret: "issuer-secret"}

	domain := "enforced-" + runID + ".test"
	register := RegisterRequest{Email: "owner-" + runID + "@" + domain, Password: "correct horse battery staple", DisplayName: "Enforcer", Organization: "Enforcing Organization"}
	var registration struct {
		Organization Organization `json:"organization"`
	}
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	if err := fixture.store.SetOrganizationSSO(context.Background(), registration.Organization.ID, domain, true, database.AuditInput{}); err != nil {
		t.Fatal(err)
	}

	redirectURI := fixture.httpServer.URL + "/landing"
	fixture.fake.profile = fakeGitHubProfile{ID: base, Login: "blocked-" + runID}
	fixture.fake.emails = []fakeGitHubEmail{{Email: "blocked-" + runID + "@" + domain, Primary: true, Verified: true}}
	_, state := githubAuthorize(t, fixture, redirectURI)
	query := url.Values{}
	query.Set("code", "oauth-code-"+runID)
	query.Set("state", state)
	_, location := githubCallback(t, fixture, query)
	var refusal ErrorResponse
	postJSON(t, fixture.httpServer.Client(), fixture.httpServer.URL+"/v1/auth/github/token",
		map[string]string{"code": location.Query().Get("github_code")}, http.StatusForbidden, &refusal, "")
	if refusal.Code != "SSO_REQUIRED" {
		t.Fatalf("github sign-in on an enforced domain must answer SSO_REQUIRED, got %q", refusal.Code)
	}
}
