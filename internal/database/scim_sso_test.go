package database

import (
	"context"
	"errors"
	"testing"

	"shift.dev/shift/internal/model"
)

// scim_sso_test.go: the SCIM filter parser (pure), the SCIM user store, and
// the SSO account linkage — all against the integration database, all skipped
// without SHIFT_TEST_DATABASE_URL.

func TestParseSCIMFilter(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		want   []SCIMClause
	}{
		{"exact email", `userName eq "a@b.test"`, []SCIMClause{{Attribute: "email", Operator: "eq", Value: "a@b.test"}}},
		{"emails path", `emails.value eq a@b.test`, []SCIMClause{{Attribute: "email", Operator: "eq", Value: "a@b.test"}}},
		{"two clauses", `userName co example and active eq true`, []SCIMClause{
			{Attribute: "email", Operator: "co", Value: "example"},
			{Attribute: "active", Operator: "eq", Value: "true"},
		}},
		{"prefix and suffix", `displayName sw "Ad" and userName ew .test`, []SCIMClause{
			{Attribute: "display_name", Operator: "sw", Value: "Ad"},
			{Attribute: "email", Operator: "ew", Value: ".test"},
		}},
		{"presence", `externalId pr`, []SCIMClause{{Attribute: "external_id", Operator: "pr"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ParseSCIMFilter(testCase.filter)
			if err != nil {
				t.Fatalf("ParseSCIMFilter(%q): %v", testCase.filter, err)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("ParseSCIMFilter(%q) = %+v, want %+v", testCase.filter, got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Fatalf("clause %d = %+v, want %+v", index, got[index], testCase.want[index])
				}
			}
		})
	}

	rejections := []string{
		"",                                  // empty is valid but yields nothing
		`userName eq`,                       // missing value
		`userName`,                          // missing operator
		`userName ne "x"`,                   // unsupported operator
		`password eq "hunter2"`,             // attribute the store never filters
		`userName eq "a" or displayName pr`, // unsupported connective
		`userName eq "a" and`,               // dangling and
		`userName eq "a(b)"`,                // unsupported characters in a value
		`userName eq "unterminated`,         // unterminated quote
		`active eq maybe`,                   // active compares booleans
		`(userName eq "a")`,                 // parentheses are not supported
		`userName pr extra`,                 // pr takes no value
	}
	for _, filter := range rejections {
		if _, err := ParseSCIMFilter(filter); err == nil && filter != "" {
			t.Fatalf("ParseSCIMFilter(%q) was accepted", filter)
		}
	}
}

