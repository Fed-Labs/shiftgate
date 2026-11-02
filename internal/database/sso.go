package database

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"shift.dev/shift/internal/model"
)

// sso.go: single sign-on account linkage. An organization that enforces SSO
// claims an email domain; while enforcement is on, that domain's users
// authenticate through the control plane's configured OIDC issuer. Federated
// accounts are created with an empty password hash — argon2 verification of
// an empty string fails structurally, so no password can ever open them.

// ErrSSOAccountLinked is returned when an account already federated to one
// issuer arrives claiming a different subject: the account is not silently
// re-linked.
var ErrSSOAccountLinked = errors.New("this account is already federated to a different identity")

// ErrUserDisabled is returned when a federated login targets an account the
// provisioning system deactivated.
var ErrUserDisabled = errors.New("this account is disabled")

// SSOEnforcedForEmail reports whether any organization enforces SSO over the
// domain of the given email. Password login for such an address must be
// refused, and registration directed to the issuer instead.
func (store *Store) SSOEnforcedForEmail(ctx context.Context, email string) (bool, error) {
	domain := EmailDomain(email)
	if domain == "" {
		return false, nil
	}
	var enforced bool
	err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE sso_enforced AND lower(sso_email_domain)=$1)`, domain).Scan(&enforced)
	return enforced, err
}

// EmailDomain returns the lower-cased domain of an email address, or an empty
// string when the address has none.
func EmailDomain(email string) string {
	_, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(email)), "@")
	if !ok {
		return ""
	}
	return strings.TrimSpace(domain)
}

// OrganizationSSO reads one organization's enforcement state.
func (store *Store) OrganizationSSO(ctx context.Context, organizationID string) (enforced bool, emailDomain string, err error) {
	err = store.pool.QueryRow(ctx, `SELECT sso_enforced, sso_email_domain FROM organizations WHERE id=$1`, organizationID).Scan(&enforced, &emailDomain)
	return enforced, emailDomain, err
}

// SetOrganizationSSO turns enforcement on or off for an email domain. Turning
// it off also clears the domain: re-enabling is a deliberate, audited act
// rather than a flag flip on a stale claim.
func (store *Store) SetOrganizationSSO(ctx context.Context, organizationID, emailDomain string, enforced bool, audit AuditInput) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `UPDATE organizations SET sso_enforced=$2,sso_email_domain=$3,updated_at=now() WHERE id=$1`, organizationID, enforced, strings.ToLower(strings.TrimSpace(emailDomain)))
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	audit.OrganizationID = organizationID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FederateSSOLink links an authenticated identity to a local account, or
// provisions one, in a single transaction:
//
//   - an account already carrying this (issuer, subject) is returned as-is;
//   - an account with this email and no federation is linked to it;
//   - otherwise a new account is created with no usable password;
//   - in every case, membership in every enforcing organization whose domain
//     matches the email is ensured.
func (store *Store) FederateSSOLink(ctx context.Context, issuer, subject, email, displayName string, audit AuditInput) (UserRecord, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UserRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var user UserRecord
	err = tx.QueryRow(ctx, `SELECT id,email,password_hash,display_name,disabled_at,created_at,external_id,sso_issuer,sso_subject FROM users WHERE sso_issuer=$1 AND sso_subject=$2`, issuer, subject).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.DisabledAt, &user.CreatedAt, &user.ExternalID, &user.SSOIssuer, &user.SSOSubject)
	switch {
	case err == nil:
		// Returning identity: refresh the display name if the issuer's is
		// newer, and fall through to membership enforcement below.
		if displayName != "" && displayName != user.DisplayName {
			if _, err := tx.Exec(ctx, `UPDATE users SET display_name=$2,updated_at=now() WHERE id=$1`, user.ID, displayName); err != nil {
				return UserRecord{}, err
			}
			user.DisplayName = displayName
		}
	case IsNotFound(err):
		user, err = store.federateByEmail(ctx, tx, issuer, subject, email, displayName)
		if err != nil {
			return UserRecord{}, err
		}
	default:
		return UserRecord{}, err
	}
	if user.DisabledAt != nil {
		return UserRecord{}, ErrUserDisabled
	}
	if err := ensureSSOMemberships(ctx, tx, user); err != nil {
		return UserRecord{}, err
	}
	audit.Action = "user.sso_login"
	audit.ResourceType = "user"
	audit.ResourceID = user.ID
	audit.ActorUserID = user.ID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return UserRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UserRecord{}, err
	}
	return user, nil
}

// federateByEmail links an existing email account to the identity or creates
// a fresh one, inside the caller's transaction.
func (store *Store) federateByEmail(ctx context.Context, tx pgx.Tx, issuer, subject, email, displayName string) (UserRecord, error) {
	if email == "" {
		return UserRecord{}, errors.New("a federated login without an existing account must carry an email claim")
	}
	var user UserRecord
	err := tx.QueryRow(ctx, `SELECT id,email,password_hash,display_name,disabled_at,created_at,external_id,sso_issuer,sso_subject FROM users WHERE lower(email)=lower($1) FOR UPDATE`, email).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.DisabledAt, &user.CreatedAt, &user.ExternalID, &user.SSOIssuer, &user.SSOSubject)
	switch {
	case err == nil:
		if user.SSOIssuer != "" && (user.SSOIssuer != issuer || user.SSOSubject != subject) {
			return UserRecord{}, ErrSSOAccountLinked
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET sso_issuer=$2,sso_subject=$3,updated_at=now() WHERE id=$1`, user.ID, issuer, subject); err != nil {
			return UserRecord{}, err
		}
		user.SSOIssuer, user.SSOSubject = issuer, subject
		return user, nil
	case IsNotFound(err):
		if displayName == "" {
			displayName = email
		}
		userID, err := model.NewID()
		if err != nil {
			return UserRecord{}, err
		}
		created := time.Now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,display_name,created_at,updated_at) VALUES($1,$2,'',$3,$4,$4)`, userID, email, displayName, created); err != nil {
			return UserRecord{}, err
		}
		return UserRecord{ID: userID, Email: email, DisplayName: displayName, SSOIssuer: issuer, SSOSubject: subject, CreatedAt: created}, nil
	default:
		return UserRecord{}, err
	}
}

// ensureSSOMemberships adds the user to every SSO-enforcing organization
// whose email domain matches, as a viewer. Existing memberships — of any
// role — are left exactly as they are. Auto-join grows membership exactly
// like an invite or a SCIM provision does, so it runs the same seat gate:
// organizations whose ceiling is reached are skipped, never over-admitted.
func ensureSSOMemberships(ctx context.Context, tx pgx.Tx, user UserRecord) error {
	domain := EmailDomain(user.Email)
	if domain == "" {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT o.id FROM organizations o
		WHERE o.sso_enforced AND lower(o.sso_email_domain)=$1
		  AND NOT EXISTS(SELECT 1 FROM organization_members m WHERE m.organization_id=o.id AND m.user_id=$2)`, domain, user.ID)
	if err != nil {
		return err
	}
	var organizationIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		organizationIDs = append(organizationIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Deterministic lock order: concurrent logins must take the seat-gate
	// locks in the same order or they can deadlock on each other.
	sort.Strings(organizationIDs)
	for _, organizationID := range organizationIDs {
		if err := lockSeatGate(ctx, tx, organizationID); err != nil {
			return err
		}
		ceiling, err := seatCeilingFor(ctx, tx, organizationID)
		if err != nil {
			return err
		}
		if ceiling > 0 {
			var members int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM organization_members m JOIN users u ON u.id = m.user_id WHERE m.organization_id=$1 AND u.disabled_at IS NULL`, organizationID).Scan(&members); err != nil {
				return err
			}
			if members+1 > ceiling {
				continue
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,created_at) VALUES($1,$2,'viewer',now())
			ON CONFLICT (organization_id,user_id) DO NOTHING`, organizationID, user.ID); err != nil {
			return err
		}
	}
	return nil
}
