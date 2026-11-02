package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// github_routes.go: "Sign in / Sign up with GitHub". The browser flow is
// three endpoints and a redirect chain:
//
//	POST /v1/auth/github/authorize  — mint a GitHub authorization URL whose
//	  signed state binds the caller's post-login redirect_uri;
//	GET  /v1/auth/github/callback   — GitHub lands here; the code is
//	  exchanged server-side (tokens never touch the browser), the identity is
//	  resolved, and the browser is redirected to the bound redirect_uri with
//	  a one-time handoff code;
//	POST /v1/auth/github/token      — redeem the handoff code for a SHIFT
//	  session: sign in (known github_id), sign up (unknown verified email,
//	  user + initial org exactly like register), or link (verified email of
//	  an unlinked account).
//
// The signed state is anti-CSRF and anti-open-redirect: it is HMAC-sealed
// with the token pepper over the caller's redirect_uri and a nonce with a
// short TTL, so a forged or stale state is refused in place instead of
// redirecting anywhere.

const (
	// githubStateTTL bounds an authorization round trip. A state older than
	// this is refused: the login attempt it belongs to is long over.
	githubStateTTL = 10 * time.Minute
	// githubCodeTTL bounds the browser handoff from callback to token
	// redemption. The code is single use on top of this, so a leaked redirect
	// URL is dead on arrival at worst one window later.
	githubCodeTTL = 10 * time.Minute
)

// githubEndpoints are the GitHub OAuth and API endpoints this flow talks to.
// They carry production values but stay variables so the integration tests
// can walk the entire redirect chain against a local GitHub double — the
// same pattern as the SSO flow's issuer stubs. Nothing but tests rewrites
// them.
var (
	githubAuthorizeEndpoint = "https://github.com/login/oauth/authorize"
	githubTokenEndpoint     = "https://github.com/login/oauth/access_token"
	githubAPIEndpoint       = "https://api.github.com"
)

// githubHTTPClient is the shared outbound client for GitHub traffic. Tests
// install the double's own client so its TLS certificate is trusted.
var githubHTTPClient = &http.Client{Timeout: 20 * time.Second}

// GitHubAuthorizeRequest starts the flow. The redirect_uri is where the
// browser should land after the callback — validated exactly like the
// billing portal's return_url (absolute http(s), host required).
type GitHubAuthorizeRequest struct {
	RedirectURI string `json:"redirect_uri"`
}

// GitHubAuthorizeResponse is what the client opens in the browser.
type GitHubAuthorizeResponse struct {
	AuthorizeURL string `json:"authorize_url"`
	State        string `json:"state"`
}

// GitHubTokenRequest redeems the one-time handoff code the callback
// redirected with. SessionCookie carries the same meaning as on login.
type GitHubTokenRequest struct {
	Code          string `json:"code"`
	SessionCookie bool   `json:"session_cookie"`
}

// githubState is the signed blob that travels to GitHub and back as the OAuth
// state parameter.
type githubState struct {
	RedirectURI string    `json:"redirect_uri"`
	Nonce       string    `json:"nonce"`
	Expiry      time.Time `json:"expiry"`
}

// githubFlowError is a typed failure of the browser flow. The callback maps
// it onto the bound redirect_uri as ?error=<Code> so the web app can tell an
// unverified email apart from an org denial from a plain failure — the error
// codes are the contract's, the message is for logs.
type githubFlowError struct {
	Code    string
	Message string
}

func (flowError *githubFlowError) Error() string { return flowError.Message }

// handleGitHubAuthorize mints the GitHub authorization URL. The OAuth
// redirect_uri is this control plane's own callback — never the caller's URL:
// the GitHub OAuth app pins exactly one callback, and the state (not the
// redirect chain) is what decides where the browser ends up afterwards.
func (server *Server) handleGitHubAuthorize(writer http.ResponseWriter, request *http.Request) {
	if !server.config.GitHub.Configured() {
		// A deployment without credentials is a state, not a failure: typed
		// 503 exactly like BILLING_NOT_CONFIGURED / SSO_NOT_CONFIGURED.
		writeError(writer, http.StatusServiceUnavailable, "GITHUB_NOT_CONFIGURED", "github sign-in is not configured on this control plane")
		return
	}
	var input GitHubAuthorizeRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if !server.allowRegistrationAttempt(request) {
		writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "too many authentication attempts; retry later")
		return
	}
	redirectURI, ok := safeReturnURL(input.RedirectURI)
	if !ok {
		writeError(writer, http.StatusBadRequest, "REDIRECT_INVALID", "redirect_uri must be an absolute http(s) URL")
		return
	}
	state, err := server.mintGitHubState(redirectURI)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "GITHUB_STATE_FAILED", err)
		return
	}
	values := url.Values{}
	values.Set("client_id", server.config.GitHub.ClientID)
	values.Set("scope", "read:user user:email")
	values.Set("redirect_uri", server.githubCallbackURL(request))
	values.Set("state", state)
	writeJSON(writer, http.StatusOK, GitHubAuthorizeResponse{AuthorizeURL: githubAuthorizeEndpoint + "?" + values.Encode(), State: state})
}