// TestSCIMUserStore drives provisioning through the store: create, list with
// filters, replace, patch-shaped replaces, and deactivate.
func TestSCIMUserStore(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "scim")
	// Emails and external ids are unique per run: lower(email) and external_id
	// are globally unique in the schema, and these tests share one database
	// across runs.
	run, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.SCIMCreateUser(ctx, organizationID, SCIMUser{Email: "scim-one-" + run + "@example.test", DisplayName: "One", ExternalID: "ext-one-" + run}, testAudit(organizationID, "scim.user.create", "one"))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Active {
		t.Fatal("a created user starts active")
	}
	second, err := store.SCIMCreateUser(ctx, organizationID, SCIMUser{Email: "scim-two-" + run + "@example.test", DisplayName: "Two", ExternalID: "ext-two-" + run}, testAudit(organizationID, "scim.user.create", "two"))
	if err != nil {
		t.Fatal(err)
	}

	// A duplicate email or external id is a conflict.
	if _, err := store.SCIMCreateUser(ctx, organizationID, SCIMUser{Email: first.Email, DisplayName: "Other"}, testAudit(organizationID, "scim.user.create", "dup")); !errors.Is(err, ErrSCIMConflict) {
		t.Fatalf("duplicate email returned %v, want ErrSCIMConflict", err)
	}

	// Membership is the tenant boundary: another organization's caller sees
	// none of these users (its own members aside).
	other := testOrganization(t, store, "scim-other")
	strangers, err := store.SCIMUsers(ctx, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stranger := range strangers {
		if stranger.ID == first.ID || stranger.ID == second.ID {
			t.Fatalf("another organization sees provisioned user %+v", stranger)
		}
	}

	// Filters run through the parameterized renderer.
	filtered, err := store.SCIMUsers(ctx, organizationID, []SCIMClause{{Attribute: "email", Operator: "sw", Value: "scim-one-" + run}})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != first.ID {
		t.Fatalf("prefix filter returned %+v", filtered)
	}
	// The "scim" contains-filter also matches the founding member's email
	// (dbtest-scim-…): the external-id presence clause is what narrows the
	// conjunction to provisioned accounts. The listing is scoped to this
	// run's fresh organization, so other runs' users cannot appear.
	both, err := store.SCIMUsers(ctx, organizationID, []SCIMClause{{Attribute: "email", Operator: "co", Value: "scim"}, {Attribute: "external_id", Operator: "pr"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 {
		t.Fatalf("conjunction filter returned %d users", len(both))
	}

	// A full replacement changes every idP-controlled field.
	replaced, err := store.SCIMReplaceUser(ctx, organizationID, first.ID, SCIMUser{Email: "renamed-" + run + "@example.test", DisplayName: "Renamed", ExternalID: "ext-one-b-" + run, Active: false}, testAudit(organizationID, "scim.user.replace", first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if replaced.Email != "renamed-"+run+"@example.test" || replaced.Active {
		t.Fatalf("replacement returned %+v", replaced)
	}

	// Re-activation through the same path.
	reactivated, err := store.SCIMSetActive(ctx, organizationID, first.ID, true, testAudit(organizationID, "scim.user.patch", first.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !reactivated.Active {
		t.Fatalf("re-activation returned %+v", reactivated)
	}

	// Deactivation is SCIM's delete; the row survives and stops appearing in
	// active filters.
	deactivated, err := store.SCIMSetActive(ctx, organizationID, second.ID, false, testAudit(organizationID, "scim.user.delete", second.ID))
	if err != nil {
		t.Fatal(err)
	}
	if deactivated.Active {
		t.Fatalf("deactivation returned %+v", deactivated)
	}
	stillThere, err := store.SCIMUserByID(ctx, organizationID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillThere.Active {
		t.Fatalf("the deactivated row did not survive: %+v", stillThere)
	}
	activeOnly, err := store.SCIMUsers(ctx, organizationID, []SCIMClause{{Attribute: "active", Operator: "eq", Value: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range activeOnly {
		if user.ID == second.ID {
			t.Fatalf("deactivated user still answers an active filter: %+v", user)
		}
	}

	// Unknown users are not found in this organization.
	if _, err := store.SCIMUserByID(ctx, organizationID, "no-such-user"); !IsNotFound(err) {
		t.Fatalf("unknown user returned %v", err)
	}
}

// TestFederateSSOLink walks account linkage: a returning identity refreshes,
// a password account is linked, a mismatched identity is refused, and the
// enforcing organization gains the member.
func TestFederateSSOLink(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "sso")
	// The enforced domain, the federated subjects, and both emails are unique
	// per run: an organization's email domain, (sso_issuer, sso_subject), and
	// lower(email) are each globally unique in the schema, and these tests
	// share one database across runs.
	run, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	domain := "federated-" + run + ".example.test"
	issuer := "https://issuer.example.test"
	newcomer := "newcomer@" + domain
	if err := store.SetOrganizationSSO(ctx, organizationID, domain, true, testAudit(organizationID, "organization.sso", organizationID)); err != nil {
		t.Fatal(err)
	}

	// The domain is now enforced.
	enforced, err := store.SSOEnforcedForEmail(ctx, newcomer)
	if err != nil || !enforced {
		t.Fatalf("enforcement lookup returned %v, %v", enforced, err)
	}

	// A first login provisions the account.
	first, err := store.FederateSSOLink(ctx, issuer, "subject-"+run+"-1", newcomer, "New Comer", testAudit(organizationID, "user.sso_login", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if first.SSOIssuer != issuer || first.SSOSubject != "subject-"+run+"-1" {
		t.Fatalf("linkage did not stick: %+v", first)
	}
	if first.PasswordHash != "" {
		t.Fatal("a federated account carries a password hash")
	}

	// The enforcing organization gained a member.
	role, err := store.MemberRole(ctx, organizationID, first.ID)
	if err != nil || role != "viewer" {
		t.Fatalf("federated member role is %q, %v", role, err)
	}

	// The same identity returns the same account.
	again, err := store.FederateSSOLink(ctx, issuer, "subject-"+run+"-1", newcomer, "New Comer", testAudit(organizationID, "user.sso_login", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("returning identity created a second account %q", again.ID)
	}

	// A different subject claiming the same email is refused.
	if _, err := store.FederateSSOLink(ctx, issuer, "subject-"+run+"-2", newcomer, "Impostor", testAudit(organizationID, "user.sso_login", "x")); !errors.Is(err, ErrSSOAccountLinked) {
		t.Fatalf("impostor login returned %v, want ErrSSOAccountLinked", err)
	}

	// A pre-existing password account is linked, not duplicated.
	passwordUserID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	passwordOrganizationID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register(ctx, passwordUserID, "existing-"+run+"@mixed.example.test", "argon2-hash", "Existing", passwordOrganizationID, "Mixed", AuditInput{ID: "audit-" + passwordUserID, OrganizationID: passwordOrganizationID, ActorUserID: passwordUserID, Action: "user.register", ResourceType: "user", ResourceID: passwordUserID, Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	linked, err := store.FederateSSOLink(ctx, issuer, "subject-"+run+"-3", "existing-"+run+"@mixed.example.test", "Existing", testAudit(organizationID, "user.sso_login", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if linked.ID != passwordUserID || linked.PasswordHash != "argon2-hash" {
		t.Fatalf("existing account was not linked in place: %+v", linked)
	}

	// A disabled account is refused.
	if _, err := store.SCIMSetActive(ctx, organizationID, first.ID, false, testAudit(organizationID, "scim.user.delete", first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FederateSSOLink(ctx, issuer, "subject-"+run+"-1", newcomer, "New Comer", testAudit(organizationID, "user.sso_login", "x")); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("disabled login returned %v, want ErrUserDisabled", err)
	}
}
