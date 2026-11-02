package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// erasureUser registers a user plus one session row carrying connection
// metadata, so EraseUser has real PII to destroy.
func erasureUser(t *testing.T, store *Store, label string) (string, string) {
	t.Helper()
	ctx := context.Background()
	userID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	organizationID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	email := "pii-" + label + "-" + userID + "@example.test"
	audit := AuditInput{ID: "audit-" + userID, OrganizationID: organizationID, ActorUserID: userID, Action: "user.register", ResourceType: "user", ResourceID: userID, Metadata: map[string]any{}, RemoteAddr: "192.0.2.10"}
	if _, _, err := store.Register(ctx, userID, email, "hash-not-checked-by-erasure", "PII "+label, organizationID, "Erasure Org "+label, audit); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO sessions(id,user_id,access_token_hash,refresh_token_hash,access_expires_at,refresh_expires_at,user_agent,remote_addr) VALUES($1,$1,$2,$2,now()+'1 hour',now()+'24 hours','erasure-test/1.0','192.0.2.10')`, userID, []byte("token")); err != nil {
		t.Fatal(err)
	}
	return userID, organizationID
}

// TestEraseUserAnonymizesPII is the GDPR erasure contract: email and display
// name are overwritten with irreversible placeholders, sessions (and their
// IP/user-agent metadata) are destroyed, audit IP addresses are scrubbed, and
// the user row survives as a pseudonymous identity for the audit trail.
// PostgreSQL-gated.
func TestEraseUserAnonymizesPII(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	userID, organizationID := erasureUser(t, store, "erase")

	report, err := store.EraseUser(ctx, userID, AuditInput{ID: "audit-erasure-" + userID, OrganizationID: organizationID, ActorUserID: userID, Metadata: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !report.EmailAnonymized || report.SessionsRemoved != 1 || report.AuditIPsScrubbed < 1 {
		t.Fatalf("erasure report = %+v", report)
	}
	var email, displayName, passwordHash string
	var disabledAt *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT email,display_name,password_hash,disabled_at FROM users WHERE id=$1`, userID).Scan(&email, &displayName, &passwordHash, &disabledAt); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(email, "@"+ErasedEmailDomain) {
		t.Fatalf("email not anonymized: %q", email)
	}
	if strings.Contains(email, "example.test") || displayName == "PII erase" {
		t.Fatalf("original PII still present: %q %q", email, displayName)
	}
	if passwordHash == "hash-not-checked-by-erasure" {
		t.Fatal("password hash was not destroyed")
	}
	var sessions int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id=$1`, userID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("%d sessions survived erasure", sessions)
	}
	var auditIPs int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND remote_addr IS NOT NULL`, userID).Scan(&auditIPs); err != nil {
		t.Fatal(err)
	}
	if auditIPs != 0 {
		t.Fatalf("%d audit rows kept a remote address", auditIPs)
	}

	// Idempotent: a second erasure is a no-op report.
	again, err := store.EraseUser(ctx, userID, AuditInput{ID: "audit-erasure-2-" + userID, OrganizationID: organizationID, Metadata: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if again.EmailAnonymized {
		t.Fatal("second erasure claimed to anonymize an already-erased email")
	}

	if _, err := store.EraseUser(ctx, "no-such-user", AuditInput{ID: "audit-erasure-miss"}); err == nil {
		t.Fatal("erasing an unknown user succeeded")
	}
}

// TestAnonymizeDeactivatedUsersGraceSweep is the SCIM-deletion remediation:
// a deactivated account keeps nothing personal past the grace window.
// PostgreSQL-gated.
func TestAnonymizeDeactivatedUsersGraceSweep(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	userID, _ := erasureUser(t, store, "scim")
	otherID, _ := erasureUser(t, store, "fresh")

	if _, err := store.pool.Exec(ctx, `UPDATE users SET disabled_at=now()-interval '3 days' WHERE id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1`, otherID); err != nil {
		t.Fatal(err)
	}

	// The sweep is global (retention is global); earlier runs against the
	// shared test database may have left their own deactivated users, so the
	// assertions are about this run's two users, not exact global counts.
	erased, err := store.AnonymizeDeactivatedUsers(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if erased < 1 {
		t.Fatalf("sweep anonymized %d users, want at least the one past its grace period", erased)
	}
	var email string
	if err := store.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(email, "@"+ErasedEmailDomain) {
		t.Fatalf("deactivated user kept its email: %q", email)
	}
	if err := store.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, otherID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(email, "@"+ErasedEmailDomain) {
		t.Fatalf("user inside the grace window was anonymized: %q", email)
	}

	// A second sweep changes nothing for this run's users: the anonymized
	// one no longer matches, the fresh one is still in grace.
	_, err = store.AnonymizeDeactivatedUsers(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, userID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(email, "@"+ErasedEmailDomain) {
		t.Fatalf("second sweep disturbed an anonymized user: %q", email)
	}
	if err := store.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, otherID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(email, "@"+ErasedEmailDomain) {
		t.Fatalf("second sweep anonymized a user inside the grace window: %q", email)
	}
}
