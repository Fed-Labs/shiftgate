package controlplane

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
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
	"shift.dev/shift/internal/oidc"
)

// sso_scim_test.go: the SSO round trip and the SCIM provisioning surface,
// exercised against the integration database and a fake identity provider
// whose ID tokens are really signed — the verification under test is
// cryptographic, not stubbed.

// requestJSON is postJSON for any method: it sends input as JSON, requires the
// expected status, and decodes the body.
func requestJSON(t *testing.T, client *http.Client, method, endpoint string, input any, expectedStatus int, output any, authorization string) {
	t.Helper()
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(ssoMustJSON(t, input)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		content, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status %d, want %d: %s", method, endpoint, response.StatusCode, expectedStatus, content)
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatalf("decode %s %s response: %v", method, endpoint, err)
		}
	}
}

// fakeIssuedCode is what the fake issuer remembers about one authorization
// code: the nonce it must appear in the ID token, the PKCE challenge it
// redeems against, and the redirect URI it is bound to.
type fakeIssuedCode struct {
	nonce       string
	challenge   string
	redirectURI string
}

// fakeIdentityProvider is a small but honest OpenID provider: discovery, JWKS,
// an authorization endpoint that issues one-time codes bound to the request's
// nonce and PKCE challenge, and a token endpoint that enforces the code, the
// verifier, the redirect URI, and client Basic authentication.
type fakeIdentityProvider struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	issuer   string
	clientID string
	secret   string
	email    string
	subject  string
	verified bool

	mutex sync.Mutex
	codes map[string]fakeIssuedCode
}

// newFakeIdentityProvider brings the issuer up over TLS and returns it.
func newFakeIdentityProvider(t *testing.T) *fakeIdentityProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdentityProvider{
		key: key, clientID: "shift-control", secret: "issuer-secret",
		email: "person@example.test", subject: "user-42", verified: true,
		codes: map[string]fakeIssuedCode{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(writer http.ResponseWriter, request *http.Request) {
		ssoWriteJSON(writer, map[string]string{
			"issuer":                 idp.issuer,
			"authorization_endpoint": idp.issuer + "/authorize",
			"token_endpoint":         idp.issuer + "/token",
			"jwks_uri":               idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(writer http.ResponseWriter, request *http.Request) {
		public := key.PublicKey
		ssoWriteJSON(writer, map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "kid": "key-1", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(bigEndian(public.E)),
		}}})
	})
	mux.HandleFunc("/authorize", idp.handleAuthorize)
	mux.HandleFunc("/token", idp.handleToken)
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	idp.server = server
	idp.issuer = server.URL
	return idp
}

