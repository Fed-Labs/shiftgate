package controlplane

import (
	"context"
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
	email, err := normalizeEmail(input.Email)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "EMAIL_INVALID", err.Error())
		return
	}
	// Domains under SSO enforcement do not take self-service registrations:
	// those accounts arrive through the issuer or the provisioning system.
	if server.config.OIDC.Configured() {
		if enforced, err := server.database.SSOEnforcedForEmail(request.Context(), email); err == nil && enforced {
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
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	organizationID, err := model.NewID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	user, organization, err := server.database.Register(request.Context(), userID, email, passwordHash, displayName, organizationID, organizationName, server.auditInput(request, "user.register", "user", userID, nil))
	if err != nil {
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "EMAIL_EXISTS", "an account with this email already exists")
			return
		}
		writeError(writer, http.StatusInternalServerError, "REGISTRATION_FAILED", err.Error())
		return
	}
	tokens, err := server.issueSession(request.Context(), user.ID, request)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "SESSION_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"user": userResponse(user), "organization": organizationResponse(organization), "tokens": tokens})
}

func (server *Server) handleLogin(writer http.ResponseWriter, request *http.Request) {
	var input LoginRequest
	if !decodeJSON(writer, request, &input) {
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
	// not depend on whether the account exists.
	if server.config.OIDC.Configured() {
		if enforced, err := server.database.SSOEnforcedForEmail(request.Context(), email); err == nil && enforced {
			writeError(writer, http.StatusForbidden, "SSO_REQUIRED", "this email domain requires single sign-on; use 'shift login --sso'")
			return
		}
	}
	user, err := server.database.UserByEmail(request.Context(), email)
	if err != nil || user.DisabledAt != nil || !verifyPassword(input.Password, user.PasswordHash, server.config.PasswordPepper) {
		writeError(writer, http.StatusUnauthorized, "INVALID_CREDENTIALS", "email or password is incorrect")
		return
	}
	tokens, err := server.issueSession(request.Context(), user.ID, request)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "SESSION_CREATE_FAILED", err.Error())
		return
	}
	audit := server.auditInput(request, "user.login", "user", user.ID, nil)
	audit.ActorUserID = user.ID
	_ = server.database.RecordAudit(request.Context(), audit)
	writeJSON(writer, http.StatusOK, map[string]any{"user": userResponse(user), "tokens": tokens})
}

func (server *Server) handleRefresh(writer http.ResponseWriter, request *http.Request) {
	var input RefreshRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	if len(input.RefreshToken) < len(refreshTokenPrefix)+20 {
		writeError(writer, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "refresh token is invalid or expired")
		return
	}
	accessToken, _, err := newToken(accessTokenPrefix)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "TOKEN_GENERATION_FAILED", err.Error())
		return
	}
	refreshToken, _, err := newToken(refreshTokenPrefix)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "TOKEN_GENERATION_FAILED", err.Error())
		return
	}
	now := time.Now().UTC()
	accessExpires := now.Add(server.config.AccessTokenTTL)
	refreshExpires := now.Add(server.config.RefreshTokenTTL)
	record, err := server.database.RotateSession(request.Context(), tokenDigest(server.config.TokenPepper, input.RefreshToken), tokenDigest(server.config.TokenPepper, accessToken), tokenDigest(server.config.TokenPepper, refreshToken), accessExpires, refreshExpires, now)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "INVALID_REFRESH_TOKEN", "refresh token is invalid or expired")
		return
	}
	writeJSON(writer, http.StatusOK, SessionTokens{SessionID: record.ID, AccessToken: accessToken, RefreshToken: refreshToken, TokenType: "Bearer", ExpiresAt: accessExpires})
}

func (server *Server) handleLogout(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	if principal.SessionID == "" {
		writeError(writer, http.StatusBadRequest, "SESSION_TOKEN_REQUIRED", "logout requires a user access token")
		return
	}
	if err := server.database.RevokeSession(request.Context(), principal.SessionID, principal.UserID); err != nil && !database.IsNotFound(err) {
		writeError(writer, http.StatusInternalServerError, "LOGOUT_FAILED", err.Error())
		return
	}
	_ = server.database.RecordAudit(request.Context(), server.auditInput(request, "user.logout", "session", principal.SessionID, nil))
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) handleMe(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request.Context())
	writeJSON(writer, http.StatusOK, principal)
}

// issueSession mints an access/refresh token pair and persists the session.
// Only the peppered digests of both tokens are stored, so a database leak does
// not leak usable tokens.
func (server *Server) issueSession(ctx context.Context, userID string, request *http.Request) (SessionTokens, error) {
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
	refreshExpires := now.Add(server.config.RefreshTokenTTL)
	if err := server.database.CreateSession(ctx, sessionID, userID, tokenDigest(server.config.TokenPepper, accessToken), tokenDigest(server.config.TokenPepper, refreshToken), accessExpires, refreshExpires, request.UserAgent(), request.RemoteAddr); err != nil {
		return SessionTokens{}, err
	}
	return SessionTokens{SessionID: sessionID, AccessToken: accessToken, RefreshToken: refreshToken, TokenType: "Bearer", ExpiresAt: accessExpires}, nil
}
