package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/database"
)

const (
	accessTokenPrefix  = "shift_at_"
	refreshTokenPrefix = "shift_rt_"
	apiKeyPrefix       = "shift_ak_"
	githubCodePrefix   = "shift_ghc_"
)

type contextKey string

const principalContextKey contextKey = "shift-control-principal"

// requireAuth runs the wrapped handler only for an authenticated principal: a
// session access token or an organization API key. Platform-admin paths carry
// one extra step: the authentication gate itself refuses every principal that
// is not a platform operator (see platformAdmin), so the admin surfaces stay
// closed even where a route is registered with bare requireAuth.
func (server *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		principal, err := server.authenticate(request)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "AUTHENTICATION_REQUIRED", err.Error())
			return
		}
		if isPlatformAdminPath(request.URL.Path) && !server.platformAdmin(request, principal) {
			writeError(writer, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "platform admin endpoints require an API key with the admin scope or the metrics scrape token")
			return
		}
		next(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey, principal)))
	}
}

// requirePlatformAdmin gates the platform-wide admin surfaces (the weekly
// scoreboard) on an actual operator credential instead of bare authentication:
// an API key carrying the broad "admin" scope, or the same metrics scrape
// token /metrics accepts. A session token — any tenant user's, whatever its
// organization role — is refused, so no customer account can read
// platform-wide business metrics. requireAuth enforces the same predicate for
// /v1/admin/ paths, so the surface stays closed regardless of how the route
// is registered.
func (server *Server) requirePlatformAdmin(next http.HandlerFunc) http.HandlerFunc {
	return server.requireAuth(func(writer http.ResponseWriter, request *http.Request) {
		if !server.platformAdmin(request, principalFrom(request.Context())) {
			writeError(writer, http.StatusForbidden, "ADMIN_SCOPE_REQUIRED", "platform admin endpoints require an API key with the admin scope or the metrics scrape token")
			return
		}
		next(writer, request)
	})
}

// isPlatformAdminPath reports whether a request targets the platform-admin
// surface family. Only the weekly scoreboard lives there today; the prefix
// check keeps future admin routes closed until they name their own gate.
func isPlatformAdminPath(path string) bool {
	return path == "/v1/admin" || strings.HasPrefix(path, "/v1/admin/")
}

// platformAdmin reports whether the principal may read platform-admin
// surfaces. The admin API-key scope is the documented contract ("Authenticated
// (admin scope)"); the metrics scrape token is the operator shared secret
// /metrics already uses, honored only when this deployment configures one.
func (server *Server) platformAdmin(request *http.Request, principal Principal) bool {
	if principal.APIKeyID != "" && scopeAllows(principal.Scopes, RoleAdmin, "administration") {
		return true
	}
	return server.presentedMetricsToken(request)
}

// presentedMetricsToken reports whether the request carries this deployment's
// metrics scrape token. An unconfigured token never matches: the scoreboard
// must not open just because no scrape secret was set.
func (server *Server) presentedMetricsToken(request *http.Request) bool {
	expected := strings.TrimSpace(server.config.MetricsToken)
	if expected == "" {
		return false
	}
	presented := strings.TrimSpace(request.Header.Get("X-Metrics-Token"))
	if token, ok := parseBearer(request.Header.Get("Authorization")); ok {
		presented = token
	}
	return secureCompareString(presented, expected)
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
			server.writeInternalError(writer, request, http.StatusInternalServerError, "AUTHORIZATION_LOOKUP_FAILED", err)
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
		if !server.consumeAPIBudget(writer, request, organizationID) {
			return
		}
		next(writer, request.WithContext(context.WithValue(request.Context(), principalContextKey, principal)))
	})
}

// machineToMachinePath marks the presence and agent-command endpoints the
// fleet itself calls. They are exempt from the per-plan API budget (plan 2
// BR-11): charging the fleet for keeping itself alive would self-DoS it.
func machineToMachinePath(path string) bool {
	return strings.Contains(path, "/heartbeat") || strings.Contains(path, "/commands/poll") || strings.Contains(path, "/commands/")
}

