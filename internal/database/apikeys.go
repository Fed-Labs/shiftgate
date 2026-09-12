package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// apikeys.go: organization API keys. Only the peppered digest of the secret is
// stored; authentication is a digest lookup that also stamps last_used_at.

func (store *Store) CreateAPIKey(ctx context.Context, record APIKeyRecord, secretHash []byte, audit AuditInput) (APIKeyRecord, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return APIKeyRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx, `INSERT INTO api_keys(id,organization_id,user_id,name,prefix,secret_hash,scopes,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`, record.ID, record.OrganizationID, record.UserID, record.Name, record.Prefix, secretHash, record.Scopes, record.ExpiresAt).Scan(&record.CreatedAt)
	if err != nil {
		return APIKeyRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return APIKeyRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return APIKeyRecord{}, err
	}
	return record, nil
}

func (store *Store) AuthenticateAPIKey(ctx context.Context, secretHash []byte, now time.Time) (APIKeyRecord, error) {
	var record APIKeyRecord
	err := store.pool.QueryRow(ctx, `SELECT k.id,k.organization_id,k.user_id,k.name,k.prefix,k.scopes,k.expires_at,k.last_used_at,k.revoked_at,k.created_at
		FROM api_keys k JOIN users u ON u.id=k.user_id
		WHERE k.secret_hash=$1 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>$2) AND u.disabled_at IS NULL`, secretHash, now).
		Scan(&record.ID, &record.OrganizationID, &record.UserID, &record.Name, &record.Prefix, &record.Scopes, &record.ExpiresAt, &record.LastUsedAt, &record.RevokedAt, &record.CreatedAt)
	if err == nil {
		_, _ = store.pool.Exec(ctx, `UPDATE api_keys SET last_used_at=$2 WHERE id=$1`, record.ID, now)
	}
	return record, err
}

func (store *Store) RevokeAPIKey(ctx context.Context, organizationID, keyID string, audit AuditInput) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1 AND organization_id=$2 AND revoked_at IS NULL`, keyID, organizationID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *Store) APIKeys(ctx context.Context, organizationID string) ([]APIKeyRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT id,organization_id,user_id,name,prefix,scopes,expires_at,last_used_at,revoked_at,created_at FROM api_keys WHERE organization_id=$1 ORDER BY created_at DESC`, organizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []APIKeyRecord
	for rows.Next() {
		var record APIKeyRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.UserID, &record.Name, &record.Prefix, &record.Scopes, &record.ExpiresAt, &record.LastUsedAt, &record.RevokedAt, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}
