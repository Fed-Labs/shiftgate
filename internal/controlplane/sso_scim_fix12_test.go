package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/oidc"
)

// sso_scim_fix12_test.go: readiness-wave regressions for the SSO domain
// ownership challenge, SCIM deactivation/re-enable session handling, SCIM
// create-active semantics, cross-tenant account protection, and the account
// link guard.

// stubSSODomainVerification answers the DNS ownership challenge as if the
// organization had published its verification record.
func stubSSODomainVerification(t *testing.T, organizationID string) {
	t.Helper()
	previous := ssoDomainTXTLookup
	ssoDomainTXTLookup = func(context.Context, string) ([]string, error) {
		return []string{ssoDomainChallengeValue(organizationID)}, nil
	}
	t.Cleanup(func() { ssoDomainTXTLookup = previous })
}

// fix12Token returns a run-unique suffix so shared-database runs never
// collide on emails, external ids, or claimed domains.
func fix12Token(t *testing.T) string {
	t.Helper()
	token, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// scimProvisioningKey mints the scim-scoped API key the provisioning
// endpoints demand.
func scimProvisioningKey(t *testing.T, httpServer *httptest.Server, organizationID, authHeader string) string {
	t.Helper()
	var key APIKeyCreated
	postJSON(t, httpServer.Client(), httpServer.URL+"/v1/organizations/"+organizationID+"/api-keys",
		CreateAPIKeyRequest{Name: "provisioning-fix12", Scopes: []string{"scim"}}, http.StatusCreated, &key, authHeader)
	return "Bearer " + key.Secret
}

// ssoLoginExpect walks the full federated round trip like ssoLogin but
// expects a specific status at the token step.
func ssoLoginExpect(t *testing.T, httpServer *httptest.Server, idp *fakeIdentityProvider, expectedStatus int) {
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
	browser := &http.Client{Transport: idp.server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, authorization.AuthorizationURL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	response, err := browser.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	location, err := response.Location()
	if err != nil {
		t.Fatal(err)
	}
	requestJSON(t, client, http.MethodPost, httpServer.URL+"/v1/auth/sso/token",
		map[string]string{"code": location.Query().Get("code"), "state": authorization.State, "code_verifier": verifier, "redirect_uri": redirectURI},
		expectedStatus, &map[string]any{}, "")
}

// TestSSODomainOwnershipVerification exercises the ownership challenge
// directly: an unproven domain never verifies, another organization's record
// never verifies, and public providers are unclaimable.
func TestSSODomainOwnershipVerification(t *testing.T) {
	previous := ssoDomainTXTLookup
	t.Cleanup(func() { ssoDomainTXTLookup = previous })

	ssoDomainTXTLookup = func(context.Context, string) ([]string, error) { return nil, nil }
	if err := verifySSODomainOwnership(context.Background(), "org-1", "example.test"); err == nil {
		t.Fatal("a domain with no verification record passed the ownership challenge")
	}

	ssoDomainTXTLookup = func(context.Context, string) ([]string, error) {
		return []string{ssoDomainChallengeValue("org-2")}, nil
	}
	if err := verifySSODomainOwnership(context.Background(), "org-1", "example.test"); err == nil {
		t.Fatal("a record naming another organization verified this claim")
	}

	sawChallenge := false
	ssoDomainTXTLookup = func(_ context.Context, name string) ([]string, error) {
		if name == "_shift-sso-verify.example.test" {
			sawChallenge = true
			return []string{" " + ssoDomainChallengeValue("org-1") + " "}, nil
		}
		return nil, nil
	}
	if err := verifySSODomainOwnership(context.Background(), "org-1", "example.test"); err != nil {
		t.Fatalf("a published verification record did not verify the claim: %v", err)
	}
	if !sawChallenge {
		t.Fatal("the ownership challenge did not consult _shift-sso-verify.<domain>")
	}

	ssoDomainTXTLookup = func(_ context.Context, name string) ([]string, error) {
		if name == "example.test" {
			return []string{ssoDomainChallengeValue("org-1")}, nil
		}
		return nil, nil
	}
	if err := verifySSODomainOwnership(context.Background(), "org-1", "example.test"); err != nil {
		t.Fatalf("an apex verification record did not verify the claim: %v", err)
	}

	for _, domain := range []string{"gmail.com", "mail.gmail.com", "outlook.com", "proton.me", "example.com"} {
		if !ssoDomainIsPublic(domain) {
			t.Fatalf("public provider %q is claimable", domain)
		}
	}
	if ssoDomainIsPublic("corp.example.test") {
		t.Fatal("a private domain was refused as public")
	}
}

// TestSSOProviderCacheReusesProvider proves one OIDC client survives across
// calls instead of re-running discovery and JWKS fetches per request.
func TestSSOProviderCacheReusesProvider(t *testing.T) {
	client := &http.Client{}
	first, err := ssoProviderFor("https://issuer.example.test", "client", "secret", client)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ssoProviderFor("https://issuer.example.test", "client", "secret", client)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("the OIDC provider was rebuilt for an unchanged configuration")
	}
	third, err := ssoProviderFor("https://other.example.test", "client", "secret", client)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("the OIDC provider cache ignored a changed issuer")
	}
}

