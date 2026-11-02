package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"shift.dev/shift/internal/database"
)

// referral_stats_regression_test.go: the report 16 referral self-dealing and
// multi-code-claim defenses on the HTTP surface, and the scoreboard's admin
// gate plus banded-money contract (report 16 MEDIUM / report 27 HIGH).
// PostgreSQL-gated like the rest of the integration suite.

// TestReferralClaimRefusesSelfDealingAndSecondCodes: a referral code cannot
// be claimed by its own organization, by a same-email-domain second
// organization (self-dealing via a second org), or as a second code by an
// organization that already claimed one — one referred organization earns one
// reward, ever (report 16 HIGH).
func TestReferralClaimRefusesSelfDealingAndSecondCodes(t *testing.T) {
	httpServer, _, _, _, _ := openRegressionServer(t)
	client := httpServer.Client()

	orgA, authA := registerOrgWith(t, httpServer, "ref-owner-a@alpha.test", "Referral Alpha")
	orgB, authB := registerOrgWith(t, httpServer, "ref-owner-b@alpha.test", "Referral Alpha Two")
	orgC, authC := registerOrgWith(t, httpServer, "ref-owner-c@beta.test", "Referral Beta")

	var stats database.ReferralStats
	getJSON(t, client, httpServer.URL+"/v1/organizations/"+orgA+"/referral", http.StatusOK, &stats, authA)
	if stats.Code == "" {
		t.Fatal("referral stats did not issue a code")
	}
	var statsB database.ReferralStats
	getJSON(t, client, httpServer.URL+"/v1/organizations/"+orgB+"/referral", http.StatusOK, &statsB, authB)
	var statsC database.ReferralStats
	getJSON(t, client, httpServer.URL+"/v1/organizations/"+orgC+"/referral", http.StatusOK, &statsC, authC)

	// Same organization claiming its own code.
	var selfClaim ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+orgA+"/referral/claim", ReferralClaimRequest{Code: stats.Code}, http.StatusUnprocessableEntity, &selfClaim, authA)
	if selfClaim.Code != "REFERRAL_SELF_CLAIM" {
		t.Fatalf("same-org claim error = %#v", selfClaim)
	}

	// Same email domain, different organization: classic self-dealing.
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+orgB+"/referral/claim", ReferralClaimRequest{Code: stats.Code}, http.StatusUnprocessableEntity, &selfClaim, authB)
	if selfClaim.Code != "REFERRAL_SELF_CLAIM" {
		t.Fatalf("same-domain claim error = %#v", selfClaim)
	}

	// A genuinely different organization claims successfully.
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+orgC+"/referral/claim", ReferralClaimRequest{Code: stats.Code}, http.StatusOK, nil, authC)

	// A second, different code by the same referred organization is refused:
	// no multi-code farming, one reward per referred organization.
	var already ErrorResponse
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+orgC+"/referral/claim", ReferralClaimRequest{Code: statsB.Code}, http.StatusConflict, &already, authC)
	if already.Code != "REFERRAL_ALREADY_CLAIMED" {
		t.Fatalf("second-claim error = %#v", already)
	}

	// Retrying the same code is idempotent, not a second event.
	postJSON(t, client, httpServer.URL+"/v1/organizations/"+orgC+"/referral/claim", ReferralClaimRequest{Code: stats.Code}, http.StatusOK, nil, authC)

	// The owner's scoreboard counts exactly one invited organization.
	getJSON(t, client, httpServer.URL+"/v1/organizations/"+orgA+"/referral", http.StatusOK, &stats, authA)
	if stats.Invited != 1 {
		t.Fatalf("invited = %d, want exactly 1 (only the genuine claim)", stats.Invited)
	}
}

