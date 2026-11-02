package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

func (server *Server) handleRegister(writer http.ResponseWriter, request *http.Request) {
	var input RegisterRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if !server.allowRegistrationAttempt(request) {
		writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "too many authentication attempts; retry later")
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "EMAIL_INVALID", err.Error())
		return
	}
	// Domains under SSO enforcement do not take self-service registrations:
	// those accounts arrive through the issuer or the provisioning system.
	// The enforcement lookup fails closed: a transient database error must
	// never let a password registration through an SSO-enforced domain.
	if server.config.OIDC.Configured() {
		enforced, err := server.database.SSOEnforcedForEmail(request.Context(), email)
		if err != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_ENFORCEMENT_LOOKUP_FAILED", err)
			return
		}
		if enforced {
			writeError(writer, http.StatusForbidden, "SSO_REQUIRED", "this email domain requires single sign-on; use 'shift login --sso'")
			return
		}
	}
	if err := validatePassword(input.Password); err != nil {
		writeError(writer, http.StatusBadRequest, "PASSWORD_INVALID", err.Error())
		return
	}
	displayName := strings.TrimSpace(input.DisplayName)
	if displayName == "" || len(displayName) > 200 {
		writeError(writer, http.StatusBadRequest, "DISPLAY_NAME_INVALID", "display name is required and must be at most 200 characters")
		return
	}
	organizationName := strings.TrimSpace(input.Organization)
	if organizationName == "" {
		organizationName = displayName + "'s organization"
	}
	if len(organizationName) > 200 {
		writeError(writer, http.StatusBadRequest, "ORGANIZATION_INVALID", "organization name is too long")
		return
	}
	passwordHash, err := hashPassword(input.Password, server.config.PasswordPepper)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "PASSWORD_INVALID", err.Error())
		return
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
	user, organization, err := server.database.Register(request.Context(), userID, email, passwordHash, displayName, organizationID, organizationName, server.auditInput(request, "user.register", "user", userID, nil))
	if err != nil {
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "EMAIL_EXISTS", "an account with this email already exists; log in instead")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "REGISTRATION_FAILED", err)
		return
	}
	// Registration is an explicit account creation, not a shared-terminal
	// sign-in, so it always issues the full remembered refresh lifetime.
	// Abuse hygiene (plan 4 BR-13b): audit-only velocity flag when one email
	// domain accumulates free organizations — reversible, bans nothing.
	server.flagDomainVelocity(request.Context(), organization.ID, email)
	tokens, err := server.issueSession(request.Context(), user.ID, server.config.RefreshTokenTTL, request)
	if err != nil {
		// The account is already committed at this point; say so, so a retry
		// that answers EMAIL_EXISTS does not read as a contradiction. The
		// recovery is simply to log in.
		server.logger.Error("session issuance after registration failed", "request_id", requestID(request), "user_id", user.ID, "error", err)
		writeError(writer, http.StatusInternalServerError, "SESSION_CREATE_FAILED", "account created but sign-in failed; log in with your email and password to continue")
		return
	}
	// Browser sessions ride the httpOnly refresh cookie from the first
	// response: the rotating token leaves the body entirely, exactly as on
	// login, so a signup session also survives reloads without ever exposing
	// the refresh credential to script.
	if browserSession(request, false) {
		setRefreshCookie(writer, tokens.RefreshToken, server.config.RefreshTokenTTL)
		tokens.RefreshToken = ""
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"user": userResponse(user), "organization": organizationResponse(organization), "tokens": tokens})
}

