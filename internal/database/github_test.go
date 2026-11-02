package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// github_test.go: the database half of "Sign in with GitHub" — the three-way
// federate resolution (sign in / link / sign up), its typed refusals, the
// one-time handoff-code rule, and the erasure scrub of the GitHub identity.
// PostgreSQL-gated.

// githubIdentity builds a verified identity with run-unique coordinates so
// the shared test database never collides across runs.
func githubIdentity(t *testing.T, id int64, label string) GitHubIdentity {
	t.Helper()
	suffix, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return GitHubIdentity{GitHubID: id, Login: label + "-" + suffix, Email: label + "-" + suffix + "@gh-" + suffix + ".test", DisplayName: "GH " + label}
}

func githubAudit(userID string) AuditInput {
	return AuditInput{ID: "audit-" + userID, Action: "user.github_login", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{}, RemoteAddr: "192.0.2.10"}
}

// githubFederate runs one FederateGitHub call with fresh ids. The audit row
// names the github login in its metadata so the erasure scrub has real PII
// to destroy.
func githubFederate(t *testing.T, store *Store, identity GitHubIdentity) (UserRecord, OrganizationRecord, bool) {
	t.Helper()
	userID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	organizationID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	audit := githubAudit(userID)
	audit.Metadata = map[string]any{"github_login": identity.Login}
	user, organization, created, err := store.FederateGitHub(context.Background(), identity, userID, organizationID, identity.DisplayName+"'s organization", audit)
	if err != nil {
		t.Fatal(err)
	}
	return user, organization, created
}

// TestFederateGitHubSignsUpSignsInAndLinks: an unknown verified email creates
// the account and its initial organization with registration's full insert
// set (owner membership + entitlements); the same GitHub identity signs back
// in unchanged; a verified email matching an unlinked account attaches the
// identity to it instead of creating anyone.
func TestFederateGitHubSignsUpSignsInAndLinks(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	identity := githubIdentity(t, 880001, "signup")
	signUpUser, organization, created := githubFederate(t, store, identity)
	if !created {
		t.Fatal("an unknown email must create the account")
	}
	if signUpUser.GitHubID == nil || *signUpUser.GitHubID != identity.GitHubID || signUpUser.GitHubLogin != identity.Login {
		t.Fatalf("sign-up user carries no github identity: %#v", signUpUser)
	}
	if signUpUser.PasswordHash != "" {
		t.Fatal("a github-created account must carry no usable password hash")
	}
	if organization.ID == "" || organization.Role != "owner" {
		t.Fatalf("sign-up organization = %#v, want the initial owner org", organization)
	}
	var members, entitlements int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM organization_members WHERE organization_id=$1 AND user_id=$2 AND role='owner'`, organization.ID, signUpUser.ID).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE organization_id=$1`, organization.ID).Scan(&entitlements); err != nil {
		t.Fatal(err)
	}
	if members != 1 || entitlements != 1 {
		t.Fatalf("sign-up inserts incomplete: members=%d entitlements=%d", members, entitlements)
	}

	signInUser, _, created := githubFederate(t, store, identity)
	if created {
		t.Fatal("the same github identity must not create a second account")
	}
	if signInUser.ID != signUpUser.ID {
		t.Fatalf("sign-in resolved user %s, want %s", signInUser.ID, signUpUser.ID)
	}

	// Link: the verified email of an existing unlinked account attaches the
	// GitHub identity and resolves to that account.
	linkedEmail := "link-" + identity.Login + "@example.test"
	linkedID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	linkedOrgID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	audit := githubAudit(linkedID)
	audit.Action = "user.register"
	if _, _, err := store.Register(ctx, linkedID, linkedEmail, "hash-not-checked", "Linked User", linkedOrgID, "Linked Org", audit); err != nil {
		t.Fatal(err)
	}
	linkIdentity := identity
	linkIdentity.GitHubID = identity.GitHubID + 1
	linkIdentity.Login = identity.Login + "-linker"
	linkIdentity.Email = linkedEmail
	linkUser, _, created := githubFederate(t, store, linkIdentity)
	if created {
		t.Fatal("a known email must link, not create")
	}
	if linkUser.ID != linkedID {
		t.Fatalf("link resolved user %s, want the registered %s", linkUser.ID, linkedID)
	}
	if linkUser.GitHubID == nil || *linkUser.GitHubID != linkIdentity.GitHubID {
		t.Fatalf("link did not attach the github identity: %#v", linkUser)
	}
}

// TestFederateGitHubTypedRefusals: an account whose email no longer matches
// the verified GitHub email is refused (mismatch), an email bound to another
// GitHub identity is refused (conflict), and a disabled account is refused
// before anything is issued.
func TestFederateGitHubTypedRefusals(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	identity := githubIdentity(t, 880101, "refuse")
	user, _, _ := githubFederate(t, store, identity)

	mismatch := identity
	mismatch.Email = "elsewhere-" + identity.Login + "@example.test"
	userID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	organizationID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.FederateGitHub(ctx, mismatch, userID, organizationID, "Org", githubAudit(userID)); !errors.Is(err, ErrGitHubEmailMismatch) {
		t.Fatalf("mismatched email must refuse with ErrGitHubEmailMismatch, got %v", err)
	}

	conflict := identity
	conflict.GitHubID = identity.GitHubID + 1
	conflict.Login = identity.Login + "-impostor"
	userID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	organizationID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.FederateGitHub(ctx, conflict, userID, organizationID, "Org", githubAudit(userID)); !errors.Is(err, ErrGitHubAccountConflict) {
		t.Fatalf("identity swap must refuse with ErrGitHubAccountConflict, got %v", err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, user.ID); err != nil {
		t.Fatal(err)
	}
	userID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	organizationID, err = model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.FederateGitHub(ctx, identity, userID, organizationID, "Org", githubAudit(userID)); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("a disabled account must refuse with ErrUserDisabled, got %v", err)
	}
}

