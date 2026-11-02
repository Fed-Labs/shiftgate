package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"shift.dev/shift/internal/model"

	"github.com/jackc/pgx/v5"
)

// Regression cover for the GDPR erasure gap (ship finding 03 blocker 3):
// erasing a user must revoke every API key that authenticates as them and
// destroy the federated identifiers (SSO subject, external id) that point at
// the natural person — an erased account must never keep working access.
// PostgreSQL-gated.
func TestEraseUserRevokesAPIKeysAndFederatedIdentifiers(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	userID, organizationID := erasureUser(t, store, "erasure-keys")

	if _, err := store.pool.Exec(ctx,
		`UPDATE users SET sso_subject=$2, external_id=$3, sso_issuer=$4 WHERE id=$1`,
		userID, "subject-123", "ext-123", "https://issuer.example.test"); err != nil {
		t.Fatal(err)
	}

	keyID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	secretHash := []byte("erasure-regression-secret-hash")
	if _, err := store.CreateAPIKey(ctx, APIKeyRecord{
		ID: keyID, OrganizationID: organizationID, UserID: userID,
		Name: "erasure key", Prefix: "sk_test", Scopes: []byte(`["read"]`),
	}, secretHash, testAudit(organizationID, "api_key.create", keyID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AuthenticateAPIKey(ctx, secretHash, time.Now().UTC()); err != nil {
		t.Fatalf("api key does not authenticate before erasure: %v", err)
	}

	if _, err := store.EraseUser(ctx, userID, AuditInput{ID: "audit-erasure-keys-" + userID, OrganizationID: organizationID, ActorUserID: userID, Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}

	// The key must be gone — not merely unauthenticated because the user
	// row was disabled: erasure is irreversible access revocation.
	var remaining int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE user_id=$1`, userID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d api key(s) survived account erasure", remaining)
	}
	if _, err := store.AuthenticateAPIKey(ctx, secretHash, time.Now().UTC()); err == nil {
		t.Fatal("an erased user's api key still authenticates")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("authenticate after erasure error = %v, want no-match refusal", err)
	}

	// Federated identifiers name the natural person and must not survive.
	var ssoIssuer, ssoSubject, externalID string
	var disabledAt *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT sso_issuer,sso_subject,external_id,disabled_at FROM users WHERE id=$1`, userID).
		Scan(&ssoIssuer, &ssoSubject, &externalID, &disabledAt); err != nil {
		t.Fatal(err)
	}
	if ssoIssuer != "" || ssoSubject != "" || externalID != "" {
		t.Fatalf("federated identifiers survived erasure: issuer=%q subject=%q external=%q", ssoIssuer, ssoSubject, externalID)
	}
	if disabledAt == nil {
		t.Fatal("erased account is not disabled; SSO login could re-activate it")
	}
}
