package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/billing"
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

// ssoDefaultHTTPClient is the shared outbound client for issuer traffic when
// no test has installed its own.
var ssoDefaultHTTPClient = &http.Client{Timeout: 20 * time.Second}

// ssoProviderCache remembers the provider built for one configuration. The
// oidc.Provider is concurrency-safe and caches discovery and JWKS itself, so
// reusing it keeps login latency (and issuer outages) from multiplying per
// request; a changed configuration or outbound client rebuilds it.
var (
	ssoProviderCacheMutex sync.Mutex
	ssoProviderCacheKey   ssoProviderCacheEntry
	ssoProviderCacheValue *oidc.Provider
)

type ssoProviderCacheEntry struct {
	issuer       string
	clientID     string
	clientSecret string
	client       *http.Client
}

// ssoProviderFor builds the OIDC client for one configuration, reusing the
// cached one when the configuration is unchanged.
func ssoProviderFor(issuer, clientID, clientSecret string, client *http.Client) (*oidc.Provider, error) {
	key := ssoProviderCacheEntry{issuer: issuer, clientID: clientID, clientSecret: clientSecret, client: client}
	ssoProviderCacheMutex.Lock()
	defer ssoProviderCacheMutex.Unlock()
	if ssoProviderCacheValue != nil && ssoProviderCacheKey == key {
		return ssoProviderCacheValue, nil
	}
	provider, err := oidc.New(issuer, clientID, clientSecret, client)
	if err != nil {
		return nil, err
	}
	ssoProviderCacheKey = key
	ssoProviderCacheValue = provider
	return provider, nil
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
		client = ssoDefaultHTTPClient
	}
	return ssoProviderFor(server.config.OIDC.Issuer, server.config.OIDC.ClientID, server.config.OIDC.ClientSecret, client)
}

