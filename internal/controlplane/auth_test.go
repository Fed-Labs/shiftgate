package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	pepper := strings.Repeat("p", 48)
	hash, err := hashPassword("correct horse battery staple", pepper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("unexpected password hash format %q", hash)
	}
	if !verifyPassword("correct horse battery staple", hash, pepper) {
		t.Fatal("password did not verify")
	}
	if verifyPassword("wrong password", hash, pepper) {
		t.Fatal("wrong password verified")
	}
	if verifyPassword("correct horse battery staple", hash, strings.Repeat("q", 48)) {
		t.Fatal("wrong pepper verified")
	}
}

func TestTokenParsingAndExpiry(t *testing.T) {
	token, _, err := newToken(accessTokenPrefix)
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := parseBearer("Bearer " + token)
	if !ok || parsed != token {
		t.Fatalf("bearer token was not parsed: %q %v", parsed, ok)
	}
	if _, ok := parseBearer("Basic " + token); ok {
		t.Fatal("basic authorization was accepted")
	}
	now := time.Now()
	if !validTokenExpiry(now, now.Add(10*time.Second)) || validTokenExpiry(now, now.Add(time.Second)) {
		t.Fatal("token expiry policy is incorrect")
	}
}

func TestAPIKeyScopesAreResourceAware(t *testing.T) {
	if !scopeAllows([]string{"machines"}, RoleViewer, "machines") || !scopeAllows([]string{"machines"}, RoleOperator, "machines") {
		t.Fatal("machines scope did not grant machine access")
	}
	if scopeAllows([]string{"machines"}, RoleViewer, "workloads") || scopeAllows([]string{"machines"}, RoleOperator, "migrations") {
		t.Fatal("resource-specific scope escaped its resource")
	}
	if !scopeAllows([]string{"read"}, RoleViewer, "billing") || scopeAllows([]string{"read"}, RoleOperator, "machines") {
		t.Fatal("read scope role boundary is incorrect")
	}
	if !scopeAllows([]string{"operate"}, RoleOperator, "checkpoints") || scopeAllows([]string{"operate"}, RoleAdmin, "administration") {
		t.Fatal("operate scope role boundary is incorrect")
	}
	if !scopeAllows([]string{"admin"}, RoleAdmin, "administration") {
		t.Fatal("admin scope did not grant administrative access")
	}
}

// TestRefreshCookieTransportFlags pins the session cookie's hardening flags:
// script must never read it (HttpOnly), it must never ride plaintext
// (Secure), it is attached to no API call outside the auth paths (Path), a
// cross-site context must not attach it (SameSite=Strict), and its lifetime
// follows the session's refresh lifetime.
func TestRefreshCookieTransportFlags(t *testing.T) {
	recorder := httptest.NewRecorder()
	setRefreshCookie(recorder, "shift_rt_example", 24*time.Hour)
	cookies := recorder.Result().Cookies()
	var cookie *http.Cookie
	for _, candidate := range cookies {
		if candidate.Name == refreshCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatalf("the refresh cookie %q was not set, got %v", refreshCookieName, cookies)
	}
	if !cookie.HttpOnly {
		t.Fatal("the refresh cookie must be HttpOnly")
	}
	if !cookie.Secure {
		t.Fatal("the refresh cookie must be Secure")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("the refresh cookie SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Path != refreshCookiePath {
		t.Fatalf("the refresh cookie Path = %q, want %q", cookie.Path, refreshCookiePath)
	}
	if cookie.MaxAge != int((24 * time.Hour).Seconds()) {
		t.Fatalf("the refresh cookie MaxAge = %d, want the session refresh lifetime", cookie.MaxAge)
	}
}

// A logged-out browser must not keep presenting a revoked token: the clear
// cookie expires the name immediately and keeps the same hardening flags.
func TestClearRefreshCookieExpires(t *testing.T) {
	recorder := httptest.NewRecorder()
	clearRefreshCookie(recorder)
	var cookie *http.Cookie
	for _, candidate := range recorder.Result().Cookies() {
		if candidate.Name == refreshCookieName {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("the clear-refresh cookie was not set")
	}
	if cookie.MaxAge >= 0 {
		t.Fatalf("the clear-refresh cookie MaxAge = %d, want negative (delete)", cookie.MaxAge)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("the clear-refresh cookie must keep the hardening flags")
	}
}

// TestBrowserSessionDetection pins the cookie-issuance trigger: an explicit
// session_cookie flag or a browser CORS fetch rides the cookie; anything else
// keeps the body token (CLI and agents).
func TestBrowserSessionDetection(t *testing.T) {
	plain := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/login", nil)
	if browserSession(plain, false) {
		t.Fatal("a non-browser request must not be treated as a browser session")
	}
	if !browserSession(plain, true) {
		t.Fatal("the explicit session_cookie flag must force a cookie session")
	}
	browser := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/login", nil)
	browser.Header.Set("Sec-Fetch-Mode", "cors")
	if !browserSession(browser, false) {
		t.Fatal("a browser CORS fetch must ride the httpOnly refresh cookie")
	}
}

// The cookie is transport for the same rotating digest-only token the body
// carries; the round trip must recover exactly what was set.
func TestRefreshCookieValueRoundTrip(t *testing.T) {
	recorder := httptest.NewRecorder()
	setRefreshCookie(recorder, "shift_rt_round_trip", time.Hour)
	var issued *http.Cookie
	for _, candidate := range recorder.Result().Cookies() {
		if candidate.Name == refreshCookieName {
			issued = candidate
		}
	}
	if issued == nil {
		t.Fatal("the refresh cookie was not set")
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/refresh", nil)
	request.AddCookie(issued)
	if got := refreshCookieValue(request); got != "shift_rt_round_trip" {
		t.Fatalf("refreshCookieValue = %q, want the issued token", got)
	}
	withoutCookie := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/auth/refresh", nil)
	if got := refreshCookieValue(withoutCookie); got != "" {
		t.Fatalf("refreshCookieValue without a cookie = %q, want empty", got)
	}
}
