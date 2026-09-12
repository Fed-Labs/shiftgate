package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeIdP is a minimal but honest OpenID provider: a discovery document, a
// JWKS, and a token endpoint. Tokens are really signed with a locally
// generated RSA key, so the verification under test is cryptographic, not
// mocked.
type fakeIdP struct {
	server       *httptest.Server
	key          *rsa.PrivateKey
	issuer       string
	clientID     string
	audience     any
	nonce        string
	email        string
	verified     bool
	subject      string
	signToken    bool
	mintStale    bool
	sawBasicAuth bool
	// claimIssuer overrides the iss claim so a token can lie about its issuer
	// while discovery stays honest.
	claimIssuer string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, clientID: "shift-control", subject: "user-42", email: "person@example.test", verified: true, signToken: true, audience: "shift-control"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(writer http.ResponseWriter, request *http.Request) {
		writeJSONTest(writer, map[string]string{
			"issuer":                 idp.issuer,
			"authorization_endpoint": idp.issuer + "/authorize",
			"token_endpoint":         idp.issuer + "/token",
			"jwks_uri":               idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(writer http.ResponseWriter, request *http.Request) {
		writeJSONTest(writer, map[string]any{"keys": []any{rsaJWK(idp.key)}})
	})
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			http.Error(writer, "bad form", http.StatusBadRequest)
			return
		}
		if request.PostForm.Get("grant_type") != "authorization_code" || request.PostForm.Get("code_verifier") == "" {
			http.Error(writer, "missing grant or verifier", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Basic ") {
			http.Error(writer, "client authentication missing", http.StatusUnauthorized)
			return
		}
		idp.sawBasicAuth = true
		writeJSONTest(writer, map[string]any{
			"access_token": "unused",
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idp.mintToken(t),
		})
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	idp.server = server
	idp.issuer = server.URL
	return idp
}

func writeJSONTest(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

// rsaJWK publishes the public half of the signing key.
func rsaJWK(key *rsa.PrivateKey) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": "key-1", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

// mintToken builds a compact JWS over the configured claims. mintStale bakes
// in an expiry in the past; signToken=false leaves the signature off.
func (idp *fakeIdP) mintToken(t *testing.T) string {
	t.Helper()
	header := map[string]string{"alg": "RS256", "kid": "key-1", "typ": "JWT"}
	expiry := time.Now().Add(time.Hour).Unix()
	if idp.mintStale {
		expiry = time.Now().Add(-time.Hour).Unix()
	}
	issuer := idp.issuer
	if idp.claimIssuer != "" {
		issuer = idp.claimIssuer
	}
	claims := map[string]any{
		"iss":            issuer,
		"sub":            idp.subject,
		"aud":            idp.audience,
		"exp":            expiry,
		"iat":            time.Now().Unix(),
		"nonce":          idp.nonce,
		"email":          idp.email,
		"email_verified": idp.verified,
		"name":           "Test Person",
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(mustJSON(t, header))
	encodedClaims := base64.RawURLEncoding.EncodeToString(mustJSON(t, claims))
	signingInput := encodedHeader + "." + encodedClaims
	if !idp.signToken {
		return signingInput + "."
	}
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func newTestProvider(t *testing.T, idp *fakeIdP) *Provider {
	t.Helper()
	provider, err := New(idp.issuer, idp.clientID, "client-secret", idp.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func TestValidateIDTokenAcceptsASignedToken(t *testing.T) {
	idp := newFakeIdP(t)
	idp.nonce = "nonce-abc"
	idp.audience = []string{"other-client", "shift-control"}
	provider := newTestProvider(t, idp)
	claims, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), "nonce-abc")
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "user-42" || claims.Email != "person@example.test" || !claims.EmailVerified {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestValidateIDTokenRejectsTamperedPayloads(t *testing.T) {
	idp := newFakeIdP(t)
	provider := newTestProvider(t, idp)
	token := idp.mintToken(t)
	// Swap the payload for one that promotes the subject, keeping the
	// original signature: verification must fail.
	parts := split3(token)
	forgedClaims, _ := json.Marshal(map[string]any{"iss": idp.issuer, "sub": "attacker", "aud": "shift-control", "exp": time.Now().Add(time.Hour).Unix()})
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(forgedClaims) + "." + parts[2]
	if _, err := provider.ValidateIDToken(context.Background(), forged, ""); err == nil {
		t.Fatal("a forged payload verified")
	}
}

func TestValidateIDTokenRejectsWrongIssuerAudienceAndNonce(t *testing.T) {
	idp := newFakeIdP(t)
	provider := newTestProvider(t, idp)
	idp.claimIssuer = "https://other-issuer.example"
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), ""); err == nil {
		t.Fatal("a token from the wrong issuer verified")
	}
	idp.claimIssuer = ""
	idp.audience = "someone-else"
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), ""); err == nil {
		t.Fatal("a token for another audience verified")
	}
	idp.audience = "shift-control"
	idp.nonce = "nonce-one"
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), "nonce-two"); err == nil {
		t.Fatal("a token with the wrong nonce verified")
	}
}

func TestValidateIDTokenRejectsExpiredAndUnsignedTokens(t *testing.T) {
	idp := newFakeIdP(t)
	provider := newTestProvider(t, idp)
	// Expiry is baked into the claims; mint one already stale.
	idp.mintStale = true
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), ""); err == nil {
		t.Fatal("an expired token verified")
	}
	idp.mintStale = false
	idp.signToken = false
	if _, err := provider.ValidateIDToken(context.Background(), idp.mintToken(t), ""); err == nil {
		t.Fatal("an unsigned token verified")
	}
}

func TestExchangeCodeSendsPKCEAndBasicAuth(t *testing.T) {
	idp := newFakeIdP(t)
	idp.nonce = "nonce-xyz"
	provider := newTestProvider(t, idp)
	verifier, challenge, err := Verifier()
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := provider.AuthorizationURL(context.Background(), "http://127.0.0.1:0/callback", "state-1", "nonce-xyz", challenge)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(authorizationURL, "code_challenge_method=S256", "state=state-1", "nonce=nonce-xyz") {
		t.Fatalf("authorization URL is missing required parameters: %s", authorizationURL)
	}
	tokens, err := provider.ExchangeCode(context.Background(), "code-1", "http://127.0.0.1:0/callback", verifier)
	if err != nil {
		t.Fatal(err)
	}
	if !idp.sawBasicAuth {
		t.Fatal("the token request carried no client Basic authentication")
	}
	if _, err := provider.ValidateIDToken(context.Background(), tokens.IDToken, "nonce-xyz"); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsMisconfiguredIssuers(t *testing.T) {
	for _, issuer := range []string{"", "http://issuer.example", "https://issuer.example/?q=1", "https://issuer.example#f", "https://user@issuer.example", "https://issuer.example/"} {
		if _, err := New(issuer, "client", "", nil); err == nil {
			t.Fatalf("issuer %q was accepted", issuer)
		}
	}
	if _, err := New("https://issuer.example", "", "", nil); err == nil {
		t.Fatal("an empty client id was accepted")
	}
}

func split3(token string) [3]string {
	var parts [3]string
	copy(parts[:], strings.SplitN(token, ".", 3))
	return parts
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