// handleGitHubCallback is the browser landing point after GitHub. It
// validates the signed state, exchanges the code with GitHub entirely
// server-side, resolves the primary verified email (and org membership when
// gating is on), and redirects to the state-bound redirect_uri with a
// one-time handoff code — never with GitHub tokens.
func (server *Server) handleGitHubCallback(writer http.ResponseWriter, request *http.Request) {
	if !server.config.GitHub.Configured() {
		writeError(writer, http.StatusServiceUnavailable, "GITHUB_NOT_CONFIGURED", "github sign-in is not configured on this control plane")
		return
	}
	query := request.URL.Query()
	redirectURI, err := server.verifyGitHubState(query.Get("state"))
	if err != nil {
		// The state is the only thing that says where the browser may be sent
		// back. With it unverified there is no trustworthy redirect target,
		// so the failure answers in place instead of redirecting — this is
		// the anti-open-redirect rule, and it applies to malformed, forged,
		// and expired states alike.
		writeError(writer, http.StatusBadRequest, "GITHUB_STATE_INVALID", err.Error())
		return
	}
	if githubError := query.Get("error"); githubError != "" {
		// GitHub redirected back with an error (the user denied the grant,
		// the app was suspended, ...). The denial is its own honest code so
		// the web app can say "you cancelled" instead of "something broke".
		code := "github_auth_failed"
		if githubError == "access_denied" {
			code = "github_access_denied"
		}
		githubRedirect(writer, request, redirectURI, "error", code)
		return
	}
	authorizationCode := query.Get("code")
	if authorizationCode == "" {
		githubRedirect(writer, request, redirectURI, "error", "github_auth_failed")
		return
	}
	identity, resolveErr := server.resolveGitHubIdentity(request.Context(), authorizationCode, server.githubCallbackURL(request))
	if resolveErr != nil {
		githubRedirect(writer, request, redirectURI, "error", resolveErr.Code)
		return
	}
	handoff, _, err := newToken(githubCodePrefix)
	if err != nil {
		server.logger.Error("github handoff code generation failed", "request_id", requestID(request), "error", err)
		githubRedirect(writer, request, redirectURI, "error", "github_auth_failed")
		return
	}
	record := database.GitHubAuthCodeRecord{GitHubID: identity.GitHubID, Login: identity.Login, Email: identity.Email, DisplayName: identity.DisplayName}
	if err := server.database.CreateGitHubAuthCode(request.Context(), tokenDigest(server.config.TokenPepper, handoff), record, time.Now().UTC().Add(githubCodeTTL)); err != nil {
		server.logger.Error("github handoff code storage failed", "request_id", requestID(request), "error", err)
		githubRedirect(writer, request, redirectURI, "error", "github_auth_failed")
		return
	}
	githubRedirect(writer, request, redirectURI, "github_code", handoff)
}