// TestSSODomainClaimRequiresOwnershipProof drives the admin surface: without
// the DNS record the claim is refused, the domain stays open for password
// signups, foreign records do not authorize the claim, public providers are
// never claimable, and the claiming organization's own record succeeds.
func TestSSODomainClaimRequiresOwnershipProof(t *testing.T) {
	httpServer, organizationID, authHeader, idp := openSSOTestServer(t, true)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID

	previous := ssoDomainTXTLookup
	lookedUp := []string{}
	ssoDomainTXTLookup = func(_ context.Context, name string) ([]string, error) {
		lookedUp = append(lookedUp, name)
		return nil, nil
	}
	t.Cleanup(func() { ssoDomainTXTLookup = previous })

	expectStatus(t, client, http.MethodPut, organizationPath+"/sso", http.StatusBadRequest, authHeader,
		SetOrganizationSSORequest{Enforced: true, EmailDomain: idp.domain})
	foundChallengeName := false
	for _, name := range lookedUp {
		if name == "_shift-sso-verify."+idp.domain {
			foundChallengeName = true
		}
	}
	if !foundChallengeName {
		t.Fatalf("the claim consulted %v, not the documented challenge name", lookedUp)
	}

	// An unproven claim must not lock the domain's users out of password
	// signups.
	var required struct {
		Required bool `json:"required"`
	}
	getJSON(t, client, httpServer.URL+"/v1/auth/sso/required?email=person@"+idp.domain, http.StatusOK, &required, "")
	if required.Required {
		t.Fatal("an unverified claim started enforcing the domain")
	}
	token := fix12Token(t)
	postJSON(t, client, httpServer.URL+"/v1/auth/register", RegisterRequest{
		Email: "still-allowed-" + token + "@" + idp.domain, Password: "correct horse battery staple",
		DisplayName: "Still Allowed", Organization: "Still Allowed Org " + token,
	}, http.StatusCreated, &struct{}{}, "")

	// A record naming another organization never authorizes this one.
	ssoDomainTXTLookup = func(context.Context, string) ([]string, error) {
		return []string{ssoDomainChallengeValue("org-" + token)}, nil
	}
	expectStatus(t, client, http.MethodPut, organizationPath+"/sso", http.StatusBadRequest, authHeader,
		SetOrganizationSSORequest{Enforced: true, EmailDomain: idp.domain})

	// Public providers are unclaimable even with a perfect record.
	ssoDomainTXTLookup = func(context.Context, string) ([]string, error) {
		return []string{ssoDomainChallengeValue(organizationID)}, nil
	}
	expectStatus(t, client, http.MethodPut, organizationPath+"/sso", http.StatusBadRequest, authHeader,
		SetOrganizationSSORequest{Enforced: true, EmailDomain: "gmail.com"})

	// The claiming organization's own record claims the domain.
	requestJSON(t, client, http.MethodPut, organizationPath+"/sso",
		SetOrganizationSSORequest{Enforced: true, EmailDomain: idp.domain}, http.StatusOK, &SetOrganizationSSORequest{}, authHeader)
}

