package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/oidc"
)

// auth_cookie_session_test.go: the browser session surface end to end against
// the integration database — register, login, and SSO all hand the rotating
// refresh token to the httpOnly shift_rt cookie (never the JSON body), the
// cookie alone can rotate and revoke the session (the reload and logout
// cases), and the hardening flags never weaken.

// cookieFlowRequest sends one JSON request and returns the raw response so a
// test can inspect Set-Cookie as well as the body. The client carries no
// cookie jar: cookies travel only where the test puts them.
func cookieFlowRequest(t *testing.T, client *http.Client, method, endpoint string, input any, headers map[string]string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), method, endpoint, bytes.NewReader(ssoMustJSON(t, input)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// mustCookie returns the named Set-Cookie or fails the test.
func mustCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q missing from response", name)
	return nil
}

// requireHardenedRefreshCookie pins the flags every issued refresh cookie
// must carry: unreadable to script, TLS-only, auth-path-scoped, and never
// attached from a cross-site context.
func requireHardenedRefreshCookie(t *testing.T, cookie *http.Cookie) {
	t.Helper()
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != refreshCookiePath {
		t.Fatalf("refresh cookie lacks hardening flags: %+v", cookie)
	}
}

// registerPlain registers a fresh account through the non-browser path (no
// Sec-Fetch-Mode), which keeps the refresh token in the body — the CLI shape
// later steps contrast against.
func registerPlain(t *testing.T, client *http.Client, endpoint string) (User, SessionTokens) {
	t.Helper()
	registrant, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	response := cookieFlowRequest(t, client, http.MethodPost, endpoint+"/v1/auth/register", RegisterRequest{
		Email:        "cookie-" + registrant + "@org-" + registrant + ".test",
		Password:     "correct horse battery staple",
		DisplayName:  "Cookie User",
		Organization: "Cookie Organization",
	}, nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", response.StatusCode)
	}
	var registration struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&registration); err != nil {
		t.Fatal(err)
	}
	if registration.Tokens.RefreshToken == "" {
		t.Fatal("the non-browser registration must return the refresh token in the body")
	}
	return registration.User, registration.Tokens
}