// consumeAPIBudget enforces the per-plan API request budgets (plan 2 BR-11,
// plan 4 BR-07) and answers 429 RATE_LIMIT_UPGRADE with the upgrade CTA when
// exhausted. Machine-to-machine presence paths are exempt. Returns false
// after writing the refusal.
func (server *Server) consumeAPIBudget(writer http.ResponseWriter, request *http.Request, organizationID string) bool {
	if machineToMachinePath(request.URL.Path) {
		return true
	}
	entitlement, err := server.database.Entitlement(request.Context(), organizationID)
	if err != nil {
		// No entitlement row yet (fresh org): the free-tier default governs.
		if !server.apiBudgets.Allow(organizationID, 1000, 600) {
			writer.Header().Set("Retry-After", "3600")
			writeError(writer, http.StatusTooManyRequests, billing.RateLimitUpgradeCode, billing.RateLimitUpgradeMessage)
			return false
		}
		return true
	}
	plan, ok := billing.FindPlan(entitlement.Plan)
	if !ok {
		return true
	}
	if entitlement.TrialActive() && entitlement.Active() {
		if business, found := billing.FindPlan("business"); found {
			plan = business
		}
	}
	if !server.apiBudgets.Allow(organizationID, plan.MaxAPIRequestsPerDay, plan.MaxAPIRequestsPerHour) {
		writer.Header().Set("Retry-After", "3600")
		writeJSON(writer, http.StatusTooManyRequests, map[string]any{
			"code":        billing.RateLimitUpgradeCode,
			"message":     billing.RateLimitUpgradeMessage,
			"upgrade_url": upgradeURL(organizationID, billing.RateLimitUpgradeCode),
		})
		return false
	}
	return true
}

// securityHeaders stamps the baseline hardening headers on every response,
// including the CORS short-circuits (preflight answers and origin refusals),
// because those are exactly the responses a hostile page can provoke. The API
// serves JSON to browsers and machines alike: nosniff keeps a mislabeled body
// from being reinterpreted as script or HTML, DENY frames nothing anywhere,
// and no-referrer keeps authenticated URLs out of outbound Referer headers.
// Handlers may override Cache-Control where caching is deliberate (the public
// stats page sets its own); everything else defaults to no-store so an
// authenticated response is never left in a shared cache. HSTS is emitted
// only when the client actually arrived over TLS — directly or through the
// TLS-terminating edge's X-Forwarded-Proto — because browsers ignore it on
// plaintext anyway and a plaintext deployment must not pin hosts it cannot
// serve over https.
func (server *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		if request.Method != http.MethodOptions {
			header.Set("Cache-Control", "no-store")
		}
		if requestIsTLS(request) {
			header.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(writer, request)
	})
}

// requestIsTLS reports whether the request traveled over TLS: a direct TLS
// socket, or plaintext from the trusted edge that terminated TLS and says so
// in X-Forwarded-Proto (the platform's proxy). A client forging the header gains
// nothing: an HSTS header received over plaintext is ignored by every
// browser.
func requestIsTLS(request *http.Request) bool {
	if request.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(request.Header.Get("X-Forwarded-Proto")), "https")
}