// handleGitHubToken redeems the one-time handoff code for a SHIFT session.
// Semantics: sign in when a user with this github_id exists; sign up (user +
// initial organization exactly like register) when the verified email
// matches no account; link (attach the GitHub identity, then sign in) when
// the email matches an account without one. The response is login's shape
// and the refresh token rides the same shift_rt cookie rules.
func (server *Server) handleGitHubToken(writer http.ResponseWriter, request *http.Request) {
	if !server.config.GitHub.Configured() {
		writeError(writer, http.StatusServiceUnavailable, "GITHUB_NOT_CONFIGURED", "github sign-in is not configured on this control plane")
		return
	}
	var input GitHubTokenRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	code := strings.TrimSpace(input.Code)
	if code == "" {
		writeError(writer, http.StatusBadRequest, "GITHUB_CODE_REQUIRED", "code is required")
		return
	}
	if !server.allowCredentialAttempt(request, code) {
		writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "too many authentication attempts; retry later")
		return
	}
	record, err := server.database.ConsumeGitHubAuthCode(request.Context(), tokenDigest(server.config.TokenPepper, code), time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, database.ErrGitHubCodeReplayed):
			writeError(writer, http.StatusUnauthorized, "GITHUB_CODE_REPLAYED", "this sign-in code was already used; start the login again")
		case errors.Is(err, database.ErrGitHubCodeExpired):
			writeError(writer, http.StatusUnauthorized, "GITHUB_CODE_EXPIRED", "this sign-in code has expired; start the login again")
		case errors.Is(err, database.ErrGitHubCodeInvalid):
			writeError(writer, http.StatusUnauthorized, "GITHUB_CODE_INVALID", "sign-in code is invalid")
		default:
			server.writeInternalError(writer, request, http.StatusInternalServerError, "GITHUB_CODE_CONSUME_FAILED", err)
		}
		return
	}
	// An organization that enforces SSO owns this email domain: its accounts
	// authenticate through the issuer and no other door opens them — not
	// passwords (handleLogin) and not GitHub sign-in. Fails closed like the
	// password path: a transient database error never lets the sign-in
	// through.
	if server.config.OIDC.Configured() {
		enforced, enforceErr := server.database.SSOEnforcedForEmail(request.Context(), record.Email)
		if enforceErr != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_ENFORCEMENT_LOOKUP_FAILED", enforceErr)
			return
		}
		if enforced {
			writeError(writer, http.StatusForbidden, "SSO_REQUIRED", "this email domain requires single sign-on; use 'shift login --sso'")
			return
		}
	}
	userID, err := model.NewID()
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", err)
		return
	}
	organizationID, err := model.NewID()
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "ID_GENERATION_FAILED", err)
		return
	}
	// Display names come from GitHub's API, not the stricter registration
	// input: clamp instead of refusing a legitimate sign-in over a long name.
	displayName := clampGitHubName(firstNonEmpty(record.DisplayName, record.Login, record.Email))
	user, organization, created, err := server.database.FederateGitHub(request.Context(),
		database.GitHubIdentity{GitHubID: record.GitHubID, Login: record.Login, Email: record.Email, DisplayName: record.DisplayName},
		userID, organizationID, clampGitHubName(displayName+"'s organization"),
		server.auditInput(request, "user.github_login", "user", "", map[string]any{"github_login": record.Login}))
	if err != nil {
		switch {
		case errors.Is(err, database.ErrUserDisabled):
			writeError(writer, http.StatusForbidden, "ACCOUNT_DISABLED", "this account is disabled")
		case errors.Is(err, database.ErrGitHubEmailMismatch):
			writeError(writer, http.StatusUnauthorized, "GITHUB_EMAIL_MISMATCH", "the verified github email no longer matches this account's email")
		case errors.Is(err, database.ErrGitHubAccountConflict):
			writeError(writer, http.StatusConflict, "GITHUB_ACCOUNT_CONFLICT", "this account is already linked to a different github identity")
		case errors.Is(err, database.ErrSSOAccountLinked):
			writeError(writer, http.StatusConflict, "ACCOUNT_ALREADY_FEDERATED", err.Error())
		default:
			server.writeInternalError(writer, request, http.StatusInternalServerError, "GITHUB_LINK_FAILED", err)
		}
		return
	}
	if created {
		// Same abuse hygiene as register (plan 4 BR-13b): audit-only velocity
		// flag when one email domain accumulates free organizations.
		server.flagDomainVelocity(request.Context(), organization.ID, record.Email)
	}
	// A GitHub sign-in is a deliberate account login, not a shared-terminal
	// password entry, so it carries the full remembered refresh lifetime —
	// the same choice the OIDC SSO token makes.
	sessionTokens, err := server.issueSession(request.Context(), user.ID, server.config.RefreshTokenTTL, request)
	if err != nil {
		if created {
			// The account is already committed; say so (register's rule) so a
			// retry that answers differently does not read as a contradiction.
			server.logger.Error("session issuance after github sign-up failed", "request_id", requestID(request), "user_id", user.ID, "error", err)
			writeError(writer, http.StatusInternalServerError, "SESSION_CREATE_FAILED", "account created but sign-in failed; log in with GitHub again to continue")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SESSION_CREATE_FAILED", err)
		return
	}
	// Browser sessions ride the httpOnly refresh cookie exactly as on login:
	// the rotating token leaves the body entirely for cookie sessions.
	if browserSession(request, input.SessionCookie) {
		setRefreshCookie(writer, sessionTokens.RefreshToken, server.config.RefreshTokenTTL)
		sessionTokens.RefreshToken = ""
	}
	writeJSON(writer, http.StatusOK, map[string]any{"user": userResponse(user), "tokens": sessionTokens})
}

