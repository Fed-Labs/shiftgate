package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// users.go: account creation and lookup.

func (store *Store) Register(ctx context.Context, userID, email, passwordHash, displayName, organizationID, organizationName string, audit AuditInput) (UserRecord, OrganizationRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UTC()
	user := UserRecord{ID: userID, Email: email, PasswordHash: passwordHash, DisplayName: displayName, CreatedAt: now}
	if _, err := tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,display_name,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, userID, email, passwordHash, displayName, now); err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	organization := OrganizationRecord{ID: organizationID, Name: organizationName, Role: "owner", CreatedAt: now}
	if _, err := tx.Exec(ctx, `INSERT INTO organizations(id,name,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$4)`, organizationID, organizationName, userID, now); err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role,created_at) VALUES($1,$2,'owner',$3)`, organizationID, userID, now); err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO entitlements(organization_id) VALUES($1)`, organizationID); err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	audit.OrganizationID = organizationID
	audit.ActorUserID = userID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UserRecord{}, OrganizationRecord{}, err
	}
	return user, organization, nil
}

func (store *Store) UserByEmail(ctx context.Context, email string) (UserRecord, error) {
	var user UserRecord
	err := store.pool.QueryRow(ctx, `SELECT id,email,password_hash,display_name,disabled_at,created_at,external_id,sso_issuer,sso_subject FROM users WHERE lower(email)=lower($1)`, email).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.DisplayName, &user.DisabledAt, &user.CreatedAt, &user.ExternalID, &user.SSOIssuer, &user.SSOSubject)
	return user, err
}
