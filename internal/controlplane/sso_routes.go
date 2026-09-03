package controlplane

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/oidc"
)

// sso_routes.go: the OIDC single sign-on flow. The CLI (or the desktop app)
// starts a loopback listener, asks this control plane for an authorization
// URL, sends the browser there, receives the code on its loopback, and
// redeems the code here. The control plane holds the nonce inside a signed
// state blob — no server-side session state, so any replica can finish the
// flow.

// ssoAuthorizeRequest starts the flow.
type ssoAuthorizeRequest struct {
	Email         string `json:"email"`
	RedirectURI   string `json:"redirect_uri"`
	CodeChallenge string `json:"code_challenge"`
}

// ssoAuthorizeResponse is what the client needs to open the browser.
type ssoAuthorizeResponse struct {
	AuthorizationURL string `json:"authorization_url"`
	State            string `json:"state"`
}

// ssoTokenRequest redeems the code the loopback received. The redirect URI
// must be sent again because the issuer checks it byte-for-byte against the
// one that started the flow.
type ssoTokenRequest struct {
	Code         string `json:"code"`
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	RedirectURI  string `json:"redirect_uri"`
}

// ssoState is the signed blob that travels to the issuer and back as the
// OAuth state parameter.
type ssoState struct {
	Email  string    `json:"email"`
	Nonce  string    `json:"nonce"`
	Expiry time.Time `json:"expiry"`
}

// ssoProvider builds the OIDC client from configuration. A nil result means
// single sign-on is not configured. The outbound HTTP client is the default
// one unless a test has installed its own.
func (server *Server) ssoProvider() (*oidc.Provider, error) {
	if !server.config.OIDC.Configured() {
		return nil, errors.New("single sign-on is not configured on this control plane")
	}
	client := server.oidcHTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return oidc.New(server.config.OIDC.Issuer, server.config.OIDC.ClientID, server.config.OIDC.ClientSecret, client)
}

// handleSSOAuthorize mints the browser URL for a federated login. The
// redirect URI must be a loopback http URL — this flow deliberately never
// redirects tokens to a remote host.
func (server *Server) handleSSOAuthorize(writer http.ResponseWriter, request *http.Request) {
	provider, err := server.ssoProvider()
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED", err.Error())
		return
	}
	var input ssoAuthorizeRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	email, err := normalizeEmail(input.Email)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "EMAIL_INVALID", err.Error())
		return
	}
	if !isLoopbackRedirect(input.RedirectURI) {
		writeError(writer, http.StatusBadRequest, "REDIRECT_INVALID", "redirect_uri must be a loopback http URL (127.0.0.1, localhost, or ::1)")
		return
	}
	if len(input.CodeChallenge) < 43 || len(input.CodeChallenge) > 128 {
		writeError(writer, http.StatusBadRequest, "CODE_CHALLENGE_INVALID", "code_challenge must be the S256 challenge of the client's verifier")
		return
	}
	state, nonce, err := server.mintSSOState(email)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "SSO_STATE_FAILED", err.Error())
		return
	}
	authorizationURL, err := provider.AuthorizationURL(request.Context(), input.RedirectURI, state, nonce, input.CodeChallenge)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "SSO_ISSUER_UNREACHABLE", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, ssoAuthorizeResponse{AuthorizationURL: authorizationURL, State: state})
}