func (server *Server) handleLogin(writer http.ResponseWriter, request *http.Request) {
	var input LoginRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if !server.allowCredentialAttempt(request, input.Email) {
		writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "too many authentication attempts; retry later")
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil || len(input.Password) > 1024 {
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
		return
	}
	// An organization that enforces SSO owns this email domain: its accounts
	// authenticate through the issuer, and no password — right or wrong —
	// opens them. The check runs before the user lookup so the answer does
	// not depend on whether the account exists. It fails closed: a transient
	// database error must never let a password login through.
	if server.config.OIDC.Configured() {
		enforced, err := server.database.SSOEnforcedForEmail(request.Context(), email)
		if err != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_ENFORCEMENT_LOOKUP_FAILED", err)
			return
		}
		if enforced {
			writeError(writer, http.StatusForbidden, "SSO_REQUIRED", "this email domain requires single sign-on; use 'shift login --sso'")
			return
		}
	}
	user, err := server.database.UserByEmail(request.Context(), email)
	if err != nil {
		// Unknown accounts cost the same Argon2id work as known ones: the
		// response latency must not say whether the email exists.
		_ = verifyPassword(input.Password, dummyPasswordHash(), server.config.PasswordPepper)
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
		return
	}
	if user.DisabledAt != nil {
		_ = verifyPassword(input.Password, dummyPasswordHash(), server.config.PasswordPepper)
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
		return
	}
	if user.PasswordHash == "" {
		// SSO-only accounts carry no password hash; burn the same work so the
		// account's existence is not observable through timing.
		_ = verifyPassword(input.Password, dummyPasswordHash(), server.config.PasswordPepper)
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
		return
	}
	if !verifyPassword(input.Password, user.PasswordHash, server.config.PasswordPepper) {
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
		return
	}
	// Remembered logins carry the full refresh lifetime; browser-session
	// logins get the short one, so they end when their refresh token expires.
	refreshTTL := server.config.ShortRefreshTokenTTL
	if input.Remember {
		refreshTTL = server.config.RefreshTokenTTL
	}
	tokens, err := server.issueSession(request.Context(), user.ID, refreshTTL, request)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SESSION_CREATE_FAILED", err)
		return
	}
	// Browser sessions ride the httpOnly refresh cookie: the rotating token
	// leaves the response body entirely, so an XSS cannot read it from
	// script-accessible storage or JSON.
	if browserSession(request, input.SessionCookie) {
		setRefreshCookie(writer, tokens.RefreshToken, refreshTTL)
		tokens.RefreshToken = ""
	}
	audit := server.auditInput(request, "user.login", "user", user.ID, nil)
	audit.ActorUserID = user.ID
	_ = server.database.RecordAudit(request.Context(), audit)
	writeJSON(writer, http.StatusOK, map[string]any{"user": userResponse(user), "tokens": tokens})
}

// refreshCookieName is the httpOnly refresh-token cookie. It lives on the
// /v1/auth path only — login, refresh, and logout — so it is never attached
// to ordinary API calls.
const (
	refreshCookieName = "shift_rt"
	refreshCookiePath = "/v1/auth"
)

// browserSession reports whether the session should ride the httpOnly
// refresh cookie: either the caller asked for it explicitly (the dashboard's
// session_cookie flag) or the request is a browser CORS fetch.
func browserSession(request *http.Request, explicit bool) bool {
	return explicit || request.Header.Get("Sec-Fetch-Mode") == "cors"
}