// TestSCIMDeactivationRevokesAndReenableDoesNotResurrect proves the standard
// IdP deprovision paths (PATCH/PUT active:false) kill live sessions the way
// DELETE does, and that a later re-enable never resurrects tokens issued
// before the deactivation.
func TestSCIMDeactivationRevokesAndReenableDoesNotResurrect(t *testing.T) {
	httpServer, organizationID, authHeader, idp := openSSOTestServer(t, true)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)

	// The account the issuer asserts, so it can hold a real session.
	var created scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath, map[string]any{"userName": idp.email, "externalId": fix12Token(t)},
		http.StatusCreated, &created, scimAuth)

	_, tokens := ssoLogin(t, httpServer, idp)
	var me Principal
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusOK, &me, "Bearer "+tokens.AccessToken)

	// PATCH deactivation revokes the live session.
	var summary scimUserSummary
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+created.ID,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}},
		http.StatusOK, &summary, scimAuth)
	if summary.Active {
		t.Fatalf("patch did not deactivate: %+v", summary)
	}
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusUnauthorized, &map[string]any{}, "Bearer "+tokens.AccessToken)

	// Re-activation must not resurrect the session.
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+created.ID,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": true}}},
		http.StatusOK, &summary, scimAuth)
	if !summary.Active {
		t.Fatalf("patch did not re-activate: %+v", summary)
	}
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusUnauthorized, &map[string]any{}, "Bearer "+tokens.AccessToken)

	// The account itself works again after a fresh login.
	_, fresh := ssoLogin(t, httpServer, idp)
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusOK, &me, "Bearer "+fresh.AccessToken)

	// PUT deactivation revokes the same way.
	requestJSON(t, client, http.MethodPut, usersPath+"/"+created.ID,
		map[string]any{"userName": idp.email, "active": false}, http.StatusOK, &summary, scimAuth)
	if summary.Active {
		t.Fatalf("replace did not deactivate: %+v", summary)
	}
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusUnauthorized, &map[string]any{}, "Bearer "+fresh.AccessToken)
}

// TestSCIMCreateInactiveProvisionsDisabledAccount proves POST honors
// active:false: the response says so, the stored account is disabled, and the
// staged account cannot authenticate through the issuer.
func TestSCIMCreateInactiveProvisionsDisabledAccount(t *testing.T) {
	httpServer, organizationID, authHeader, idp := openSSOTestServer(t, true)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)

	var created scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath,
		map[string]any{"userName": idp.email, "externalId": fix12Token(t), "active": false},
		http.StatusCreated, &created, scimAuth)
	if created.Active {
		t.Fatalf("SCIM create ignored active:false: %+v", created)
	}
	var fetched scimUserSummary
	getJSON(t, client, usersPath+"/"+created.ID, http.StatusOK, &fetched, scimAuth)
	if fetched.Active {
		t.Fatalf("the staged account is stored active: %+v", fetched)
	}
	ssoLoginExpect(t, httpServer, idp, http.StatusForbidden)

	// An explicit activation brings the staged account to life.
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+created.ID,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": true}}},
		http.StatusOK, &fetched, scimAuth)
	if !fetched.Active {
		t.Fatalf("activation did not take: %+v", fetched)
	}
	ssoLogin(t, httpServer, idp)
}