// handleSSOToken finishes the flow: exchange the code, validate the ID token,
// link or provision the local account, and issue a session.
func (server *Server) handleSSOToken(writer http.ResponseWriter, request *http.Request) {
	provider, err := server.ssoProvider()
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED", err.Error())
		return
	}
	var input ssoTokenRequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	email, nonce, err := server.verifySSOState(input.State)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "SSO_STATE_INVALID", err.Error())
		return
	}
	if len(input.Code) < 16 {
		writeError(writer, http.StatusBadRequest, "SSO_CODE_INVALID", "the authorization code is missing or malformed")
		return
	}
	if len(input.CodeVerifier) < 43 || len(input.CodeVerifier) > 128 {
		writeError(writer, http.StatusBadRequest, "SSO_VERIFIER_INVALID", "code_verifier must be the PKCE verifier this flow started with")
		return
	}
	if !isLoopbackRedirect(input.RedirectURI) {
		writeError(writer, http.StatusBadRequest, "REDIRECT_INVALID", "redirect_uri must be a loopback http URL (127.0.0.1, localhost, or ::1)")
		return
	}
	tokens, err := provider.ExchangeCode(request.Context(), input.Code, input.RedirectURI, input.CodeVerifier)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "SSO_EXCHANGE_FAILED", err.Error())
		return
	}
	claims, err := provider.ValidateIDToken(request.Context(), tokens.IDToken, nonce)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "SSO_TOKEN_INVALID", err.Error())
		return
	}
	if claims.Email == "" || !claims.EmailVerified {
		writeError(writer, http.StatusUnauthorized, "SSO_EMAIL_UNVERIFIED", "the issuer must return a verified email claim")
		return
	}
	if email != "" && !strings.EqualFold(claims.Email, email) {
		writeError(writer, http.StatusUnauthorized, "SSO_EMAIL_MISMATCH", "the token authenticates a different address than the one that started this flow")
		return
	}
	user, err := server.database.FederateSSOLink(request.Context(), provider.Issuer(), claims.Subject, claims.Email, claims.Name, server.auditInput(request, "user.sso_login", "user", "", map[string]any{"issuer": provider.Issuer()}))
	if err != nil {
		if errors.Is(err, database.ErrUserDisabled) {
			writeError(writer, http.StatusForbidden, "ACCOUNT_DISABLED", "this account is disabled")
			return
		}
		if errors.Is(err, database.ErrSSOAccountLinked) {
			writeError(writer, http.StatusConflict, "ACCOUNT_ALREADY_FEDERATED", err.Error())
			return
		}
		writeError(writer, http.StatusInternalServerError, "SSO_LINK_FAILED", err.Error())
		return
	}
	sessionTokens, err := server.issueSession(request.Context(), user.ID, request)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "SESSION_CREATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"user": userResponse(user), "tokens": sessionTokens})
}

// mintSSOState signs the state blob that binds this authorization to one
// email and one nonce for ten minutes. Signing with the token pepper means a
// forged state is as hard to produce as a session token.
func (server *Server) mintSSOState(email string) (state, nonce string, err error) {
	nonceBytes := make([]byte, 24)
	if _, err = rand.Read(nonceBytes); err != nil {
		return "", "", fmt.Errorf("generate SSO nonce: %w", err)
	}
	nonce = base64.RawURLEncoding.EncodeToString(nonceBytes)
	blob, err := json.Marshal(ssoState{Email: email, Nonce: nonce, Expiry: time.Now().UTC().Add(10 * time.Minute)})
	if err != nil {
		return "", "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(blob)
	signature := base64.RawURLEncoding.EncodeToString(hmacSHA256([]byte(server.config.TokenPepper), []byte(encoded)))
	return encoded + "." + signature, nonce, nil
}

// verifySSOState checks the signature and expiry and returns the email and
// nonce the state was minted with.
func (server *Server) verifySSOState(state string) (email, nonce string, err error) {
	encoded, signature, ok := strings.Cut(state, ".")
	if !ok || encoded == "" || signature == "" {
		return "", "", errors.New("state is malformed")
	}
	expected := base64.RawURLEncoding.EncodeToString(hmacSHA256([]byte(server.config.TokenPepper), []byte(encoded)))
	if !secureCompareString(signature, expected) {
		return "", "", errors.New("state signature is invalid")
	}
	blob, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", errors.New("state is malformed")
	}
	var parsed ssoState
	if err := json.Unmarshal(blob, &parsed); err != nil {
		return "", "", errors.New("state is malformed")
	}
	if time.Now().UTC().After(parsed.Expiry) {
		return "", "", errors.New("state has expired; start the login again")
	}
	return parsed.Email, parsed.Nonce, nil
}