// refreshCookieValue returns the refresh token the request carried in the
// shift_rt cookie, or "" when it has none.
func refreshCookieValue(request *http.Request) string {
	cookie, err := request.Cookie(refreshCookieName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

// setRefreshCookie issues the rotating refresh token as a SameSite=Strict
// httpOnly cookie scoped to the auth paths. The value is the same digest-only
// stored rotating token the body would carry; the cookie is purely transport.
func setRefreshCookie(writer http.ResponseWriter, token string, lifetime time.Duration) {
	maxAge := int(lifetime.Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(writer, &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearRefreshCookie expires the refresh cookie so a logged-out browser does
// not keep presenting a revoked token.
func clearRefreshCookie(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

func (server *Server) handleRefresh(writer http.ResponseWriter, request *http.Request) {
	var input RefreshRequest
	if request.ContentLength != 0 && !decodeJSON(writer, request, &input) {
		return
	}
	refreshToken := strings.TrimSpace(input.RefreshToken)
	fromCookie := false
	if cookieToken := refreshCookieValue(request); cookieToken != "" {
		// The cookie wins when both are present: it carries the same rotating
		// token, and the browser's httpOnly copy is the authoritative one.
		refreshToken = cookieToken
		fromCookie = true
	}
	if !server.allowCredentialAttempt(request, refreshToken) {
		writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "too many authentication attempts; retry later")
		return
	}
	if len(refreshToken) < len(refreshTokenPrefix)+20 {
		writeError(writer, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "refresh token is invalid or expired")
		return
	}
	accessToken, _, err := newToken(accessTokenPrefix)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "TOKEN_GENERATION_FAILED", err)
		return
	}
	nextRefresh, _, err := newToken(refreshTokenPrefix)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "TOKEN_GENERATION_FAILED", err)
		return
	}
	now := time.Now().UTC()
	accessExpires := now.Add(server.config.AccessTokenTTL)
	refreshExpires := now.Add(server.config.RefreshTokenTTL)
	record, err := server.database.RotateSession(request.Context(), tokenDigest(server.config.TokenPepper, refreshToken), tokenDigest(server.config.TokenPepper, accessToken), tokenDigest(server.config.TokenPepper, nextRefresh), accessExpires, refreshExpires, now)
	if err != nil {
		if errors.Is(err, database.ErrRefreshTokenReuse) {
			// A rotated-out refresh token was replayed: the session is already
			// revoked (both the thief and the owner are logged out); record
			// the security event before the generic rejection. The digest
			// prefix joins the row to the session's retired digests in the
			// sessions table (previous_refresh_hash(es)), so an operator can
			// attribute the replay to the attacked account.
			presentedDigest := tokenDigest(server.config.TokenPepper, refreshToken)
			audit := server.auditInput(request, "session.refresh_reuse", "session", "", map[string]any{
				"remote_addr":    server.clientIP(request),
				"refresh_digest": fmt.Sprintf("%x", presentedDigest[:8]),
			})
			_ = server.database.RecordAudit(request.Context(), audit)
		}
		writeError(writer, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "refresh token is invalid or expired")
		return
	}
	bodyRefresh := nextRefresh
	if fromCookie || browserSession(request, input.SessionCookie) {
		// The rotated token goes back only as the httpOnly cookie; the JSON
		// body carries the access half alone. The cookie's lifetime follows
		// the session's remaining refresh lifetime — rotation never extends
		// past the lifetime the remember flag bought — so a non-remembered
		// login keeps its short TTL instead of being promoted to the full
		// remembered one by the first refresh.
		setRefreshCookie(writer, nextRefresh, time.Until(record.RefreshExpiresAt))
		bodyRefresh = ""
	}
	writeJSON(writer, http.StatusOK, SessionTokens{SessionID: record.ID, AccessToken: accessToken, RefreshToken: bodyRefresh, TokenType: "Bearer", ExpiresAt: accessExpires})
}

func (server *Server) handleLogout(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	// Logout is registered without requireAuth so a browser whose access token
	// has expired can still revoke its session through the httpOnly cookie.
	// A presented bearer is resolved right here — tolerating failure so an
	// expired token cannot block the cookie path — because without this the
	// bare route never populates a principal and bearer-only logout (the
	// CLI's) revokes nothing.
	bearerPresented := false
	if principal.SessionID == "" {
		if _, ok := parseBearer(request.Header.Get("Authorization")); ok {
			bearerPresented = true
			if resolved, err := server.authenticate(request); err == nil {
				principal = resolved
			}
		}
	}
	revoked := false
	if principal.SessionID != "" {
		if err := server.database.RevokeSession(request.Context(), principal.SessionID, principal.UserID); err != nil && !database.IsNotFound(err) {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "LOGOUT_FAILED", err)
			return
		}
		revoked = true
	}
	if cookieToken := refreshCookieValue(request); cookieToken != "" {
		// The httpOnly cookie names its session directly: a browser logout
		// revokes that rotating session even without a bearer access token.
		if err := server.database.RevokeSessionByRefreshToken(request.Context(), tokenDigest(server.config.TokenPepper, cookieToken)); err != nil && !database.IsNotFound(err) {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "LOGOUT_FAILED", err)
			return
		}
		revoked = true
		clearRefreshCookie(writer)
	}
	if !revoked {
		if bearerPresented {
			// The client presented credentials that resolve to nothing
			// actionable (expired/revoked token, or an API key, which has no
			// session to revoke): 401 — the status clients treat as "clear
			// your stored tokens", unlike the old 400, which left them in
			// place after a failed logout.
			writeError(writer, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", "access token is invalid or expired; nothing was revoked")
			return
		}
		writeError(writer, http.StatusBadRequest, "SESSION_TOKEN_REQUIRED", "logout requires a user access token")
		return
	}
	audit := server.auditInput(request, "user.logout", "session", principal.SessionID, nil)
	audit.ActorUserID = principal.UserID
	_ = server.database.RecordAudit(request.Context(), audit)
	writer.WriteHeader(http.StatusNoContent)
}

