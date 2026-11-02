package database

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// github.go: the database half of "Sign in / Sign up with GitHub". Two
// concerns live here: the one-time browser handoff codes the OAuth callback
// mints (single use, short-lived, digest-only) and the federate step that
// resolves a verified GitHub identity to a local account — signing in an
// already-linked user, linking a verified email to an unlinked account, or
// creating a fresh account with its initial organization exactly like a
// password registration.

// ErrGitHubCodeInvalid reports an unknown or malformed handoff code.
var ErrGitHubCodeInvalid = errors.New("github sign-in code is invalid")

// ErrGitHubCodeExpired reports a handoff code whose short TTL has elapsed.
var ErrGitHubCodeExpired = errors.New("github sign-in code has expired")

// ErrGitHubCodeReplayed reports a handoff code that was already redeemed. The
// one-time rule is what keeps a leaked redirect URL from minting a session.
var ErrGitHubCodeReplayed = errors.New("github sign-in code was already used")

// ErrGitHubEmailMismatch reports a linked account whose stored email no
// longer matches the GitHub account's verified primary email. The two sides
// disagree about who this is, so the sign-in is refused rather than silently
// following one of them.
var ErrGitHubEmailMismatch = errors.New("the verified github email does not match this account's email")

// ErrGitHubAccountConflict reports a verified email that belongs to a local
// account already linked to a different GitHub identity. Email alone must
// not swap one GitHub account for another.
var ErrGitHubAccountConflict = errors.New("this account is already linked to a different github identity")

// GitHubAuthCodeRecord is the resolved identity a handoff code carries. It
// holds no GitHub token: the OAuth access token dies at the callback, and
// only the identity facts the redeem call needs cross the browser.
type GitHubAuthCodeRecord struct {
	GitHubID    int64
	Login       string
	Email       string
	DisplayName string
}

// GitHubIdentity is a verified GitHub account as the callback resolved it:
// the stable numeric id, the login handle, and the primary verified email.
type GitHubIdentity struct {
	GitHubID    int64
	Login       string
	Email       string
	DisplayName string
}

// CreateGitHubAuthCode stores the handoff code under its peppered digest with
// the resolved identity and a short expiry. Only the digest is persisted, so
// a database leak does not leak redeemable codes — the same rule sessions
// and API keys follow.
func (store *Store) CreateGitHubAuthCode(ctx context.Context, digest []byte, record GitHubAuthCodeRecord, expiresAt time.Time) error {
	// Opportunistic garbage collection: every code is dead within minutes of
	// its expiry, so day-old rows are pure residue. Sweeping them here (a
	// bounded delete on an existing write path) keeps the table small without
	// adding a background worker for an auth-day-scale table.
	if _, err := store.pool.Exec(ctx, `DELETE FROM github_auth_codes WHERE created_at < now() - interval '24 hours'`); err != nil {
		return err
	}
	_, err := store.pool.Exec(ctx, `INSERT INTO github_auth_codes(digest,github_id,github_login,email,display_name,expires_at)
		VALUES($1,$2,$3,$4,$5,$6)`, digest, record.GitHubID, record.Login, record.Email, record.DisplayName, expiresAt)
	return err
}

// ConsumeGitHubAuthCode atomically claims a handoff code for exactly one
// redeem: the UPDATE sets consumed_at only on an unclaimed, unexpired row, so
// two concurrent redeems cannot both win. A failed claim is then diagnosed
// against the row state — replayed, expired, or never issued — so the API
// can answer with the honest typed error.
func (store *Store) ConsumeGitHubAuthCode(ctx context.Context, digest []byte, now time.Time) (GitHubAuthCodeRecord, error) {
	var record GitHubAuthCodeRecord
	var expiresAt time.Time
	err := store.pool.QueryRow(ctx, `UPDATE github_auth_codes SET consumed_at=$2
		WHERE digest=$1 AND consumed_at IS NULL AND expires_at > $2
		RETURNING github_id,github_login,email,display_name,expires_at`, digest, now).
		Scan(&record.GitHubID, &record.Login, &record.Email, &record.DisplayName, &expiresAt)
	switch {
	case err == nil:
		return record, nil
	case IsNotFound(err):
		var consumedAt *time.Time
		diagnostic := store.pool.QueryRow(ctx, `SELECT consumed_at FROM github_auth_codes WHERE digest=$1`, digest)
		if scanErr := diagnostic.Scan(&consumedAt); scanErr != nil {
			if IsNotFound(scanErr) {
				return GitHubAuthCodeRecord{}, ErrGitHubCodeInvalid
			}
			return GitHubAuthCodeRecord{}, scanErr
		}
		if consumedAt != nil {
			return GitHubAuthCodeRecord{}, ErrGitHubCodeReplayed
		}
		// The row exists and is unclaimed, so the claim must have failed on
		// the expiry window.
		return GitHubAuthCodeRecord{}, ErrGitHubCodeExpired
	default:
		return GitHubAuthCodeRecord{}, err
	}
}

