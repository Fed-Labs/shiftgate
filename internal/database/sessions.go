package database

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// sessions.go: session tokens. Only the peppered digests of access and refresh
// tokens are stored; rotation is a single serializable transaction so a
// refresh token can never be replayed.

func (store *Store) CreateSession(ctx context.Context, sessionID, userID string, accessHash, refreshHash []byte, accessExpires, refreshExpires time.Time, userAgent, remoteAddr string) error {
	_, err := store.pool.Exec(ctx, `INSERT INTO sessions(id,user_id,access_token_hash,refresh_token_hash,access_expires_at,refresh_expires_at,user_agent,remote_addr) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
		sessionID, userID, accessHash, refreshHash, accessExpires, refreshExpires, truncate(userAgent, 1024), nullableIP(remoteAddr))
	return err
}

func (store *Store) AuthenticateAccess(ctx context.Context, accessHash []byte, now time.Time) (SessionRecord, error) {
	var record SessionRecord
	err := store.pool.QueryRow(ctx, `SELECT s.id,u.id,u.email,u.display_name,s.access_expires_at,s.refresh_expires_at
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.access_token_hash=$1 AND s.revoked_at IS NULL AND s.access_expires_at>$2 AND u.disabled_at IS NULL`, accessHash, now).
		Scan(&record.ID, &record.UserID, &record.Email, &record.DisplayName, &record.AccessExpiresAt, &record.RefreshExpiresAt)
	if err == nil {
		_, _ = store.pool.Exec(ctx, `UPDATE sessions SET last_seen_at=$2 WHERE id=$1`, record.ID, now)
	}
	return record, err
}

func (store *Store) RotateSession(ctx context.Context, refreshHash, nextAccessHash, nextRefreshHash []byte, accessExpires, refreshExpires, now time.Time) (SessionRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SessionRecord{}, err
	}
	defer tx.Rollback(ctx)
	var record SessionRecord
	err = tx.QueryRow(ctx, `SELECT s.id,u.id,u.email,u.display_name,s.access_expires_at,s.refresh_expires_at
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.refresh_token_hash=$1 AND s.revoked_at IS NULL AND s.refresh_expires_at>$2 AND u.disabled_at IS NULL FOR UPDATE`, refreshHash, now).
		Scan(&record.ID, &record.UserID, &record.Email, &record.DisplayName, &record.AccessExpiresAt, &record.RefreshExpiresAt)
	if err != nil {
		return SessionRecord{}, err
	}
	if refreshExpires.After(record.RefreshExpiresAt) {
		refreshExpires = record.RefreshExpiresAt
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET access_token_hash=$2,refresh_token_hash=$3,access_expires_at=$4,refresh_expires_at=$5,last_seen_at=$6 WHERE id=$1`,
		record.ID, nextAccessHash, nextRefreshHash, accessExpires, refreshExpires, now); err != nil {
		return SessionRecord{}, err
	}
	record.AccessExpiresAt = accessExpires
	record.RefreshExpiresAt = refreshExpires
	if err := tx.Commit(ctx); err != nil {
		return SessionRecord{}, err
	}
	return record, nil
}

func (store *Store) RevokeSession(ctx context.Context, sessionID, userID string) error {
	command, err := store.pool.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, sessionID, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