// TestSCIMReplaceWithoutActivePreservesState proves an attribute-sync PUT
// that omits active never re-enables a deactivated account.
func TestSCIMReplaceWithoutActivePreservesState(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)
	email := "sync-" + fix12Token(t) + "@scim.example.test"

	var user scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath, map[string]any{"userName": email}, http.StatusCreated, &user, scimAuth)
	expectStatus(t, client, http.MethodDelete, usersPath+"/"+user.ID, http.StatusNoContent, scimAuth, nil)

	var replaced scimUserSummary
	requestJSON(t, client, http.MethodPut, usersPath+"/"+user.ID,
		map[string]any{"userName": email, "displayName": "Attribute Sync"}, http.StatusOK, &replaced, scimAuth)
	if replaced.Active {
		t.Fatalf("PUT without active re-enabled the account: %+v", replaced)
	}

	email = "sync2-" + fix12Token(t) + "@scim.example.test"
	requestJSON(t, client, http.MethodPost, usersPath, map[string]any{"userName": email}, http.StatusCreated, &user, scimAuth)
	requestJSON(t, client, http.MethodPut, usersPath+"/"+user.ID,
		map[string]any{"userName": email, "displayName": "Attribute Sync"}, http.StatusOK, &replaced, scimAuth)
	if !replaced.Active {
		t.Fatalf("PUT without active disabled a live account: %+v", replaced)
	}
}

// TestSCIMConflictMapsToConflictOnPatch proves a patch that violates the
// unique email or external id answers 409 like create and replace, not 500.
func TestSCIMConflictMapsToConflictOnPatch(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)
	token := fix12Token(t)

	var first, second scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath,
		map[string]any{"userName": "first-" + token + "@scim.example.test", "externalId": "first-" + token},
		http.StatusCreated, &first, scimAuth)
	requestJSON(t, client, http.MethodPost, usersPath,
		map[string]any{"userName": "second-" + token + "@scim.example.test", "externalId": "second-" + token},
		http.StatusCreated, &second, scimAuth)

	expectStatus(t, client, http.MethodPatch, usersPath+"/"+second.ID, http.StatusConflict, scimAuth,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "userName", "value": "first-" + token + "@scim.example.test"}}})
	expectStatus(t, client, http.MethodPatch, usersPath+"/"+second.ID, http.StatusConflict, scimAuth,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "externalId", "value": "first-" + token}}})
}

// TestSCIMPatchEnforcesAttributeCaps proves the patch path enforces the same
// displayName and externalId length caps as create and replace.
func TestSCIMPatchEnforcesAttributeCaps(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)

	var user scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath,
		map[string]any{"userName": "caps-" + fix12Token(t) + "@scim.example.test"}, http.StatusCreated, &user, scimAuth)

	expectStatus(t, client, http.MethodPatch, usersPath+"/"+user.ID, http.StatusBadRequest, scimAuth,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "displayName", "value": strings.Repeat("d", 201)}}})
	expectStatus(t, client, http.MethodPatch, usersPath+"/"+user.ID, http.StatusBadRequest, scimAuth,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "externalId", "value": strings.Repeat("e", 257)}}})

	var patched scimUserSummary
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+user.ID,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "displayName", "value": strings.Repeat("d", 200)}}},
		http.StatusOK, &patched, scimAuth)
	if len(patched.DisplayName) != 200 {
		t.Fatalf("displayName cap rejected a value at the limit: %d", len(patched.DisplayName))
	}
}