// FederateGitHub resolves a verified GitHub identity to a local account in
// one transaction, with the same three-way decision the SSO federation makes
// plus the initial-organization case registration owns:
//
//   - an account already carrying this github_id is signed in as-is;
//   - an account with this email and no GitHub identity is linked to it;
//   - an account with this email bound to a *different* GitHub identity is
//     refused — email possession must not swap the link;
//   - otherwise a fresh account is created with no usable password and an
//     initial organization, exactly like a password registration.
//
// The email is always the primary *verified* one the GitHub API vouched for,
// never a user-supplied claim.
func (store *Store) FederateGitHub(ctx context.Context, identity GitHubIdentity, userID, organizationID, organizationName string, audit AuditInput) (UserRecord, OrganizationRecord, bool, error) {
	identity.Email = strings.ToLower(strings.TrimSpace(identity.Email))
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UserRecord{}, OrganizationRecord{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var user UserRecord
	userCreated := false
	err = tx.QueryRow(ctx, `SELECT id,email,password_hash,display_name,disabled_at,created_at,external_id,sso_issuer,sso_subject,github_login FROM users WHERE github_id=$1`, identity.GitHubID).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.DisabledAt, &user.CreatedAt, &user.ExternalID, &user.SSOIssuer, &user.SSOSubject, &user.GitHubLogin)
	user.GitHubID = &identity.GitHubID
	switch {
	case err == nil:
		// Returning identity: the stored email must still be the one GitHub
		// verifies, or this GitHub account and this local account no longer
		// agree on who is signing in. A stale handle is refreshed; a stale
		// address is refused up in the handler as GITHUB_EMAIL_MISMATCH.
		if !strings.EqualFold(user.Email, identity.Email) {
			return UserRecord{}, OrganizationRecord{}, false, ErrGitHubEmailMismatch
		}
		if identity.Login != "" && identity.Login != user.GitHubLogin {
			if _, err := tx.Exec(ctx, `UPDATE users SET github_login=$2,updated_at=now() WHERE id=$1`, user.ID, identity.Login); err != nil {
				return UserRecord{}, OrganizationRecord{}, false, err
			}
			user.GitHubLogin = identity.Login
		}
	case IsNotFound(err):
		user, userCreated, err = store.federateGitHubByEmail(ctx, tx, identity, userID, organizationID, organizationName)
		if err != nil {
			return UserRecord{}, OrganizationRecord{}, false, err
		}
	default:
		return UserRecord{}, OrganizationRecord{}, false, err
	}
	if user.DisabledAt != nil {
		return UserRecord{}, OrganizationRecord{}, false, ErrUserDisabled
	}
	organization := OrganizationRecord{}
	if userCreated {
		organization = OrganizationRecord{ID: organizationID, Name: organizationName, Role: "owner", CreatedAt: user.CreatedAt}
		audit.Action = "user.register"
	} else {
		audit.Action = "user.github_login"
	}
	audit.ResourceType = "user"
	audit.ResourceID = user.ID
	audit.ActorUserID = user.ID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return UserRecord{}, OrganizationRecord{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UserRecord{}, OrganizationRecord{}, false, err
	}
	return user, organization, userCreated, nil
}

// federateGitHubByEmail links an existing email account to the GitHub
// identity or creates a fresh account with its initial organization — the
// registration inserts verbatim — inside the caller's transaction. The
// boolean reports whether a new account was created.
func (store *Store) federateGitHubByEmail(ctx context.Context, tx pgx.Tx, identity GitHubIdentity, userID, organizationID, organizationName string) (UserRecord, bool, error) {
	if identity.Email == "" {
		return UserRecord{}, false, errors.New("a github login without an existing account must carry a verified email")
	}
	var user UserRecord
	err := tx.QueryRow(ctx, `SELECT id,email,password_hash,display_name,disabled_at,created_at,external_id,sso_issuer,sso_subject,github_id,github_login FROM users WHERE lower(email)=lower($1) FOR UPDATE`, identity.Email).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.DisabledAt, &user.CreatedAt, &user.ExternalID, &user.SSOIssuer, &user.SSOSubject, &user.GitHubID, &user.GitHubLogin)
	switch {
	case err == nil:
		if user.GitHubID != nil && *user.GitHubID != identity.GitHubID {
			return UserRecord{}, false, ErrGitHubAccountConflict
		}
		// An SSO-federated account belongs to its identity provider: the
		// issuer asserts the address space, and a GitHub link would open a
		// second door that enforcement cannot close. Password accounts link
		// freely — GitHub verifies the mailbox itself.
		if user.SSOIssuer != "" {
			return UserRecord{}, false, ErrSSOAccountLinked
		}
		if user.DisabledAt != nil {
			return UserRecord{}, false, ErrUserDisabled
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET github_id=$2,github_login=$3,updated_at=now() WHERE id=$1`, user.ID, identity.GitHubID, identity.Login); err != nil {
			return UserRecord{}, false, err
		}
		user.GitHubID = &identity.GitHubID
		user.GitHubLogin = identity.Login
		return user, false, nil
	case IsNotFound(err):
		displayName := identity.DisplayName
		if displayName == "" {
			displayName = identity.Login
		}
		if displayName == "" {
			displayName = identity.Email
		}
		created := time.Now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,display_name,github_id,github_login,created_at,updated_at) VALUES($1,$2,'',$3,$4,$5,$6,$6)`,
			userID, identity.Email, displayName, identity.GitHubID, identity.Login, created); err != nil {
			return UserRecord{}, false, err
		}
		// The initial organization is registration's contract for a new
		// account (owner membership, entitlements row) so a GitHub sign-up
		// lands on the same footing as a password one.
		if _, err := tx.Exec(ctx, `INSERT INTO organizations(id,name,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, organizationID, organizationName, userID, created); err != nil {
			return UserRecord{}, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,created_at) VALUES($1,$2,'owner',$3)`, organizationID, userID, created); err != nil {
			return UserRecord{}, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO entitlements(organization_id) VALUES($1)`, organizationID); err != nil {
			return UserRecord{}, false, err
		}
		return UserRecord{ID: userID, Email: identity.Email, DisplayName: displayName, GitHubID: &identity.GitHubID, GitHubLogin: identity.Login, CreatedAt: created}, true, nil
	default:
		return UserRecord{}, false, err
	}
}