// mintGitHubState signs the state blob binding this authorization to one
// redirect_uri and one nonce for githubStateTTL. Signing with the token
// pepper means a forged state is as hard to produce as a session token.
func (server *Server) mintGitHubState(redirectURI string) (string, error) {
	nonce, _, err := newToken("ghn_")
	if err != nil {
		return "", fmt.Errorf("generate github state nonce: %w", err)
	}
	blob, err := json.Marshal(githubState{RedirectURI: redirectURI, Nonce: nonce, Expiry: time.Now().UTC().Add(githubStateTTL)})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(blob)
	signature := base64.RawURLEncoding.EncodeToString(hmacSHA256([]byte(server.config.TokenPepper), []byte(encoded)))
	return encoded + "." + signature, nil
}

// verifyGitHubState checks the signature, expiry, and nonce of a state and
// returns the redirect_uri it was minted for. Every failure mode is fatal for
// the redirect: the caller must answer in place.
func (server *Server) verifyGitHubState(state string) (string, error) {
	encoded, signature, ok := strings.Cut(state, ".")
	if !ok || encoded == "" || signature == "" {
		return "", errors.New("state is malformed")
	}
	expected := base64.RawURLEncoding.EncodeToString(hmacSHA256([]byte(server.config.TokenPepper), []byte(encoded)))
	if !secureCompareString(signature, expected) {
		return "", errors.New("state signature is invalid")
	}
	blob, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("state is malformed")
	}
	var parsed githubState
	if err := json.Unmarshal(blob, &parsed); err != nil {
		return "", errors.New("state is malformed")
	}
	if parsed.Nonce == "" {
		return "", errors.New("state is malformed")
	}
	if time.Now().UTC().After(parsed.Expiry) {
		return "", errors.New("state has expired; start the login again")
	}
	return parsed.RedirectURI, nil
}

// githubRedirect sends the browser to the state-bound redirect_uri carrying
// exactly one extra query parameter (the handoff code, or the error code).
// Existing query parameters survive: the redirect_uri is the caller's own
// application URL and may already carry routing state.
func githubRedirect(writer http.ResponseWriter, request *http.Request, redirectURI, key, value string) {
	target, err := url.Parse(redirectURI)
	if err != nil {
		// Unreachable through safeReturnURL validation, but never redirect to
		// a URL that cannot be parsed.
		writeError(writer, http.StatusBadRequest, "REDIRECT_INVALID", "redirect_uri could not be parsed")
		return
	}
	query := target.Query()
	query.Set(key, value)
	target.RawQuery = query.Encode()
	http.Redirect(writer, request, target.String(), http.StatusFound)
}