// TestRegisterIssuesBrowserCookieSession proves the signup session is cookie
// anchored from the first response: the body never carries the rotating
// token, and the cookie alone can refresh and revoke the session — the
// dashboard's reload and logout paths.
func TestRegisterIssuesBrowserCookieSession(t *testing.T) {
	httpServer, _, _, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	registrant, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	response := cookieFlowRequest(t, client, http.MethodPost, httpServer.URL+"/v1/auth/register", RegisterRequest{
		Email:        "browser-" + registrant + "@org-" + registrant + ".test",
		Password:     "correct horse battery staple",
		DisplayName:  "Browser User",
		Organization: "Browser Organization",
	}, map[string]string{"Sec-Fetch-Mode": "cors"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("register status = %d, want 201", response.StatusCode)
	}
	var registration struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	if err := json.NewDecoder(response.Body).Decode(&registration); err != nil {
		t.Fatal(err)
	}
	if registration.Tokens.RefreshToken != "" {
		t.Fatalf("a browser registration must strip the refresh token from the body, got %q", registration.Tokens.RefreshToken)
	}
	cookie := mustCookie(t, response, refreshCookieName)
	requireHardenedRefreshCookie(t, cookie)

	// The access token works immediately.
	var me map[string]any
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusOK, &me, "Bearer "+registration.Tokens.AccessToken)

	// The reload case: the cookie alone rotates the session.
	refreshRequest, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/auth/refresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshRequest.AddCookie(cookie)
	refreshed, err := client.Do(refreshRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer refreshed.Body.Close()
	if refreshed.StatusCode != http.StatusOK {
		t.Fatalf("cookie-only refresh status = %d, want 200", refreshed.StatusCode)
	}
	var rotated SessionTokens
	if err := json.NewDecoder(refreshed.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken == "" || rotated.RefreshToken != "" {
		t.Fatalf("cookie-only refresh must return a fresh access token and no body refresh token: %+v", rotated)
	}
	rotatedCookie := mustCookie(t, refreshed, refreshCookieName)
	requireHardenedRefreshCookie(t, rotatedCookie)

	// The logout case: the cookie alone revokes the session, and clears
	// itself; a later presentation of the retired cookie is refused.
	logoutRequest, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/auth/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	logoutRequest.AddCookie(rotatedCookie)
	loggedOut, err := client.Do(logoutRequest)
	if err != nil {
		t.Fatal(err)
	}
	loggedOut.Body.Close()
	if loggedOut.StatusCode != http.StatusNoContent {
		t.Fatalf("cookie logout status = %d, want 204", loggedOut.StatusCode)
	}
	if cleared := mustCookie(t, loggedOut, refreshCookieName); cleared.MaxAge >= 0 {
		t.Fatalf("logout must expire the cookie, MaxAge = %d", cleared.MaxAge)
	}
	replayRequest, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/auth/refresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	replayRequest.AddCookie(rotatedCookie)
	replay, err := client.Do(replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	replay.Body.Close()
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("refresh with a revoked cookie status = %d, want 401", replay.StatusCode)
	}
}

// TestLoginSessionCookieFlag proves the explicit session_cookie flag takes
// the same path as the browser hint, and that a body-token login (CLI shape)
// is unaffected.
func TestLoginSessionCookieFlag(t *testing.T) {
	httpServer, _, _, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	user, _ := registerPlain(t, client, httpServer.URL)

	// CLI shape: no cookie machinery at all.
	plain := cookieFlowRequest(t, client, http.MethodPost, httpServer.URL+"/v1/auth/login", LoginRequest{Email: user.Email, Password: "correct horse battery staple", Remember: true}, nil)
	defer plain.Body.Close()
	var plainSession struct {
		Tokens SessionTokens `json:"tokens"`
	}
	if err := json.NewDecoder(plain.Body).Decode(&plainSession); err != nil {
		t.Fatal(err)
	}
	if plainSession.Tokens.RefreshToken == "" {
		t.Fatal("a non-browser login must return the refresh token in the body")
	}
	for _, cookie := range plain.Cookies() {
		if cookie.Name == refreshCookieName {
			t.Fatal("a non-browser login must not set the refresh cookie")
		}
	}

	// Browser shape: session_cookie strips the body token behind the cookie.
	browser := cookieFlowRequest(t, client, http.MethodPost, httpServer.URL+"/v1/auth/login", LoginRequest{Email: user.Email, Password: "correct horse battery staple", Remember: true, SessionCookie: true}, nil)
	defer browser.Body.Close()
	if browser.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", browser.StatusCode)
	}
	var session struct {
		Tokens SessionTokens `json:"tokens"`
	}
	if err := json.NewDecoder(browser.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.Tokens.RefreshToken != "" {
		t.Fatalf("a cookie login must strip the refresh token from the body, got %q", session.Tokens.RefreshToken)
	}
	requireHardenedRefreshCookie(t, mustCookie(t, browser, refreshCookieName))
}

// TestSSOTokenIssuesBrowserCookieSession proves a federated login hands the
// browser the same cookie-anchored session as password login: the rotating
// token never reaches script, and the cookie survives to refresh the session
// after a reload.
func TestSSOTokenIssuesBrowserCookieSession(t *testing.T) {
	httpServer, _, _, idp := openSSOTestServer(t, true)
	client := httpServer.Client()
	verifier, challenge, err := oidc.Verifier()
	if err != nil {
		t.Fatal(err)
	}
	redirectURI := "http://127.0.0.1:49152/callback"
	var authorization struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
	}
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/authorize",
		map[string]string{"email": idp.email, "redirect_uri": redirectURI, "code_challenge": challenge},
		http.StatusOK, &authorization, "")

	browser := &http.Client{Transport: idp.server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	walk, err := http.NewRequestWithContext(context.Background(), http.MethodGet, authorization.AuthorizationURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	walkResponse, err := browser.Do(walk)
	if err != nil {
		t.Fatal(err)
	}
	defer walkResponse.Body.Close()
	location, err := url.Parse(walkResponse.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}

	redeem := cookieFlowRequest(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/token",
		map[string]string{"code": location.Query().Get("code"), "state": authorization.State, "code_verifier": verifier, "redirect_uri": redirectURI},
		map[string]string{"Sec-Fetch-Mode": "cors"})
	defer redeem.Body.Close()
	if redeem.StatusCode != http.StatusOK {
		t.Fatalf("sso token status = %d, want 200", redeem.StatusCode)
	}
	var session struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	if err := json.NewDecoder(redeem.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.Tokens.RefreshToken != "" {
		t.Fatalf("a browser sso login must strip the refresh token from the body, got %q", session.Tokens.RefreshToken)
	}
	cookie := mustCookie(t, redeem, refreshCookieName)
	requireHardenedRefreshCookie(t, cookie)

	// The federated session refreshes from the cookie alone.
	refreshRequest, err := http.NewRequestWithContext(context.Background(), http.MethodPost, httpServer.URL+"/v1/auth/refresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	refreshRequest.AddCookie(cookie)
	refreshed, err := client.Do(refreshRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer refreshed.Body.Close()
	if refreshed.StatusCode != http.StatusOK {
		t.Fatalf("cookie-only refresh after sso status = %d, want 200", refreshed.StatusCode)
	}
	var rotated SessionTokens
	if err := json.NewDecoder(refreshed.Body).Decode(&rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.AccessToken == "" || rotated.RefreshToken != "" {
		t.Fatalf("cookie-only refresh after sso must rotate the access token and keep the body refresh token empty: %+v", rotated)
	}
}