// TestGitHubAuthCodeIsSingleUse: a handoff code redeems exactly once. The
// replay is refused with its own typed error, a never-issued digest is
// refused as invalid, and an expired code is refused as expired — even
// though it was never redeemed.
func TestGitHubAuthCodeIsSingleUse(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	suffix, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	record := GitHubAuthCodeRecord{GitHubID: 880201, Login: "once-" + suffix, Email: "once-" + suffix + "@example.test", DisplayName: "Once"}
	digest := []byte("digest-once-" + suffix)
	now := time.Now().UTC()
	if err := store.CreateGitHubAuthCode(ctx, digest, record, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	redeemed, err := store.ConsumeGitHubAuthCode(ctx, digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if redeemed.GitHubID != record.GitHubID || redeemed.Login != record.Login || redeemed.Email != record.Email {
		t.Fatalf("redeemed record = %#v, want %#v", redeemed, record)
	}
	if _, err := store.ConsumeGitHubAuthCode(ctx, digest, now); !errors.Is(err, ErrGitHubCodeReplayed) {
		t.Fatalf("replay must refuse with ErrGitHubCodeReplayed, got %v", err)
	}
	if _, err := store.ConsumeGitHubAuthCode(ctx, []byte("digest-unknown-"+suffix), now); !errors.Is(err, ErrGitHubCodeInvalid) {
		t.Fatalf("unknown digest must refuse with ErrGitHubCodeInvalid, got %v", err)
	}
	expiredDigest := []byte("digest-expired-" + suffix)
	if err := store.CreateGitHubAuthCode(ctx, expiredDigest, record, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeGitHubAuthCode(ctx, expiredDigest, now); !errors.Is(err, ErrGitHubCodeExpired) {
		t.Fatalf("expired code must refuse with ErrGitHubCodeExpired, got %v", err)
	}
}

// TestGitHubAuthCodePurgesStaleRows: handoff rows are garbage a day after
// minting; creating a new code sweeps them so the table stays small.
func TestGitHubAuthCodePurgesStaleRows(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	suffix, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	staleDigest := []byte("digest-stale-" + suffix)
	if _, err := store.pool.Exec(ctx, `INSERT INTO github_auth_codes(digest,github_id,github_login,email,expires_at,created_at) VALUES($1,1,$2,$3,now()-interval '2 days',now()-interval '2 days')`,
		staleDigest, "stale-"+suffix, "stale-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	freshDigest := []byte("digest-fresh-" + suffix)
	if err := store.CreateGitHubAuthCode(ctx, freshDigest, GitHubAuthCodeRecord{GitHubID: 2, Login: "fresh-" + suffix, Email: "fresh-" + suffix + "@example.test"}, time.Now().UTC().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var stale, fresh int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM github_auth_codes WHERE digest=$1`, staleDigest).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM github_auth_codes WHERE digest=$1`, freshDigest).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatal("a day-old handoff row must be purged on the next create")
	}
	if fresh != 1 {
		t.Fatal("the fresh handoff row must survive the sweep")
	}
}

// TestGitHubIdentityIsUniqueWhenPresent: the partial unique index keeps one
// GitHub account from ever opening two local users.
func TestGitHubIdentityIsUniqueWhenPresent(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	identity := githubIdentity(t, 880301, "unique")
	user, _, _ := githubFederate(t, store, identity)
	otherID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,display_name,github_id,github_login,created_at,updated_at) VALUES($1,$2,'','Dup',$3,$4,now(),now())`,
		otherID, "dup-"+identity.Login+"@example.test", identity.GitHubID, "dup")
	if err == nil {
		t.Fatal("a second user with the same github_id must be refused")
	}
	if !IsConflict(err) {
		t.Fatalf("duplicate github_id must be a conflict, got %v", err)
	}
	if user.GitHubID == nil {
		t.Fatal("the original user lost its github identity")
	}
}

// TestEraseUserScrubsGitHubIdentity: erasure severs the GitHub link so "Sign
// in with GitHub" cannot re-activate the erased account, and the handle is
// scrubbed as a personal identifier.
func TestEraseUserScrubsGitHubIdentity(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	identity := githubIdentity(t, 880401, "erasure")
	user, _, _ := githubFederate(t, store, identity)
	if _, err := store.EraseUser(ctx, user.ID, AuditInput{ID: "audit-erase-" + user.ID, Action: "user.erased", ResourceType: "user", ResourceID: user.ID, Metadata: map[string]any{"github_login": identity.Login}}); err != nil {
		t.Fatal(err)
	}
	var githubID *int64
	var githubLogin string
	if err := store.pool.QueryRow(ctx, `SELECT github_id, github_login FROM users WHERE id=$1`, user.ID).Scan(&githubID, &githubLogin); err != nil {
		t.Fatal(err)
	}
	if githubID != nil || githubLogin != "" {
		t.Fatalf("erasure must sever the github identity, got id=%v login=%q", githubID, githubLogin)
	}
	// The federation audit row named the github login; the scrub must have
	// destroyed it there. (The erasure event's own row deliberately keeps
	// its metadata — it is a security event, not erased data.)
	var metadata string
	if err := store.pool.QueryRow(ctx, `SELECT metadata::text FROM audit_events WHERE id=$1`, "audit-"+user.ID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metadata, identity.Login) {
		t.Fatalf("erasure audit scrub still names the github login: %s", metadata)
	}
}