// resolveGitHubIdentity walks the OAuth code exchange and the profile reads
// the handoff needs. Any failure is typed with the redirect error code the
// browser should see — the details stay in the message for logs only.
func (server *Server) resolveGitHubIdentity(ctx context.Context, authorizationCode, callbackURL string) (database.GitHubAuthCodeRecord, *githubFlowError) {
	accessToken, flowErr := server.exchangeGitHubCode(ctx, authorizationCode, callbackURL)
	if flowErr != nil {
		return database.GitHubAuthCodeRecord{}, flowErr
	}
	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := server.getGitHubJSON(ctx, accessToken, "/user", &profile); err != nil {
		return database.GitHubAuthCodeRecord{}, &githubFlowError{Code: "github_auth_failed", Message: "github user fetch failed: " + err.Error()}
	}
	if profile.ID == 0 || profile.Login == "" {
		return database.GitHubAuthCodeRecord{}, &githubFlowError{Code: "github_auth_failed", Message: "github user response carried no id or login"}
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := server.getGitHubJSON(ctx, accessToken, "/user/emails", &emails); err != nil {
		return database.GitHubAuthCodeRecord{}, &githubFlowError{Code: "github_auth_failed", Message: "github email fetch failed: " + err.Error()}
	}
	primaryEmail := ""
	for _, address := range emails {
		if address.Primary && address.Verified {
			primaryEmail = address.Email
			break
		}
	}
	if primaryEmail == "" {
		// The identity is real but the mailbox is not proven: no account may
		// be claimed on an unverified address, so the flow stops here with
		// the honest reason instead of the generic failure.
		return database.GitHubAuthCodeRecord{}, &githubFlowError{Code: "github_email_not_verified", Message: "github account has no primary verified email"}
	}
	if len(server.config.GitHub.AllowedOrganizations) > 0 {
		member, err := server.gitHubOrganizationAllowed(ctx, accessToken, profile.Login)
		if err != nil {
			return database.GitHubAuthCodeRecord{}, &githubFlowError{Code: "github_auth_failed", Message: "github organization lookup failed: " + err.Error()}
		}
		if !member {
			return database.GitHubAuthCodeRecord{}, &githubFlowError{Code: "github_org_denied", Message: "github account is not a member of any allowed organization"}
		}
	}
	return database.GitHubAuthCodeRecord{GitHubID: profile.ID, Login: profile.Login, Email: primaryEmail, DisplayName: profile.Name}, nil
}

// exchangeGitHubCode trades the OAuth code for a GitHub access token. GitHub
// answers 200 with an error body for bad codes, so both the transport error
// and the payload error end in the same typed failure.
func (server *Server) exchangeGitHubCode(ctx context.Context, authorizationCode, callbackURL string) (string, *githubFlowError) {
	form := url.Values{}
	form.Set("client_id", server.config.GitHub.ClientID)
	form.Set("client_secret", server.config.GitHub.ClientSecret)
	form.Set("code", authorizationCode)
	form.Set("redirect_uri", callbackURL)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, githubTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", &githubFlowError{Code: "github_auth_failed", Message: "build github token request: " + err.Error()}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := githubHTTPClient.Do(request)
	if err != nil {
		return "", &githubFlowError{Code: "github_auth_failed", Message: "github token exchange failed: " + err.Error()}
	}
	defer func() { _ = response.Body.Close() }()
	var payload struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", &githubFlowError{Code: "github_auth_failed", Message: "github token response was unreadable: " + err.Error()}
	}
	if response.StatusCode != http.StatusOK || payload.AccessToken == "" {
		return "", &githubFlowError{Code: "github_auth_failed", Message: fmt.Sprintf("github token exchange refused (status %d, error %q)", response.StatusCode, payload.Error)}
	}
	return payload.AccessToken, nil
}

// getGitHubJSON performs one authenticated GitHub API read.
func (server *Server) getGitHubJSON(ctx context.Context, accessToken, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIEndpoint+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/vnd.github+json")
	// GitHub's API refuses requests without a User-Agent.
	request.Header.Set("User-Agent", "shift-control")
	response, err := githubHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("github api %s returned status %d", path, response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(target)
}

// gitHubOrganizationAllowed reports whether the user is a member of any
// configured allowlist organization. Memberships come from the API with the
// user's own token; the scope this flow requests (read:user user:email)
// shows public memberships, so a user whose org membership is private is
// denied — the gate fails closed toward the allowlist, never open.
func (server *Server) gitHubOrganizationAllowed(ctx context.Context, accessToken, login string) (bool, error) {
	for page := 1; page <= 10; page++ {
		var organizations []struct {
			Login string `json:"login"`
		}
		if err := server.getGitHubJSON(ctx, accessToken, fmt.Sprintf("/user/orgs?per_page=100&page=%d", page), &organizations); err != nil {
			return false, err
		}
		for _, organization := range organizations {
			for _, allowed := range server.config.GitHub.AllowedOrganizations {
				if strings.EqualFold(strings.TrimSpace(allowed), organization.Login) {
					return true, nil
				}
			}
		}
		if len(organizations) < 100 {
			return false, nil
		}
	}
	return false, nil
}

// githubCallbackURL builds the absolute OAuth callback this flow registers
// with GitHub: this control plane's own /v1/auth/github/callback. The base is
// derived from the request the client already reached the API with (scheme +
// Host), because the deployment's public URL is whatever hostname serves the
// API. Behind the TLS edge the scheme comes from X-Forwarded-Proto, honored
// only from a trusted proxy like every other forwarding header.
func (server *Server) githubCallbackURL(request *http.Request) string {
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	if server.trustedPeer(request) && strings.EqualFold(request.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + request.Host + "/v1/auth/github/callback"
}

// trustedPeer reports whether the socket peer is one of the configured
// trusted proxies, the same gate clientIP applies to forwarding headers.
func (server *Server) trustedPeer(request *http.Request) bool {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(request.RemoteAddr)
	}
	peer := net.ParseIP(host)
	return peer != nil && server.isTrustedProxy(peer)
}

// clampGitHubName bounds a GitHub-supplied name to the display-name length
// registration enforces (200 characters), on a rune boundary so multi-byte
// names never arrive truncated mid-character.
func clampGitHubName(value string) string {
	runes := []rune(value)
	if len(runes) > 200 {
		runes = runes[:200]
	}
	return strings.TrimSpace(string(runes))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