// handleSSOAuthorize mints the browser URL for a federated login. The
// redirect URI must be a loopback http URL — this flow deliberately never
// redirects tokens to a remote host.
func (server *Server) handleSSOAuthorize(writer http.ResponseWriter, request *http.Request) {
	provider, err := server.ssoProvider()
	if err != nil {
		// An unconfigured issuer is a deployment state, not a failure: the
		// answer is the typed 503 the contract documents (exactly like
		// billing's BILLING_NOT_CONFIGURED), never an "internal error" body
		// and an error-level log line per probe.
		writeError(writer, http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED", "single sign-on is not configured on this control plane")
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
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_STATE_FAILED", err)
		return
	}
	authorizationURL, err := provider.AuthorizationURL(request.Context(), input.RedirectURI, state, nonce, input.CodeChallenge)
	if err != nil {
		// The issuer could not be reached or refused the request: a stable
		// 502 message (contract: "The issuer could not be reached") with the
		// cause logged for operators, matching billing's CHECKOUT_FAILED.
		server.logger.Error("sso authorization url failed", "request_id", requestID(request), "error", err)
		writeError(writer, http.StatusBadGateway, "SSO_ISSUER_UNREACHABLE", "the identity issuer could not be reached")
		return
	}
	writeJSON(writer, http.StatusOK, ssoAuthorizeResponse{AuthorizationURL: authorizationURL, State: state})
}

// handleSSOToken finishes the flow: exchange the code, validate the ID token,
// link or provision the local account, and issue a session.
func (server *Server) handleSSOToken(writer http.ResponseWriter, request *http.Request) {
	provider, err := server.ssoProvider()
	if err != nil {
		// Deployment state, not a failure — typed like handleSSOAuthorize.
		writeError(writer, http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED", "single sign-on is not configured on this control plane")
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
		// A stable 502 message (contract: "The code exchange failed") with
		// the cause logged; the wrapped exchange error can carry issuer
		// response detail and never reaches the client.
		server.logger.Error("sso code exchange failed", "request_id", requestID(request), "error", err)
		writeError(writer, http.StatusBadGateway, "SSO_EXCHANGE_FAILED", "the authorization code exchange with the issuer failed")
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
	// An existing password account is never claimed on the email assertion
	// alone: an issuer (or its admin) asserting the address must not silently
	// take over an un-federated account. The link is only allowed once some
	// organization enforces SSO for the domain, which declares the issuer the
	// authority for that address space.
	if existing, lookupErr := server.database.UserByEmail(request.Context(), claims.Email); lookupErr == nil {
		if existing.PasswordHash != "" && existing.SSOIssuer == "" {
			enforced, enforceErr := server.database.SSOEnforcedForEmail(request.Context(), claims.Email)
			if enforceErr != nil {
				server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_LINK_FAILED", enforceErr)
				return
			}
			if !enforced {
				writeError(writer, http.StatusConflict, "ACCOUNT_LINK_CONFLICT", "an account with this address already exists with a password; single sign-on may only claim it once an organization enforces SSO for the address's domain")
				return
			}
		}
	} else if !database.IsNotFound(lookupErr) {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_LINK_FAILED", lookupErr)
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
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_LINK_FAILED", err)
		return
	}
	// An issuer-asserted login is not a shared-terminal sign-in, so it issues
	// the full remembered refresh lifetime.
	sessionTokens, err := server.issueSession(request.Context(), user.ID, server.config.RefreshTokenTTL, request)
	if err != nil {
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SESSION_CREATE_FAILED", err)
		return
	}
	// Browser sessions ride the httpOnly refresh cookie from the first
	// response, exactly as on login and register: the rotating token leaves
	// the body entirely, so a federated signup session also survives reloads
	// without ever exposing the refresh credential to script. Non-browser
	// callers (the CLI's loopback flow) keep the body token as before.
	if browserSession(request, false) {
		setRefreshCookie(writer, sessionTokens.RefreshToken, server.config.RefreshTokenTTL)
		sessionTokens.RefreshToken = ""
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
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_LOOKUP_FAILED", err)
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
	if !server.requirePlanFeature(writer, request, request.PathValue("organizationID"), billing.FeatureSSO) {
		return
	}
	enforced, emailDomain, err := server.database.OrganizationSSO(request.Context(), request.PathValue("organizationID"))
	if err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "organization was not found")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_LOOKUP_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, SetOrganizationSSORequest{Enforced: enforced, EmailDomain: emailDomain})
}

// handleOrganizationSSOSet turns enforcement on or off. Turning it on claims
// an email domain for this organization: from that moment the domain's users
// authenticate through the configured issuer. A claim is only honored after
// the organization proves control of the domain with a DNS TXT record — the
// claim locks every address on the domain out of password login and auto-joins
// its users to this organization, so syntactic possession of a name is not
// ownership.
func (server *Server) handleOrganizationSSOSet(writer http.ResponseWriter, request *http.Request) {
	if !server.config.OIDC.Configured() {
		writeError(writer, http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED", "the control plane has no OIDC issuer configured")
		return
	}
	organizationID := request.PathValue("organizationID")
	if !server.requirePlanFeature(writer, request, organizationID, billing.FeatureSSO) {
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
	if input.Enforced && ssoDomainIsPublic(emailDomain) {
		writeError(writer, http.StatusBadRequest, "SSO_DOMAIN_NOT_CLAIMABLE", "public email providers cannot be claimed for organization SSO enforcement")
		return
	}
	if input.Enforced {
		if err := verifySSODomainOwnership(request.Context(), organizationID, emailDomain); err != nil {
			writeError(writer, http.StatusBadRequest, "SSO_DOMAIN_UNVERIFIED", err.Error())
			return
		}
	}
	if !input.Enforced {
		emailDomain = ""
	}
	if err := server.database.SetOrganizationSSO(request.Context(), organizationID, emailDomain, input.Enforced, server.auditInput(request, "organization.sso", "organization", organizationID, map[string]any{"enforced": input.Enforced, "email_domain": emailDomain})); err != nil {
		if database.IsNotFound(err) {
			writeError(writer, http.StatusNotFound, "ORGANIZATION_NOT_FOUND", "organization was not found")
			return
		}
		if database.IsConflict(err) {
			writeError(writer, http.StatusConflict, "SSO_DOMAIN_CLAIMED", "another organization already enforces SSO for this email domain")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "SSO_UPDATE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, SetOrganizationSSORequest{Enforced: input.Enforced, EmailDomain: emailDomain})
}

// ssoDomainTXTLookup resolves TXT records for the domain-ownership challenge.
// Production uses the system resolver; tests install their own answers.
var ssoDomainTXTLookup = func(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// ssoUnclaimableDomains are public email providers and shared mail services.
// No organization can prove ownership of them, and a claim would lock every
// user of the provider out of password login platform-wide.
var ssoUnclaimableDomains = []string{
	"gmail.com", "googlemail.com", "yahoo.com", "yahoo.co.jp", "yahoo.co.uk", "yahoo.co.in",
	"ymail.com", "rocketmail.com", "outlook.com", "hotmail.com", "live.com", "msn.com", "windowslive.com",
	"icloud.com", "me.com", "mac.com", "aol.com", "gmx.com", "gmx.net", "gmx.de", "gmx.at", "gmx.ch",
	"mail.com", "email.com", "inbox.com", "protonmail.com", "proton.me", "pm.me",
	"tutanota.com", "tutanota.de", "tuta.io", "zoho.com", "zoho.eu", "fastmail.com", "fastmail.fm", "hey.com",
	"yandex.com", "yandex.ru", "ya.ru", "mail.ru", "inbox.ru", "list.ru", "bk.ru",
	"163.com", "126.com", "yeah.net", "qq.com", "foxmail.com", "sina.com", "sohu.com", "139.com", "189.cn", "aliyun.com",
	"posteo.de", "posteo.net", "mailbox.org", "disroot.org", "riseup.net", "hushmail.com", "runbox.com",
	"lycos.com", "rediffmail.com", "seznam.cz", "atlas.cz", "centrum.cz", "wp.pl", "onet.pl", "interia.pl",
	"t-online.de", "web.de", "online.de", "freenet.de", "arcor.de", "orange.fr", "free.fr", "wanadoo.fr", "laposte.net", "sfr.fr",
	"btinternet.com", "virginmedia.com", "sky.com", "comcast.net", "verizon.net", "att.net", "sbcglobal.net",
	"cox.net", "earthlink.net", "mindspring.com", "optonline.net", "centurytel.net", "embarqmail.com",
	"ntlworld.com", "talktalk.net", "plus.net", "naver.com", "daum.net", "hanmail.net", "nate.com",
	"example.com", "example.org", "example.net", "example.edu",
}

// ssoDomainIsPublic reports whether a domain belongs to a public email
// provider, including its subdomains.
func ssoDomainIsPublic(domain string) bool {
	for _, public := range ssoUnclaimableDomains {
		if domain == public || strings.HasSuffix(domain, "."+public) {
			return true
		}
	}
	return false
}

// ssoDomainChallengeValue is the TXT record value an organization must
// publish to prove it controls a domain.
func ssoDomainChallengeValue(organizationID string) string {
	return "shift-sso-verify=" + organizationID
}

// verifySSODomainOwnership proves the organization controls the domain via a
// DNS TXT challenge: a record naming this organization must exist under the
// challenge name (or the domain apex) before the claim is honored. Without
// the proof, any plan holder could claim a foreign domain and squat it —
// locking its users out of password login and auto-joining them here.
func verifySSODomainOwnership(ctx context.Context, organizationID, domain string) error {
	expected := ssoDomainChallengeValue(organizationID)
	for _, name := range []string{"_shift-sso-verify." + domain, domain} {
		records, err := ssoDomainTXTLookup(ctx, name)
		if err != nil {
			continue
		}
		for _, record := range records {
			if strings.TrimSpace(record) == expected {
				return nil
			}
		}
	}
	return fmt.Errorf("this organization does not control %s: publish a DNS TXT record on _shift-sso-verify.%s with the value %s, then repeat the request", domain, domain, expected)
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
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}