// handleUserErase runs the GDPR erasure path for the calling account: every
// personal identifier is destroyed in place, every session is revoked, and
// audit IPs are scrubbed. The response is the counts-only ErasureReport — it
// never echoes the destroyed data — and the pseudonymous user row survives so
// audit trails and billing history keep valid references.
func (server *Server) handleUserErase(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	report, err := server.database.EraseUser(request.Context(), principal.UserID, server.auditInput(request, "user.erased", "user", principal.UserID, nil))
	if err != nil {
		if errors.Is(err, database.ErrUserNotFound) {
			writeError(writer, http.StatusNotFound, "USER_NOT_FOUND", "user account was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "USER_ERASE_FAILED", err)
		return
	}
	clearRefreshCookie(writer)
	writeJSON(writer, http.StatusOK, report)
}

func (server *Server) handleMe(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	writeJSON(writer, http.StatusOK, principal)
}

// issueSession mints an access/refresh token pair and persists the session.
// refreshTTL is the session's full refresh lifetime: rotation never extends
// past it. Only the peppered digests of both tokens are stored, so a database
// leak does not leak usable tokens.
func (server *Server) issueSession(ctx context.Context, userID string, refreshTTL time.Duration, request *http.Request) (SessionTokens, error) {
	accessToken, _, err := newToken(accessTokenPrefix)
	if err != nil {
		return SessionTokens{}, err
	}
	refreshToken, _, err := newToken(refreshTokenPrefix)
	if err != nil {
		return SessionTokens{}, err
	}
	sessionID, err := model.NewID()
	if err != nil {
		return SessionTokens{}, err
	}
	now := time.Now().UTC()
	accessExpires := now.Add(server.config.AccessTokenTTL)
	refreshExpires := now.Add(refreshTTL)
	if err := server.database.CreateSession(ctx, sessionID, userID, tokenDigest(server.config.TokenPepper, accessToken), tokenDigest(server.config.TokenPepper, refreshToken), accessExpires, refreshExpires, request.UserAgent(), request.RemoteAddr); err != nil {
		return SessionTokens{}, err
	}
	return SessionTokens{SessionID: sessionID, AccessToken: accessToken, RefreshToken: refreshToken, TokenType: "Bearer", ExpiresAt: accessExpires}, nil
}

// allowCredentialAttempt applies the per-client+account throttle on top of the
// global request limiter. Credential endpoints get their own tight budget
// keyed by both the derived client IP and the account, so neither one shared
// NAT address nor one targeted account can be brute-forced through the
// general API budget.
func (server *Server) allowCredentialAttempt(request *http.Request, keyMaterial string) bool {
	digest := tokenDigest("login-throttle", strings.ToLower(strings.TrimSpace(keyMaterial)))
	return server.loginLimiter.Allow("login:" + server.clientIP(request) + ":" + fmt.Sprintf("%x", digest[:8]))
}

// allowRegistrationAttempt throttles account creation per derived client IP.
// The login throttle's per-account limb cannot apply — there is no account yet
// — and keying on the submitted email would mint one throttle bucket per
// address, letting a single client flood registrations without limit.
func (server *Server) allowRegistrationAttempt(request *http.Request) bool {
	return server.loginLimiter.Allow("register:" + server.clientIP(request))
}