// TestStatsScoreboardRequiresAdminGrant: the platform-wide scoreboard reads
// only on platform-operator credentials — an admin-scope API key or the
// metrics scrape token. A user session is refused whatever its organization
// role (no customer account reads platform-wide metrics), a machines-scope
// agent key used to read exact revenue and is refused, and the payload is
// counts plus banded money for everyone (report 16 MEDIUM / report 27 HIGH).
func TestStatsScoreboardRequiresAdminGrant(t *testing.T) {
	httpServer, server, _, organizationID, authHeader := openRegressionServer(t)
	client := httpServer.Client()
	organizationPath := httpServer.URL + "/v1/organizations/" + organizationID
	scoreboardURL := httpServer.URL + "/v1/admin/scoreboard"

	// An unauthenticated read is refused.
	expectStatus(t, client, http.MethodGet, scoreboardURL, http.StatusUnauthorized, "", nil)

	// The organization owner's session is refused: sessions are not platform
	// operator credentials.
	expectStatus(t, client, http.MethodGet, scoreboardURL, http.StatusForbidden, authHeader, nil)

	// A machines-scope key — the fleet's daily credential — is refused.
	var machinesKey APIKeyCreated
	postJSON(t, client, organizationPath+"/api-keys", CreateAPIKeyRequest{Name: "agent", Scopes: []string{"machines"}}, http.StatusCreated, &machinesKey, authHeader)
	expectStatus(t, client, http.MethodGet, scoreboardURL, http.StatusForbidden, "Bearer "+machinesKey.Secret, nil)

	// A read-scope key is refused too.
	var readKey APIKeyCreated
	postJSON(t, client, organizationPath+"/api-keys", CreateAPIKeyRequest{Name: "reader", Scopes: []string{"read"}}, http.StatusCreated, &readKey, authHeader)
	expectStatus(t, client, http.MethodGet, scoreboardURL, http.StatusForbidden, "Bearer "+readKey.Secret, nil)

	// The metrics scrape token is the operator shared secret: presented on a
	// request it authorizes platform-admin reads (here exercised at the
	// handler contract; the route-level wiring is the auth middleware's). The
	// payload carries a money band, never exact revenue.
	var body map[string]any
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/admin/scoreboard", nil)
	request.Header.Set("X-Metrics-Token", "regression-metrics-token")
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey, Principal{UserID: "user_1"}))
	recorder := httptest.NewRecorder()
	server.handleScoreboard(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics-token handler read: status %d body %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, exact := body["mrr_cents"]; exact {
		t.Fatalf("scoreboard leaked exact revenue: %v", body)
	}
	if band, ok := body["mrr_band_usd"].(string); !ok || band == "" {
		t.Fatalf("scoreboard carries no mrr_band_usd: %v", body)
	}
	if body["new_orgs"] == nil || body["migrations_attempted"] == nil {
		t.Fatalf("scoreboard lost its count fields: %v", body)
	}

	// An admin-scope key passes the gate.
	var adminKey APIKeyCreated
	postJSON(t, client, organizationPath+"/api-keys", CreateAPIKeyRequest{Name: "ops", Scopes: []string{"admin"}}, http.StatusCreated, &adminKey, authHeader)
	getJSON(t, client, scoreboardURL, http.StatusOK, &body, "Bearer "+adminKey.Secret)
	if _, exact := body["mrr_cents"]; exact {
		t.Fatalf("admin scoreboard leaked exact revenue: %v", body)
	}
}

// TestStoragePrefixRefusesGlobInjection: the organization ID is embedded in
// the STS session policy's ARN and s3:prefix globs — a wildcard there would
// read every tenant. The pure prefix builder refuses such IDs outright
// (report 16 LOW).
func TestStoragePrefixRefusesGlobInjection(t *testing.T) {
	for _, hostile := range []string{"", "org*", "org?", "org/../x", "org/x", "org x", "org\x00", "org\n"} {
		if prefix, err := storagePrefix(hostile); err == nil {
			t.Fatalf("hostile id %q produced prefix %q", hostile, prefix)
		}
	}
	prefix, err := storagePrefix("1c9e4c1a-2b3c-4d5e-8f90-0123456789ab")
	if err != nil || prefix != "org/1c9e4c1a-2b3c-4d5e-8f90-0123456789ab" {
		t.Fatalf("UUID id mishandled: %q %v", prefix, err)
	}
}

// TestStatsRevenueBandMatchesPublicShape: the scoreboard's money band is the same
// representation the public numbers page publishes — bands, not cents.
func TestStatsRevenueBandMatchesPublicShape(t *testing.T) {
	cases := map[int64]string{
		0:     "$0–100",
		50:    "$0–100",
		250:   "$100–500",
		900:   "$500–1k",
		2500:  "$1k–5k",
		25000: "$10k–50k",
		90000: "$50k+",
	}
	for dollars, want := range cases {
		if got := revenueBandUSD(dollars); got != want {
			t.Fatalf("revenueBandUSD(%d) = %q, want %q", dollars, got, want)
		}
	}
}