// isLoopbackRedirect accepts exactly the native-application pattern from RFC
// 8252: plain http to the loopback interface, any port, nothing else. A
// remote redirect target would receive authorization codes in the clear.
func isLoopbackRedirect(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	if parsed.Port() == "" && parsed.Host != "" && !strings.Contains(parsed.Host, ":") {
		// A loopback URL without an explicit port still names a host.
		host = parsed.Host
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
}

// handleSSORequired reports whether an email's domain is under SSO
// enforcement, so clients can route to the right login before asking for a
// password that cannot succeed.
func (server *Server) handleSSORequired(writer http.ResponseWriter, request *http.Request) {
	if !server.config.OIDC.Configured() {
		writeJSON(writer, http.StatusOK, map[string]any{"required": false})
		return
	}
	email := strings.TrimSpace(request.URL.Query().Get("email"))
	domain := database.EmailDomain(email)
	if domain == "" {
		writeError(writer, http.StatusBadRequest, "EMAIL_INVALID", "an email query parameter is required")
		return
	}
	enforced, err := server.database.SSOEnforcedForEmail(request.Context(), email)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "SSO_LOOKUP_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"required": enforced})
}

// SetOrganizationSSORequest is the admin surface for enforcement. The domain
// is required exactly when enforcement is on.
type SetOrganizationSSORequest struct {
	Enforced    bool   `json:"enforced"`
	EmailDomain string `json:"email_domain"`
}

// handleOrganizationSSOGet reads the organization's enforcement state.
func (server *Server) handleOrganizationSSOGet(writer http.ResponseWriter, request *http.Request) {
	enforced, emailDomain, err := server.database.OrganizationSSO(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "organization was not found")
			return
		}
		writeError(writer, http.StatusInternalServerError, "SSO_LOOKUP_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, SetOrganizationSSORequest{Enforced: enforced, EmailDomain: emailDomain})
}

// handleOrganizationSSOSet turns enforcement on or off. Turning it on claims
// an email domain for this organization: from that moment the domain's users
// authenticate through the configured issuer.
func (server *Server) handleOrganizationSSOSet(writer http.ResponseWriter, request *http.Request) {
	if !server.config.OIDC.Configured() {
		writeError(writer, http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED", "the control plane has no OIDC issuer configured")
		return
	}
	var input SetOrganizationSSORequest
	if !decodeJSON(writer, request, &input) {
		return
	}
	emailDomain := strings.ToLower(strings.TrimSpace(input.EmailDomain))
	if input.Enforced && !validEmailDomain(emailDomain) {
		writeError(writer, http.StatusBadRequest, "SSO_DOMAIN_INVALID", "a valid email domain is required to enforce SSO")
		return
	}
	if !input.Enforced {
		emailDomain = ""
	}
	organizationID := request.PathValue("organizationID")
	if err := server.database.SetOrganizationSSO(request.Context(), organizationID, emailDomain, input.Enforced, server.auditInput(request, "organization.sso", "organization", organizationID, map[string]any{"enforced": input.Enforced, "email_domain": emailDomain})); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "organization was not found")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "SSO_DOMAIN_CLAIMED", "another organization already enforces SSO for this email domain")
			return
		}
		writeError(writer, http.StatusInternalServerError, "SSO_UPDATE_FAILED", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, SetOrganizationSSORequest{Enforced: input.Enforced, EmailDomain: emailDomain})
}

// validEmailDomain accepts a bare domain like example.com — letters, digits,
// hyphens, dots, at least one dot, and no mailbox part.
func validEmailDomain(domain string) bool {
	if len(domain) < 4 || len(domain) > 253 || strings.Contains(domain, "@") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-') {
				return false
			}
		}
	}
	return true
}
