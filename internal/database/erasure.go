package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"shift.dev/shift/internal/model"
)

// erasure.go: the GDPR erasure / anonymization path.
//
// Account erasure replaces personal data with irreversible placeholders
// instead of deleting rows: the user row survives so audit trails, foreign
// keys, and billing history keep pointing at a stable pseudonymous account,
// while everything that identifies a natural person — email, display name,
// password hash, session tokens, IP addresses, user agents — is destroyed.
// The original email is overwritten, not hashed-and-kept, so it cannot be
// correlated or reversed. This is the path that supersedes the "SCIM delete
// keeps the email forever" behavior described in docs/limitations.md.

// ErasedEmailDomain is the domain used for placeholder emails. It is an
// RFC 2606 reserved name: nothing sent to it can reach a real mailbox.
const ErasedEmailDomain = "erased.invalid"

// erasedPasswordHash is an intentionally unverifiable password hash: no
// input verifies against it (the encoded form does not parse as any scheme
// verifyPassword accepts), so an erased account cannot log in again.
const erasedPasswordHash = "!erased"

// ErrUserNotFound reports that erasure targeted an unknown user id.
var ErrUserNotFound = errors.New("user was not found")

// ErasureReport states what the anonymization destroyed. It is returned to
// the caller for logging and audit metadata, and is safe to publish in a
// subject-access response: it contains counts, not the destroyed data.
type ErasureReport struct {
	UserID           string `json:"user_id"`
	EmailAnonymized  bool   `json:"email_anonymized"`
	SessionsRemoved  int    `json:"sessions_removed"`
	APIKeysRemoved   int    `json:"api_keys_removed"`
	AuditIPsScrubbed int    `json:"audit_ips_scrubbed"`
}

// scrubMetadataKeys are the metadata fields that can name or locate the
// natural person behind an account. They are removed wherever they appear —
// at the top level or nested inside objects and arrays — from every audit row
// the erased user touched as actor or as subject.
var scrubMetadataKeys = map[string]bool{
	"email":           true,
	"remote_addr":     true,
	"user_agent":      true,
	"external_id":     true,
	"sso_subject":     true,
	"github_login":    true,
	"github_id":       true,
	"ip":              true,
	"ip_address":      true,
	"x_forwarded_for": true,
}

// scrubMetadataValue recursively deletes every scrub-listed key from decoded
// JSON containers. Objects and arrays are rebuilt in place; scalars pass
// through untouched.
func scrubMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if scrubMetadataKeys[strings.ToLower(key)] {
				delete(typed, key)
				continue
			}
			typed[key] = scrubMetadataValue(child)
		}
		return typed
	case []any:
		for index, child := range typed {
			typed[index] = scrubMetadataValue(child)
		}
		return typed
	default:
		return value
	}
}

// scrubMetadataPII rewrites one audit metadata document with every
// scrub-listed key removed at any depth. Malformed documents become empty
// objects rather than surviving verbatim.
func scrubMetadataPII(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return json.RawMessage(`{}`)
	}
	cleaned, err := json.Marshal(scrubMetadataValue(decoded))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return cleaned
}

