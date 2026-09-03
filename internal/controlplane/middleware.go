package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
)

const (
	accessTokenPrefix  = "shift_at_"
	refreshTokenPrefix = "shift_rt_"
	apiKeyPrefix       = "shift_ak_"
)

type contextKey string

const principalContextKey contextKey = "shift-control-principal"

// requireAuth runs the wrapped handler only for an authenticated principal: a
// session access token or an organization API key.
func (server *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		principal, err := server.authenticate(request)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", err.Error())
			return
		}
		next(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey, principal)))
	}
}

// requireOrganization layers membership, role, and API-key scope checks on top
// of requireAuth. API keys are pinned to their own organization; user
// principals must be members with a role that allows the action.
func (server *Server) requireOrganization(required Role, scope string, next http.HandlerFunc) http.HandlerFunc {
	return server.requireAuth(func(writer http.ResponseWriter, request *http.Request) {
		principal := principalFrom(request.Context())
		organizationID := request.PathValue("organizationID")
		if organizationID == "" {
			writeError(writer, http.StatusBadRequest, "ORGANIZATION_REQUIRED", "organization id is required")
			return
		}
		if principal.APIKeyID != "" && principal.OrganizationID != organizationID {
			writeError(writer, http.StatusForbidden, "ORGANIZATION_FORBIDDEN", "the API key belongs to another organization")
			return
		}
		role, err := server.database.MemberRole(request.Context(), organizationID, principal.UserID)
		if err != nil {
			if database.IsNotFound(err) {
				writeError(writer, http.StatusForbidden, "ORGANIZATION_FORBIDDEN", "you are not a member of this organization")
				return
			}
			writeError(writer, http.StatusInternalServerError, "AUTHORIZATION_LOOKUP_FAILED", err.Error())
			return
		}
		parsedRole := Role(role)
		if !parsedRole.Allows(required) {
			writeError(writer, http.StatusForbidden, "INSUFFICIENT_ROLE", "your organization role cannot perform this action")
			return
		}
		if principal.APIKeyID != "" && !scopeAllows(principal.Scopes, required, scope) {
			writeError(writer, http.StatusForbidden, "API_KEY_SCOPE_FORBIDDEN", "the API key scope cannot perform this action")
			return
		}
		principal.OrganizationID = organizationID
		principal.Role = parsedRole
		next(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey, principal)))
	})
}

// authenticate resolves the bearer token to a principal. API keys and access
// tokens are both random secrets; only their peppered digests are stored, so
// authentication is a database lookup on the digest.
func (server *Server) authenticate(request *http.Request) (Principal, error) {
	token, ok := parseBearer(request.Header.Get("Authorization"))
	if !ok {
		return Principal{}, errors.New("a bearer access token is required")
	}
	now := time.Now().UTC()
	if strings.HasPrefix(token, apiKeyPrefix) {
		record, err := server.database.AuthenticateAPIKey(request.Context(), tokenDigest(server.config.TokenPepper, token), now)
		if err != nil {
			return Principal{}, errors.New("API key is invalid, revoked, or expired")
		}
		var scopes []string
		_ = json.Unmarshal(record.Scopes, &scopes)
		return Principal{UserID: record.UserID, OrganizationID: record.OrganizationID, APIKeyID: record.ID, Scopes: scopes}, nil
	}
	if !strings.HasPrefix(token, accessTokenPrefix) {
		return Principal{}, errors.New("access token has an unsupported type")
	}
	record, err := server.database.AuthenticateAccess(request.Context(), tokenDigest(server.config.TokenPepper, token), now)
	if err != nil {
		return Principal{}, errors.New("access token is invalid or expired")
	}
	return Principal{SessionID: record.ID, UserID: record.UserID, Email: record.Email, DisplayName: record.DisplayName}, nil
}

func principalFrom(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalContextKey).(Principal)
	return principal
}

// An API-key scope grants a role, optionally narrowed to one resource family.
// The role-granting scopes ("read", "operate", "admin") span every resource;
// the named scopes only act on their own family.
var supportedScopes = map[string]scopeGrant{
	"read":        {Role: RoleViewer},
	"operate":     {Role: RoleOperator},
	"admin":       {Role: RoleAdmin},
	"machines":    {Role: RoleOperator, Resource: "machines"},
	"workloads":   {Role: RoleOperator, Resource: "workloads"},
	"migrations":  {Role: RoleOperator, Resource: "migrations"},
	"checkpoints": {Role: RoleOperator, Resource: "checkpoints"},
	"compute":     {Role: RoleOperator, Resource: "compute"},
	"scim":        {Role: RoleAdmin, Resource: "scim"},
}

type scopeGrant struct {
	Role     Role
	Resource string
}

// requireSCIM authenticates a SCIM provisioning request. Provisioning is a
// machine-to-machine surface: only API keys carrying the "scim" scope may call
// it, and the key's own organization is the tenant. A user session token — even
// an administrator's — cannot provision accounts, so a compromised browser
// session never reaches the directory.
func (server *Server) requireSCIM(next http.HandlerFunc) http.HandlerFunc {
	return server.requireAuth(func(writer http.ResponseWriter, request *http.Request) {
		principal := principalFrom(request.Context())
		if principal.APIKeyID == "" {
			writeError(writer, http.StatusForbidden, "SCIM_API_KEY_REQUIRED", "SCIM provisioning requires an API key with the scim scope")
			return
		}
		if !scopeAllows(principal.Scopes, RoleAdmin, "scim") {
			writeError(writer, http.StatusForbidden, "API_KEY_SCOPE_FORBIDDEN", "the API key scope cannot perform SCIM provisioning")
			return
		}
		next(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey, principal)))
	})
}

func validScopes(scopes []string) bool {
	if len(scopes) == 0 || len(scopes) > 16 {
		return false
	}
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		if _, ok := supportedScopes[scope]; !ok || seen[scope] {
			return false
		}
		seen[scope] = true
	}
	return true
}

func scopeAllows(scopes []string, required Role, resource string) bool {
	for _, scope := range scopes {
		grant, ok := supportedScopes[scope]
		if !ok || !grant.Role.Allows(required) {
			continue
		}
		if grant.Resource == "" || grant.Resource == resource {
			return true
		}
	}
	return false
}