// handleAuthorize plays the browser-facing half: validate the authorization
// request the control plane built, then redirect back with a one-time code.
func (idp *fakeIdentityProvider) handleAuthorize(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	if query.Get("client_id") != idp.clientID || query.Get("response_type") != "code" {
		http.Error(writer, "unknown client or response type", http.StatusBadRequest)
		return
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		http.Error(writer, "this issuer requires PKCE S256", http.StatusBadRequest)
		return
	}
	state, nonce, redirectURI := query.Get("state"), query.Get("nonce"), query.Get("redirect_uri")
	if state == "" || nonce == "" || redirectURI == "" {
		http.Error(writer, "state, nonce, and redirect_uri are required", http.StatusBadRequest)
		return
	}
	codeBytes := make([]byte, 24)
	if _, err := rand.Read(codeBytes); err != nil {
		http.Error(writer, "code generation failed", http.StatusInternalServerError)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(codeBytes)
	idp.mutex.Lock()
	idp.codes[code] = fakeIssuedCode{nonce: nonce, challenge: query.Get("code_challenge"), redirectURI: redirectURI}
	idp.mutex.Unlock()
	http.Redirect(writer, request, redirectURI+"?code="+code+"&state="+url.QueryEscape(state), http.StatusFound)
}

// handleToken plays the token endpoint: the code must be the one issued, be
// one-time, redeem against the same verifier challenge it was issued for, and
// arrive with client Basic authentication.
func (idp *fakeIdentityProvider) handleToken(writer http.ResponseWriter, request *http.Request) {
	if err := request.ParseForm(); err != nil {
		http.Error(writer, "bad form", http.StatusBadRequest)
		return
	}
	if request.PostForm.Get("grant_type") != "authorization_code" {
		http.Error(writer, "unsupported grant", http.StatusBadRequest)
		return
	}
	username, password, ok := request.BasicAuth()
	if !ok || username != idp.clientID || password != idp.secret {
		http.Error(writer, "client authentication failed", http.StatusUnauthorized)
		return
	}
	code := request.PostForm.Get("code")
	idp.mutex.Lock()
	issued, found := idp.codes[code]
	delete(idp.codes, code)
	idp.mutex.Unlock()
	if !found {
		http.Error(writer, "unknown or reused code", http.StatusBadRequest)
		return
	}
	if request.PostForm.Get("redirect_uri") != issued.redirectURI {
		http.Error(writer, "redirect_uri mismatch", http.StatusBadRequest)
		return
	}
	digest := sha256.Sum256([]byte(request.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(digest[:]) != issued.challenge {
		http.Error(writer, "PKCE verification failed", http.StatusBadRequest)
		return
	}
	token, err := idp.mintIDToken(issued.nonce)
	if err != nil {
		http.Error(writer, "signing failed", http.StatusInternalServerError)
		return
	}
	ssoWriteJSON(writer, map[string]any{"access_token": "unused", "token_type": "Bearer", "expires_in": 3600, "id_token": token})
}

// mintIDToken signs the ID token with the issuer's RSA key: a real JWS, so
// the control plane's signature verification is genuinely exercised.
func (idp *fakeIdentityProvider) mintIDToken(nonce string) (string, error) {
	header := map[string]string{"alg": "RS256", "kid": "key-1", "typ": "JWT"}
	claims := map[string]any{
		"iss":            idp.issuer,
		"sub":            idp.subject,
		"aud":            idp.clientID,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"iat":            time.Now().Unix(),
		"nonce":          nonce,
		"email":          idp.email,
		"email_verified": idp.verified,
		"name":           "IdP Person",
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(ssoMustJSON(nil, header))
	encodedClaims := base64.RawURLEncoding.EncodeToString(ssoMustJSON(nil, claims))
	signingInput := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// bigEndian encodes an RSA public exponent the way a JWK carries it.
func bigEndian(value int) []byte {
	encoded := []byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}
	for len(encoded) > 1 && encoded[0] == 0 {
		encoded = encoded[1:]
	}
	return encoded
}

func ssoMustJSON(t *testing.T, value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	return encoded
}

func ssoWriteJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

// openSSOTestServer boots a control plane against the integration database
// with one registered organization. When sso is true the control plane is
// configured against the fake issuer and its outbound client trusts the
// issuer's test certificate.
func openSSOTestServer(t *testing.T, sso bool) (*httptest.Server, string, string, *fakeIdentityProvider) {
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
	var idp *fakeIdentityProvider
	if sso {
		idp = newFakeIdentityProvider(t)
		configuration.OIDC = config.OIDC{Issuer: idp.issuer, ClientID: idp.clientID, ClientSecret: idp.secret}
	}
	server := New(configuration, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if sso {
		server.oidcHTTPClient = idp.server.Client()
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	register := RegisterRequest{Email: "sso-" + time.Now().UTC().Format("20060102150405.000000000") + "@example.test", Password: "correct horse battery staple", DisplayName: "SSO User", Organization: "SSO Organization"}
	var registration struct {
		User         User          `json:"user"`
		Organization Organization  `json:"organization"`
		Tokens       SessionTokens `json:"tokens"`
	}
	postJSON(t, httpServer.Client(), httpServer.URL+"/v1/auth/register", register, http.StatusCreated, &registration, "")
	return httpServer, registration.Organization.ID, "Bearer " + registration.Tokens.AccessToken, idp
}

// ssoLogin performs the full federated round trip the way the CLI does: begin
// on the control plane, walk the issuer's authorization endpoint as the
// browser would, and redeem the code.
func ssoLogin(t *testing.T, httpServer *httptest.Server, idp *fakeIdentityProvider) (User, SessionTokens) {
	t.Helper()
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

	// The browser: hit the issuer without following the redirect, and take the
	// code and state out of the Location.
	browser := &http.Client{Transport: idp.server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := browser.Get(authorization.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("issuer authorization status %d, want 302", response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("state") != authorization.State {
		t.Fatal("the issuer did not echo the state back")
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatal("the issuer returned no authorization code")
	}

	var session struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/token",
		map[string]string{"code": code, "state": authorization.State, "code_verifier": verifier, "redirect_uri": redirectURI},
		http.StatusOK, &session, "")
	return session.User, session.Tokens
}

func TestSSOLoginFlow(t *testing.T) {
	httpServer, _, _, idp := openSSOTestServer(t, true)
	client := httpServer.Client()

	// A domain under no enforcement answers required=false, so clients can
	// route to a password prompt without guessing.
	var required struct {
		Required bool `json:"required"`
	}
	getJSON(t, client, httpServer.URL+"/v1/auth/sso/required?email="+url.QueryEscape(idp.email), http.StatusOK, &required, "")
	if required.Required {
		t.Fatal("no organization enforces this domain yet")
	}

	user, tokens := ssoLogin(t, httpServer, idp)
	if user.Email != idp.email {
		t.Fatalf("federated login authenticated %q, want %q", user.Email, idp.email)
	}

	// The federated session is an ordinary session.
	var me Principal
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusOK, &me, "Bearer "+tokens.AccessToken)
	if me.Email != idp.email {
		t.Fatalf("session resolves to %q, want %q", me.Email, idp.email)
	}

	// The same identity logs in to the same account, not a second one.
	again, _ := ssoLogin(t, httpServer, idp)
	if again.ID != user.ID {
		t.Fatalf("second federated login created %q instead of returning %q", again.ID, user.ID)
	}

	// Authorization codes are one-time: a replay is the issuer's refusal.
	verifier, _, err := oidc.Verifier()
	if err != nil {
		t.Fatal(err)
	}
	redirectURI := "http://127.0.0.1:49152/callback"
	var begin struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
	}
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/authorize",
		map[string]string{"email": idp.email, "redirect_uri": redirectURI, "code_challenge": verifierChallenge(t, verifier)},
		http.StatusOK, &begin, "")
	browser := &http.Client{Transport: idp.server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := browser.Get(begin.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	redeem := map[string]string{"code": location.Query().Get("code"), "state": begin.State, "code_verifier": verifier, "redirect_uri": redirectURI}
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/token", redeem, http.StatusOK, &struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}{}, "")
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/token", redeem, http.StatusBadGateway, &struct{}{}, "")

	// A tampered state never reaches the issuer.
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/token",
		map[string]string{"code": "aaaaaaaaaaaaaaaaaaaaaaaa", "state": "forged.signature", "code_verifier": verifier, "redirect_uri": redirectURI},
		http.StatusBadRequest, &struct{}{}, "")

	// Non-loopback redirect URIs are refused: authorization codes must not
	// travel to a remote host.
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/authorize",
		map[string]string{"email": idp.email, "redirect_uri": "https://attacker.example/callback", "code_challenge": verifierChallenge(t, verifier)},
		http.StatusBadRequest, &struct{}{}, "")
}

// verifierChallenge derives the S256 challenge of a verifier, for request
// bodies that send the challenge directly.
func verifierChallenge(t *testing.T, verifier string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// TestSSOEnforcement walks the admin surface: claiming a domain turns password
// login into a refusal, and a federated login lands in the enforcing
// organization.
func TestSSOEnforcement(t *testing.T) {
	httpServer, organizationID, authHeader, idp := openSSOTestServer(t, true)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	// The enforcement surface is honest before it is configured on.
	getJSON(t, client, organizationPath+"/sso", http.StatusOK, &SetOrganizationSSORequest{Enforced: false, EmailDomain: ""}, authHeader)

	// Enforce the fake issuer's domain.
	requestJSON(t, client, http.MethodPut, organizationPath+"/sso", SetOrganizationSSORequest{Enforced: true, EmailDomain: "example.test"}, http.StatusOK, &SetOrganizationSSORequest{}, authHeader)
	var state SetOrganizationSSORequest
	getJSON(t, client, organizationPath+"/sso", http.StatusOK, &state, authHeader)
	if !state.Enforced || state.EmailDomain != "example.test" {
		t.Fatalf("enforcement did not stick: %+v", state)
	}

	// required now answers true for the domain.
	var required struct {
		Required bool `json:"required"`
	}
	getJSON(t, client, httpServer.URL+"/v1/auth/sso/required?email="+url.QueryEscape(idp.email), http.StatusOK, &required, "")
	if !required.Required {
		t.Fatal("the enforcing domain did not answer required=true")
	}

	// Password login and self-service registration for the domain are
	// refused before any credential is checked.
	expectStatus(t, client, http.MethodPost, httpServer.URL+"/v1/auth/login", http.StatusForbidden, "", LoginRequest{Email: idp.email, Password: "whatever the password is"})
	expectStatus(t, client, http.MethodPost, httpServer.URL+"/v1/auth/register", http.StatusForbidden, "", RegisterRequest{Email: "newcomer@example.test", Password: "correct horse battery staple", DisplayName: "Newcomer"})

	// A federated login lands in the enforcing organization as a viewer.
	user, tokens := ssoLogin(t, httpServer, idp)
	if user.Email != idp.email {
		t.Fatalf("federated login authenticated %q", user.Email)
	}
	var organizations []Organization
	getJSON(t, client, httpServer.URL+"/v1/organizations", http.StatusOK, &organizations, "Bearer "+tokens.AccessToken)
	found := false
	for _, organization := range organizations {
		if organization.ID == organizationID {
			found = true
		}
	}
	if !found {
		t.Fatalf("federated user %s did not join the enforcing organization: %v", user.Email, organizations)
	}

	// An invalid domain never claims anything.
	expectStatus(t, client, http.MethodPut, organizationPath+"/sso", http.StatusBadRequest, authHeader, SetOrganizationSSORequest{Enforced: true, EmailDomain: "not a domain"})
	// Disabling clears the claim.
	requestJSON(t, client, http.MethodPut, organizationPath+"/sso", SetOrganizationSSORequest{Enforced: false}, http.StatusOK, &SetOrganizationSSORequest{}, authHeader)
	getJSON(t, client, organizationPath+"/sso", http.StatusOK, &state, authHeader)
	if state.Enforced || state.EmailDomain != "" {
		t.Fatalf("disabling did not clear the domain: %+v", state)
	}
}

// TestSSOUnconfigured proves the surface holds together when no issuer is
// configured at all: SSO is unreachable and password login is untouched.
func TestSSOUnconfigured(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()

	expectStatus(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/authorize", http.StatusServiceUnavailable, "", map[string]string{"email": "person@example.test", "redirect_uri": "http://127.0.0.1:49152/callback", "code_challenge": strings.Repeat("c", 43)})
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID
	expectStatus(t, client, http.MethodPut, organizationPath+"/sso", http.StatusServiceUnavailable, authHeader, SetOrganizationSSORequest{Enforced: true, EmailDomain: "example.test"})
	var required struct {
		Required bool `json:"required"`
	}
	getJSON(t, client, httpServer.URL+"/v1/auth/sso/required?email=person@example.test", http.StatusOK, &required, "")
	if required.Required {
		t.Fatal("SSO cannot be required when no issuer is configured")
	}
}

// scimUserSummary is the subset of a SCIM User resource the tests assert on.
type scimUserSummary struct {
	ID          string `json:"id"`
	ExternalID  string `json:"externalId"`
	UserName    string `json:"userName"`
	DisplayName string `json:"displayName"`
	Active      bool   `json:"active"`
}

// TestSCIMProvisioning drives the whole provisioning surface: an API key with
// the scim scope creates, lists, replaces, patches, and deactivates users,
// and nothing weaker than that key can reach it.
func TestSCIMProvisioning(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"

	// The provisioning credential: an API key with the scim scope.
	var key APIKeyCreated
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+organizationID+"/api-keys", CreateAPIKeyRequest{Name: "provisioning", Scopes: []string{"scim"}}, http.StatusCreated, &key, authHeader)
	scimAuth := "Bearer " + key.Secret

	// Weaker credentials cannot provision: not a browser session, not a key
	// without the scope, not an anonymous request.
	expectStatus(t, client, http.MethodGet, httpServer.URL+"/v1/scim/v2/ServiceProviderConfig", http.StatusForbidden, authHeader, nil)
	var readKey APIKeyCreated
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+organizationID+"/api-keys", CreateAPIKeyRequest{Name: "observer", Scopes: []string{"read"}}, http.StatusCreated, &readKey, authHeader)
	expectStatus(t, client, http.MethodGet, usersPath, http.StatusForbidden, "Bearer "+readKey.Secret, nil)
	expectStatus(t, client, http.MethodGet, usersPath, http.StatusUnauthorized, "", nil)

	// Discovery answers with the protocol's shapes.
	var providerConfig map[string]any
	getJSON(t, client, httpServer.URL+"/v1/scim/v2/ServiceProviderConfig", http.StatusOK, &providerConfig, scimAuth)
	if providerConfig["patch"] == nil {
		t.Fatalf("service provider config is missing its capability statements: %v", providerConfig)
	}
	getJSON(t, client, httpServer.URL+"/v1/scim/v2/ResourceTypes", http.StatusOK, &map[string]any{}, scimAuth)
	getJSON(t, client, httpServer.URL+"/v1/scim/v2/Schemas", http.StatusOK, &map[string]any{}, scimAuth)

	// Create.
	email := "provisioned-" + time.Now().UTC().Format("20060102150405.000000000") + "@scim.example.test"
	var user scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath, map[string]any{"userName": email, "displayName": "Provisioned Person", "externalId": "ext-1"}, http.StatusCreated, &user, scimAuth)
	if user.ID == "" || user.UserName != email || !user.Active {
		t.Fatalf("created user is wrong: %+v", user)
	}

	// A duplicate email is a conflict, not a second account.
	expectStatus(t, client, http.MethodPost, usersPath, http.StatusConflict, scimAuth, map[string]any{"userName": email})

	// Listing with a filter finds exactly the provisioned account.
	var list struct {
		TotalResults int               `json:"totalResults"`
		StartIndex   int               `json:"startIndex"`
		Resources    []scimUserSummary `json:"Resources"`
	}
	getJSON(t, client, usersPath+"?filter="+url.QueryEscape(`userName eq "`+email+`"`), http.StatusOK, &list, scimAuth)
	if list.TotalResults != 1 || len(list.Resources) != 1 || list.Resources[0].ID != user.ID {
		t.Fatalf("filtered list returned %+v", list)
	}
	// Unfiltered listing sees at least the provisioned account.
	getJSON(t, client, usersPath, http.StatusOK, &list, scimAuth)
	if list.TotalResults < 1 || list.StartIndex != 1 {
		t.Fatalf("unfiltered list returned %+v", list)
	}
	// Unsupported filter shapes are refused, not mis-executed.
	getJSON(t, client, usersPath+"?filter="+url.QueryEscape("userName eq"), http.StatusBadRequest, &map[string]any{}, scimAuth)
	getJSON(t, client, usersPath+"?filter="+url.QueryEscape(`password eq "hunter2"`), http.StatusBadRequest, &map[string]any{}, scimAuth)

	// A full replacement rewrites the idP-controlled fields.
	requestJSON(t, client, http.MethodPut, usersPath+"/"+user.ID, map[string]any{"userName": email, "displayName": "Renamed Person", "externalId": "ext-2", "active": true}, http.StatusOK, &user, scimAuth)
	if user.DisplayName != "Renamed Person" || user.ExternalID != "ext-2" {
		t.Fatalf("replacement did not take: %+v", user)
	}

	// A patch deactivates the account; deactivation is SCIM's delete.
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+user.ID, map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}}, http.StatusOK, &user, scimAuth)
	if user.Active {
		t.Fatalf("patch did not deactivate: %+v", user)
	}
	// Unsupported patch shapes say so instead of silently doing nothing.
	expectStatus(t, client, http.MethodPatch, usersPath+"/"+user.ID, http.StatusBadRequest, scimAuth, map[string]any{"Operations": []map[string]any{{"op": "remove", "path": "displayName"}}})
	expectStatus(t, client, http.MethodPatch, usersPath+"/"+user.ID, http.StatusBadRequest, scimAuth, map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "nickName", "value": "nick"}}})

	// Re-activation restores the account.
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+user.ID, map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": true}}}, http.StatusOK, &user, scimAuth)
	if !user.Active {
		t.Fatalf("re-activation did not take: %+v", user)
	}

	// Provisioned accounts have no password: no password opens them.
	expectStatus(t, client, http.MethodPost, httpServer.URL+"/v1/auth/login", http.StatusUnauthorized, "", LoginRequest{Email: email, Password: "some password"})

	// DELETE deactivates; the row survives.
	expectStatus(t, client, http.MethodDelete, usersPath+"/"+user.ID, http.StatusNoContent, scimAuth, nil)
	getJSON(t, client, usersPath+"/"+user.ID, http.StatusOK, &user, scimAuth)
	if user.Active {
		t.Fatalf("delete left the account active: %+v", user)
	}
	// A deactivated account cannot authenticate at all.
	expectStatus(t, client, http.MethodPost, httpServer.URL+"/v1/auth/login", http.StatusUnauthorized, "", LoginRequest{Email: email, Password: "some password"})

	// Unknown users are a 404 in the SCIM error shape.
	var missing map[string]any
	getJSON(t, client, usersPath+"/does-not-exist", http.StatusNotFound, &missing, scimAuth)
	if missing["status"] != "404" {
		t.Fatalf("SCIM error shape is wrong: %v", missing)
	}
}