// EraseUser irreversibly anonymizes one user's personal data and revokes
// every session and API key. The user row itself remains (pseudonymized) so
// organization membership, audit events, and Stripe history keep valid
// references. The audit argument records the erasure action itself (with the
// actor's remote address intact — the erasure request is a security event,
// not erased data).
//
// Everything that identifies the natural person is destroyed: the email,
// display name, and password hash; the IdP subject, issuer, and external id
// (which would otherwise let an SSO login re-activate the account); the
// linked GitHub id and login (which would otherwise let "Sign in with
// GitHub" re-activate it); every session; every API key (which would
// otherwise keep authenticating); and every audit row the user touched as
// actor or as subject — remote addresses and scrub-listed metadata keys at
// any nesting depth included.
//
// Idempotent: erasing an already-erased user anonymizes the placeholder again
// and reports EmailAnonymized=false.
func (store *Store) EraseUser(ctx context.Context, userID string, audit AuditInput) (ErasureReport, error) {
	report := ErasureReport{UserID: userID}
	if strings.TrimSpace(userID) == "" {
		return report, errors.New("erasure requires a user id")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return report, fmt.Errorf("user %s: %w", userID, ErrUserNotFound)
		}
		return report, err
	}
	placeholder := "erased-" + userID + "@" + ErasedEmailDomain
	alreadyErased := strings.HasSuffix(strings.ToLower(email), "@"+ErasedEmailDomain)
	command, err := tx.Exec(ctx, `UPDATE users SET email=$2,display_name=$3,password_hash=$4,
		sso_issuer='',sso_subject='',external_id='',github_id=NULL,github_login='',
		disabled_at=COALESCE(disabled_at,now()),updated_at=now() WHERE id=$1`,
		userID, placeholder, "Erased user", erasedPasswordHash)
	if err != nil {
		return report, err
	}
	report.EmailAnonymized = command.RowsAffected() > 0 && !alreadyErased
	sessions, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID)
	if err != nil {
		return report, err
	}
	report.SessionsRemoved = int(sessions.RowsAffected())
	keys, err := tx.Exec(ctx, `DELETE FROM api_keys WHERE user_id=$1`, userID)
	if err != nil {
		return report, err
	}
	report.APIKeysRemoved = int(keys.RowsAffected())
	// Audit scrub covers both sides of every event the user touched: rows
	// where they acted and rows where they were the subject carry PII in the
	// remote address and in metadata (invites and SCIM rows name the email).
	// Metadata is scrubbed unconditionally and recursively, so nested keys
	// cannot smuggle an identifier past the erasure.
	rows, err := tx.Query(ctx, `SELECT id,metadata FROM audit_events WHERE actor_user_id=$1 OR resource_id=$1`, userID)
	if err != nil {
		return report, err
	}
	type scrubTarget struct {
		id       string
		metadata json.RawMessage
	}
	var targets []scrubTarget
	for rows.Next() {
		var target scrubTarget
		if err := rows.Scan(&target.id, &target.metadata); err != nil {
			rows.Close()
			return report, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	for _, target := range targets {
		if _, err := tx.Exec(ctx, `UPDATE audit_events SET remote_addr=NULL, metadata=$2 WHERE id=$1`, target.id, scrubMetadataPII(target.metadata)); err != nil {
			return report, err
		}
	}
	report.AuditIPsScrubbed = len(targets)
	if audit.Action == "" {
		audit.Action = "user.erased"
	}
	if audit.ResourceType == "" {
		audit.ResourceType = "user"
	}
	audit.ResourceID = userID
	if audit.ID == "" {
		auditID, err := model.NewID()
		if err != nil {
			return report, err
		}
		audit.ID = auditID
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	return report, nil
}

// AnonymizeDeactivatedUsers sweeps deactivated accounts whose grace period
// has expired and anonymizes each one. This is the retention hook that turns
// SCIM deactivation ("the row survives for the audit trail") into the
// GDPR-minimized end state: the email and connection metadata of a deleted
// user live only for the grace window, then are destroyed while the
// pseudonymous row stays for audit integrity.
//
// It returns how many users were anonymized in this pass. The retention
// sweeper calls it (see the cross-agent hook in the fix log); it is safe to
// run concurrently and cheap to run often.
func (store *Store) AnonymizeDeactivatedUsers(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan < 0 {
		return 0, errors.New("grace period cannot be negative")
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	rows, err := store.pool.Query(ctx, `SELECT id FROM users WHERE disabled_at IS NOT NULL AND disabled_at < $1 AND email NOT LIKE '%@`+ErasedEmailDomain+`'`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	erased := 0
	for _, id := range ids {
		if _, err := store.EraseUser(ctx, id, AuditInput{Action: "user.anonymized", ResourceType: "user", ResourceID: id}); err != nil {
			return erased, err
		}
		erased++
	}
	return erased, nil
}