// TestSCIMSharedAccountProtected proves one tenant's provisioning cannot
// disable, rename, or re-link an account that also belongs to another
// organization.
func TestSCIMSharedAccountProtected(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	usersPath := httpServer.URL + "/v1/scim/v2/Users"
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)
	token := fix12Token(t)

	var second struct {
		Organization Organization `json:"organization"`
	}
	postJSON(t, client, httpServer.URL+"/v1/auth/register", RegisterRequest{
		Email: "second-owner-" + token + "@org-" + token + ".test", Password: "correct horse battery staple",
		DisplayName: "Second Owner", Organization: "Second Org " + token,
	}, http.StatusCreated, &second, "")

	email := "shared-" + token + "@shared.example.test"
	var user scimUserSummary
	requestJSON(t, client, http.MethodPost, usersPath, map[string]any{"userName": email}, http.StatusCreated, &user, scimAuth)

	databaseURL := os.Getenv("SHIFT_TEST_DATABASE_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	if err := store.AddMember(ctx, second.Organization.ID, email, "viewer", 0, database.AuditInput{}); err != nil {
		t.Fatal(err)
	}

	// Account-wide changes from one tenant are refused.
	expectStatus(t, client, http.MethodPatch, usersPath+"/"+user.ID, http.StatusConflict, scimAuth,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}}})
	expectStatus(t, client, http.MethodPatch, usersPath+"/"+user.ID, http.StatusConflict, scimAuth,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "userName", "value": "renamed-" + token + "@shared.example.test"}}})
	expectStatus(t, client, http.MethodPut, usersPath+"/"+user.ID, http.StatusConflict, scimAuth,
		map[string]any{"userName": email, "active": false})
	expectStatus(t, client, http.MethodDelete, usersPath+"/"+user.ID, http.StatusConflict, scimAuth, nil)

	// Cosmetic attribute sync is still allowed, and the account is untouched.
	var patched scimUserSummary
	requestJSON(t, client, http.MethodPatch, usersPath+"/"+user.ID,
		map[string]any{"Operations": []map[string]any{{"op": "replace", "path": "displayName", "value": "Shared Person"}}},
		http.StatusOK, &patched, scimAuth)
	if !patched.Active || patched.DisplayName != "Shared Person" {
		t.Fatalf("displayName-only patch changed the wrong things: %+v", patched)
	}
}

// TestSCIMServiceProviderConfigAuthScheme proves discovery advertises the
// credential shape the API actually authenticates with.
func TestSCIMServiceProviderConfigAuthScheme(t *testing.T) {
	httpServer, organizationID, authHeader, _ := openSSOTestServer(t, false)
	client := httpServer.Client()
	scimAuth := scimProvisioningKey(t, httpServer, organizationID, authHeader)

	var providerConfig struct {
		AuthenticationSchemes []map[string]any `json:"authenticationSchemes"`
	}
	getJSON(t, client, httpServer.URL+"/v1/scim/v2/ServiceProviderConfig", http.StatusOK, &providerConfig, scimAuth)
	if len(providerConfig.AuthenticationSchemes) == 0 {
		t.Fatal("service provider config advertises no authentication scheme")
	}
	if providerConfig.AuthenticationSchemes[0]["type"] != "oauthbearertoken" {
		t.Fatalf("authentication scheme type is %v, want oauthbearertoken", providerConfig.AuthenticationSchemes[0]["type"])
	}
}

// TestSSOLinkRefusesUnfederatedPasswordAccounts proves the issuer cannot
// silently take over a password account on the email claim alone: the link is
// refused until the address's domain enforces SSO and the issuer is its
// declared authority.
func TestSSOLinkRefusesUnfederatedPasswordAccounts(t *testing.T) {
	httpServer, organizationID, authHeader, idp := openSSOTestServer(t, true)
	client := httpServer.Client()
	token := fix12Token(t)

	postJSON(t, client, httpServer.URL+"/v1/auth/register", RegisterRequest{
		Email: idp.email, Password: "correct horse battery staple",
		DisplayName: "Password Person", Organization: "Password Org " + token,
	}, http.StatusCreated, &struct{}{}, "")

	ssoLoginExpect(t, httpServer, idp, http.StatusConflict)

	requestJSON(t, client, http.MethodPut, httpServer.URL+"/v1/organizations/"+organizationID+"/sso",
		SetOrganizationSSORequest{Enforced: true, EmailDomain: idp.domain}, http.StatusOK, &SetOrganizationSSORequest{}, authHeader)

	user, tokens := ssoLogin(t, httpServer, idp)
	if user.Email != idp.email {
		t.Fatalf("federated login authenticated %q, want %q", user.Email, idp.email)
	}
	var me Principal
	getJSON(t, client, httpServer.URL+"/v1/me", http.StatusOK, &me, "Bearer "+tokens.AccessToken)
}
