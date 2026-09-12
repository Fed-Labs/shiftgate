// Package oidc implements the client half of OpenID Connect that the control
// plane needs: provider discovery, the authorization-code exchange with PKCE,
// and ID-token verification against the issuer's published JWKS. Signature
// verification is real RS256/ES256 with the standard library — no token is
// ever trusted because it decoded, only because it verified.
package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Provider is one configured issuer: its discovery document and signing keys,
// fetched on first use and cached. Keys are refreshed when a token arrives
// carrying a kid the cache has never seen.
type Provider struct {
	issuer       string
	clientID     string
	clientSecret string
	httpClient   *http.Client

	mutex         sync.Mutex
	discovered    *Discovery
	keys          []JWK
	keysFetchedAt time.Time
}

// Discovery is the subset of the OpenID provider configuration the control
// plane uses. The issuer field inside the document must equal the configured
// issuer exactly, or the document is rejected.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// JWK is one published signing key. RSA keys carry n and e; EC keys carry the
// curve and affine coordinates.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// New validates and remembers the issuer configuration. The issuer must be an
// https URL without query or fragment; the client secret may be empty, which
// selects the public-client flow — PKCE is used either way.
func New(issuer, clientID, clientSecret string, httpClient *http.Client) (*Provider, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimRight(parsed.Path, "/") != parsed.Path {
		return nil, fmt.Errorf("OIDC issuer must be an https URL without query or fragment: %q", issuer)
	}
	if strings.TrimSpace(clientID) == "" {
		return nil, errors.New("OIDC client id is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Provider{issuer: strings.TrimRight(issuer, "/"), clientID: clientID, clientSecret: clientSecret, httpClient: httpClient}, nil
}

// Issuer is the configured issuer, without a trailing slash.
func (provider *Provider) Issuer() string { return provider.issuer }

// ClientID is the configured OAuth client.
func (provider *Provider) ClientID() string { return provider.clientID }

// discovery fetches and caches the provider configuration. The document's own
// issuer field must match the configured issuer — a mismatch means DNS or
// configuration is lying about who is answering.
func (provider *Provider) discovery(ctx context.Context) (Discovery, error) {
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	if provider.discovered != nil {
		return *provider.discovered, nil
	}
	endpoint := provider.issuer + "/.well-known/openid-configuration"
	var document Discovery
	if err := fetchJSON(ctx, provider.httpClient, endpoint, &document); err != nil {
		return Discovery{}, fmt.Errorf("fetch OIDC discovery from %s: %w", endpoint, err)
	}
	if document.Issuer != provider.issuer {
		return Discovery{}, fmt.Errorf("discovery document issuer %q does not match configured issuer %q", document.Issuer, provider.issuer)
	}
	for name, value := range map[string]string{"authorization endpoint": document.AuthorizationEndpoint, "token endpoint": document.TokenEndpoint, "jwks uri": document.JWKSURI} {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return Discovery{}, fmt.Errorf("OIDC discovery %s must be an https URL: %q", name, value)
		}
	}
	provider.discovered = &document
	return document, nil
}

// AuthorizationURL builds the redirect to the issuer's authorization endpoint.
// The caller supplies the redirect URI, the state it will verify on callback,
// the nonce it will demand inside the ID token, and the S256 PKCE challenge
// derived from its verifier.
func (provider *Provider) AuthorizationURL(ctx context.Context, redirectURI, state, nonce, challenge string) (string, error) {
	document, err := provider.discovery(ctx)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", provider.clientID)
	values.Set("redirect_uri", redirectURI)
	values.Set("state", state)
	values.Set("nonce", nonce)
	values.Set("scope", "openid email profile")
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	return document.AuthorizationEndpoint + "?" + values.Encode(), nil
}

// Verifier returns a fresh PKCE code verifier and its S256 challenge.
func Verifier() (verifier, challenge string, err error) {
	raw := make([]byte, 48)
	if _, err = rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// TokenResponse is the issuer's answer at the token endpoint. Only the ID
// token matters to the control plane; the access token is neither used nor
// stored.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
}

// ExchangeCode trades the authorization code for tokens. PKCE is always sent;
// the client secret, when one is configured, travels as HTTP Basic
// authentication per the OAuth 2.0 token-endpoint convention, never in the
// form body.
func (provider *Provider) ExchangeCode(ctx context.Context, code, redirectURI, verifier string) (TokenResponse, error) {
	document, err := provider.discovery(ctx)
	if err != nil {
		return TokenResponse{}, err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", provider.clientID)
	form.Set("code_verifier", verifier)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, document.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if provider.clientSecret != "" {
		request.SetBasicAuth(url.QueryEscape(provider.clientID), url.QueryEscape(provider.clientSecret))
	}
	response, err := provider.httpClient.Do(request)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("exchange authorization code: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var tokens TokenResponse
	if err := decodeBody(response, &tokens); err != nil {
		return TokenResponse{}, err
	}
	if tokens.IDToken == "" {
		return TokenResponse{}, errors.New("token endpoint returned no id_token")
	}
	return tokens, nil
}

// Claims are the ID-token assertions the control plane acts on. Audiences
// accept either the single-string or the array spelling, because issuers use
// both.
type Claims struct {
	Issuer        string   `json:"iss"`
	Subject       string   `json:"sub"`
	Audiences     []string `json:"-"`
	ExpiresAt     int64    `json:"exp"`
	IssuedAt      int64    `json:"iat"`
	Nonce         string   `json:"nonce"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
}

type rawClaims struct {
	Claims
	Audience json.RawMessage `json:"aud"`
}

// ValidateIDToken verifies the token's signature against the issuer's keys,
// then checks issuer, audience, expiry, and nonce. An unknown kid triggers one
// key refresh before failing — issuers rotate.
func (provider *Provider) ValidateIDToken(ctx context.Context, token, expectedNonce string) (Claims, error) {
	if _, err := provider.discovery(ctx); err != nil {
		return Claims{}, err
	}
	header, payload, signature, err := splitToken(token)
	if err != nil {
		return Claims{}, err
	}
	if header.Alg != "RS256" && header.Alg != "ES256" {
		return Claims{}, fmt.Errorf("unsupported ID-token algorithm %q; only RS256 and ES256 are accepted", header.Alg)
	}
	claims, err := decodeClaims(payload)
	if err != nil {
		return Claims{}, err
	}
	signingInput := token[:strings.LastIndex(token, ".")]
	for attempt := 0; attempt < 2; attempt++ {
		key, err := provider.key(ctx, header.Kid, attempt > 0)
		if err != nil {
			if attempt == 0 {
				continue // unknown kid may mean the keys rotated; refresh once
			}
			return Claims{}, err
		}
		if err := verifySignature(key, header.Alg, []byte(signingInput), signature); err != nil {
			if attempt == 0 {
				continue // a rotated key may have reused the kid; refresh once
			}
			return Claims{}, err
		}
		return provider.checkClaims(claims, expectedNonce)
	}
	return Claims{}, errors.New("ID-token signature did not verify")
}

// checkClaims enforces the assertions after the signature has verified.
func (provider *Provider) checkClaims(claims Claims, expectedNonce string) (Claims, error) {
	if claims.Issuer != provider.issuer {
		return Claims{}, fmt.Errorf("ID-token issuer %q does not match %q", claims.Issuer, provider.issuer)
	}
	if !audienceContains(claims.Audiences, provider.clientID) {
		return Claims{}, fmt.Errorf("ID-token audience %v does not include client %q", claims.Audiences, provider.clientID)
	}
	now := time.Now().UTC()
	if now.Add(2*time.Minute).Unix() >= claims.ExpiresAt {
		return Claims{}, errors.New("ID token is expired")
	}
	if claims.IssuedAt > now.Add(5*time.Minute).Unix() {
		return Claims{}, errors.New("ID token was issued in the future")
	}
	if expectedNonce != "" && claims.Nonce != expectedNonce {
		return Claims{}, errors.New("ID-token nonce does not match this authorization request")
	}
	if claims.Subject == "" {
		return Claims{}, errors.New("ID token carries no subject")
	}
	return claims, nil
}

// audienceContains reports whether the token was minted for this client.
func audienceContains(audiences []string, clientID string) bool {
	for _, audience := range audiences {
		if audience == clientID {
			return true
		}
	}
	return false
}

type tokenHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// splitToken takes a compact JWS apart. The returned header keeps its decoded
// form; payload and signature stay raw because their consumers differ.
func splitToken(token string) (header tokenHeader, payload, signature []byte, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenHeader{}, nil, nil, errors.New("ID token is not a compact JWS")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return tokenHeader{}, nil, nil, fmt.Errorf("decode ID-token header: %w", err)
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return tokenHeader{}, nil, nil, fmt.Errorf("decode ID-token header: %w", err)
	}
	payload, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenHeader{}, nil, nil, fmt.Errorf("decode ID-token payload: %w", err)
	}
	signature, err = base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return tokenHeader{}, nil, nil, fmt.Errorf("decode ID-token signature: %w", err)
	}
	return header, payload, signature, nil
}

// decodeClaims parses the payload, accepting the aud claim in either of its
// two legal spellings.
func decodeClaims(payload []byte) (Claims, error) {
	var raw rawClaims
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Claims{}, fmt.Errorf("decode ID-token claims: %w", err)
	}
	claims := raw.Claims
	claims.Audiences = nil
	if len(raw.Audience) > 0 {
		var single string
		if err := json.Unmarshal(raw.Audience, &single); err == nil {
			claims.Audiences = []string{single}
		} else {
			var many []string
			if err := json.Unmarshal(raw.Audience, &many); err != nil {
				return Claims{}, fmt.Errorf("decode ID-token audience: %w", err)
			}
			claims.Audiences = many
		}
	}
	return claims, nil
}

// key returns the published key with the given kid, refreshing the JWKS when
// told to or when nothing has been fetched yet.
func (provider *Provider) key(ctx context.Context, kid string, refresh bool) (crypto.PublicKey, error) {
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	if refresh || provider.keys == nil {
		document := provider.discovered
		if document == nil {
			return nil, errors.New("JWKS requested before discovery; call AuthorizationURL first")
		}
		endpoint := document.JWKSURI
		keys, err := fetchKeys(ctx, provider.httpClient, endpoint)
		if err != nil {
			return nil, fmt.Errorf("refresh issuer signing keys from %s: %w", endpoint, err)
		}
		provider.keys = keys
		provider.keysFetchedAt = time.Now().UTC()
	}
	for _, candidate := range provider.keys {
		if candidate.Kid != kid {
			continue
		}
		publicKey, err := candidate.publicKey()
		if err != nil {
			return nil, err
		}
		return publicKey, nil
	}
	return nil, fmt.Errorf("issuer published no signing key with kid %q", kid)
}

// fetchKeys downloads and decodes the issuer's JWKS.
func fetchKeys(ctx context.Context, httpClient *http.Client, endpoint string) ([]JWK, error) {
	var document struct {
		Keys []JWK `json:"keys"`
	}
	if err := fetchJSON(ctx, httpClient, endpoint, &document); err != nil {
		return nil, err
	}
	if len(document.Keys) == 0 {
		return nil, errors.New("issuer published an empty key set")
	}
	return document.Keys, nil
}

// publicKey converts one published key into a verifiable public key. Keys
// marked for a purpose other than signing are refused.
func (key JWK) publicKey() (crypto.PublicKey, error) {
	if key.Use != "" && key.Use != "sig" {
		return nil, fmt.Errorf("key %q is for %q, not signing", key.Kid, key.Use)
	}
	switch key.Kty {
	case "RSA":
		if key.Alg != "" && key.Alg != "RS256" {
			return nil, fmt.Errorf("key %q advertises algorithm %q; only RS256 is accepted", key.Kid, key.Alg)
		}
		modulus, err := decodeBase64Int(key.N)
		if err != nil || modulus == nil || modulus.Sign() <= 0 {
			return nil, fmt.Errorf("key %q carries an invalid RSA modulus", key.Kid)
		}
		exponent, err := decodeBase64Int(key.E)
		if err != nil || exponent == nil || !exponent.IsInt64() || exponent.Int64() <= 0 || exponent.Int64() > 1<<31 {
			return nil, fmt.Errorf("key %q carries an invalid RSA exponent", key.Kid)
		}
		return &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}, nil
	case "EC":
		if key.Crv != "P-256" {
			return nil, fmt.Errorf("key %q uses curve %q; only P-256 is accepted", key.Kid, key.Crv)
		}
		x, err := decodeBase64Int(key.X)
		if err != nil || x == nil {
			return nil, fmt.Errorf("key %q carries an invalid x coordinate", key.Kid)
		}
		y, err := decodeBase64Int(key.Y)
		if err != nil || y == nil {
			return nil, fmt.Errorf("key %q carries an invalid y coordinate", key.Kid)
		}
		if x.Cmp(elliptic.P256().Params().N) >= 0 || y.Cmp(elliptic.P256().Params().N) >= 0 {
			return nil, fmt.Errorf("key %q carries out-of-range coordinates", key.Kid)
		}
		// On-curve validation without the deprecated low-level API: parsing
		// the point as an ecdh key performs the same check, and rejects
		// anything not on P-256.
		point := make([]byte, 65)
		point[0] = 4
		x.FillBytes(point[1:33])
		y.FillBytes(point[33:])
		if _, ecdhErr := ecdh.P256().NewPublicKey(point); ecdhErr != nil {
			return nil, fmt.Errorf("key %q is not a point on P-256", key.Kid)
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
	default:
		return nil, fmt.Errorf("key %q has unsupported key type %q", key.Kid, key.Kty)
	}
}

// decodeBase64Int decodes a big-endian base64url integer with no sign prefix,
// as JWK specifies.
func decodeBase64Int(value string) (*big.Int, error) {
	if value == "" {
		return nil, errors.New("empty integer")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(decoded), nil
}

// verifySignature checks the compact-JWS signature over the signing input
// with the given algorithm.
func verifySignature(publicKey crypto.PublicKey, algorithm string, signingInput, signature []byte) error {
	switch algorithm {
	case "RS256":
		key, ok := publicKey.(*rsa.PublicKey)
		if !ok {
			return errors.New("RS256 signature with a non-RSA key")
		}
		digest := sha256.Sum256(signingInput)
		return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature)
	case "ES256":
		key, ok := publicKey.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("ES256 signature with a non-EC key")
		}
		if len(signature) != 64 {
			return errors.New("ES256 signature is not 64 bytes")
		}
		r := new(big.Int).SetBytes(signature[:32])
		s := new(big.Int).SetBytes(signature[32:])
		digest := sha256.Sum256(signingInput)
		if !ecdsa.Verify(key, digest[:], r, s) {
			return errors.New("ES256 signature is invalid")
		}
		return nil
	default:
		return fmt.Errorf("unsupported algorithm %q", algorithm)
	}
}

// fetchJSON GETs endpoint and decodes into target.
func fetchJSON(ctx context.Context, httpClient *http.Client, endpoint string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	return decodeBody(response, target)
}

// decodeBody bounds and decodes a JSON response body, rejecting non-2xx
// statuses with their first line rather than a raw error dump.
func decodeBody(response *http.Response, target any) error {
	body := http.MaxBytesReader(nil, response.Body, 4<<20)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		content, _ := readAllBounded(body, 1024)
		message := strings.TrimSpace(string(content))
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("status %d: %s", response.StatusCode, message)
	}
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	// Reject trailing data the same way the control plane does: a document
	// with something after it is not the document we asked for.
	if err := decoder.Decode(&struct{}{}); err == nil {
		return errors.New("response contains trailing data")
	}
	return nil
}

func readAllBounded(reader io.Reader, limit int64) ([]byte, error) {
	buffer := &bytes.Buffer{}
	_, err := io.Copy(buffer, io.LimitReader(reader, limit))
	return buffer.Bytes(), err
}