// cors lets the browser dashboard call the API from its own origin. The CLI,
// agents, and anything else that sends no Origin header pass straight through
// untouched; a browser origin is answered only when it is listed in the
// control plane's allowed_origins, and the header echoes that exact origin —
// never a wildcard, so no other site can read an authenticated response. A
// request from an unlisted origin is served normally but without any CORS
// header, which the browser blocks client-side; the same goes for its
// preflight, which falls through to the router's regular response.
func (server *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if len(server.config.AllowedOrigins) > 0 {
			// Responses differ by requesting origin; without this, a cache could
			// hand one origin's allowed response to another.
			writer.Header().Add("Vary", "Origin")
		}
		origin := request.Header.Get("Origin")
		if origin == "" || !server.originAllowed(origin) {
			if origin != "" && len(server.config.AllowedOrigins) > 0 && isCredentialCookiePath(request.URL.Path) {
				// Defense in depth beyond SameSite=Strict: when this deployment
				// lists allowed origins, a cookie-authenticated mutation from
				// any other origin is refused outright, never merely answered
				// without CORS headers. With no origin list configured (CLI-
				// only or same-origin proxy deployments) the old behavior
				// stands.
				writeError(writer, http.StatusForbidden, "ORIGIN_FORBIDDEN", "request origin is not allowed for cookie authentication")
				return
			}
			next.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Access-Control-Allow-Origin", origin)
		if isCredentialCookiePath(request.URL.Path) {
			// The browser dashboard's cookie session rides these auth routes:
			// credentialed CORS requires an explicit allow (never a wildcard,
			// which the exact origin echo above already guarantees).
			writer.Header().Set("Access-Control-Allow-Credentials", "true")
		}
		if request.Method == http.MethodOptions {
			writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			writer.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Request-ID")
			writer.Header().Set("Access-Control-Max-Age", "600")
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// isCredentialCookiePath reports whether a request rides the httpOnly
// refresh-cookie session surface. Those responses — and only those — carry
// Access-Control-Allow-Credentials, which a browser requires before it will
// deliver or accept Set-Cookie on a cross-origin fetch made with
// credentials:"include" (the web dashboard's transport for exactly these
// routes). The list is every route that sets, rotates, or clears the shift_rt
// cookie plus the account-erasure mutation that destroys the session behind
// it: register issues the signup session's cookie, sso/authorize and
// sso/token complete a browser sign-in, github/authorize and github/token do
// the same for "Sign in with GitHub" (the github/callback redirect is a
// top-level navigation, not a fetch, and needs no CORS grant), and
// login/refresh/logout are the core session surface — while
// DELETE /v1/users/me clears the cookie as it erases.
// A route missing here would answer a credentialed fetch with a CORS failure
// even though the server processed it — the browser rejects the response and
// the caller sees a network error instead of the body.
func isCredentialCookiePath(path string) bool {
	switch path {
	case "/v1/auth/register", "/v1/auth/login", "/v1/auth/refresh", "/v1/auth/logout",
		"/v1/auth/sso/authorize", "/v1/auth/sso/token",
		"/v1/auth/github/authorize", "/v1/auth/github/token", "/v1/users/me":
		return true
	}
	return false
}

// originAllowed reports whether origin exactly matches a configured origin.
// Origins compare as whole strings — no wildcard or suffix matching — so
// "http://localhost:3000" admits exactly that scheme, host, and port.
func (server *Server) originAllowed(origin string) bool {
	for _, allowed := range server.config.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

// authenticate resolves the bearer token to a principal. API keys and access
// tokens are both random secrets; only their peppered digests are stored, so
// authentication is a database lookup on the digest. A credential that
// resolves is re-marked on the request limiter, which gives it — and only it —
// a per-credential rate-limit bucket.
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
		server.markCredentialValidated(token)
		return Principal{UserID: record.UserID, OrganizationID: record.OrganizationID, APIKeyID: record.ID, Scopes: scopes}, nil
	}
	if !strings.HasPrefix(token, accessTokenPrefix) {
		return Principal{}, errors.New("access token has an unsupported type")
	}
	record, err := server.database.AuthenticateAccess(request.Context(), tokenDigest(server.config.TokenPepper, token), now)
	if err != nil {
		return Principal{}, errors.New("access token is invalid or expired")
	}
	server.markCredentialValidated(token)
	return Principal{SessionID: record.ID, UserID: record.UserID, Email: record.Email, DisplayName: record.DisplayName}, nil
}

// markCredentialValidated tells the request limiter that this bearer
// credential just authenticated, so its rate-limit bucket is keyed by the
// credential instead of the shared per-client-IP fallback.
func (server *Server) markCredentialValidated(token string) {
	if server.limiter != nil {
		server.limiter.MarkValidated(rateLimitCredentialID(token))
	}
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
		if !server.requirePlanFeature(writer, request, principal.OrganizationID, billing.FeatureSCIM) {
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
