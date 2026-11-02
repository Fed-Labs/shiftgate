package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// sessions.go: session tokens. Only the peppered digests of access and refresh
// tokens are stored; rotation is a single serializable transaction so a
// refresh token can never be replayed. Rotated-out refresh digests are
// remembered for a bounded window — the newest in previous_refresh_hash, the
// last five generations in previous_refresh_hashes — and presenting any of
// them later is refresh reuse: a stolen token racing its owner, which revokes
// the whole session.

// ErrRefreshTokenReuse reports that a refresh token was presented after it had
// already been rotated away. The session it belonged to is revoked before this
// error returns, so the racing party and the legitimate owner are both logged
// out and must authenticate again.
var ErrRefreshTokenReuse = errors.New("refresh token was already rotated")

// refreshClassification is the outcome of a rotation lookup. It is a pure
// decision table so the reuse rules are testable without a database.
type refreshClassification int

const (
	// refreshRotate: the digest matches a live session's current refresh hash.
	refreshRotate refreshClassification = iota
	// refreshInvalid: unknown, expired, or revoked — ordinary rejection.
	refreshInvalid
	// refreshReuse: the digest matches a session's previous (already rotated)
	// refresh hash — a replay that must revoke the session.
	refreshReuse
)

// classifyRefreshLookup decides a rotation attempt's fate from what the two
// lookups found: the live-hash lookup first, the stale-hash lookup second.
func classifyRefreshLookup(currentFound, reuseFound bool) refreshClassification {
	switch {
	case currentFound:
		return refreshRotate
	case reuseFound:
		return refreshReuse
	default:
		return refreshInvalid
	}
}

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

// RotateSession swaps a live refresh digest for a fresh token pair in one
// serializable transaction. When the presented digest matches nothing live but
// does match a session's retired digests, the presentation is a replay: that
// session is revoked and ErrRefreshTokenReuse returns. Retired digests are
// remembered for the last five generations — wide enough that a stolen token
// is still recognized after its owner rotates a few times, bounded so token
// history cannot grow without limit.
func (store *Store) RotateSession(ctx context.Context, refreshHash, nextAccessHash, nextRefreshHash []byte, accessExpires, refreshExpires, now time.Time) (SessionRecord, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SessionRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var record SessionRecord
	currentErr := tx.QueryRow(ctx, `SELECT s.id,u.id,u.email,u.display_name,s.access_expires_at,s.refresh_expires_at
		FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.refresh_token_hash=$1 AND s.revoked_at IS NULL AND s.refresh_expires_at>$2 AND u.disabled_at IS NULL FOR UPDATE`, refreshHash, now).
		Scan(&record.ID, &record.UserID, &record.Email, &record.DisplayName, &record.AccessExpiresAt, &record.RefreshExpiresAt)
	currentFound := currentErr == nil
	if currentErr != nil && !errors.Is(currentErr, pgx.ErrNoRows) {
		return SessionRecord{}, currentErr
	}
	var reuseSessionID string
	reuseErr := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE previous_refresh_hash=$1 OR $1 = ANY(previous_refresh_hashes) LIMIT 1`, refreshHash).Scan(&reuseSessionID)
	reuseFound := reuseErr == nil
	if reuseErr != nil && !errors.Is(reuseErr, pgx.ErrNoRows) {
		return SessionRecord{}, reuseErr
	}
	switch classifyRefreshLookup(currentFound, reuseFound) {
	case refreshReuse:
		// Replay of a rotated-out token: kill the session both parties share,
		// so neither keeps an authenticated chain alive on stolen state.
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at=$2 WHERE id=$1 AND revoked_at IS NULL`, reuseSessionID, now); err != nil {
			return SessionRecord{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return SessionRecord{}, err
		}
		return SessionRecord{}, ErrRefreshTokenReuse
	case refreshInvalid:
		return SessionRecord{}, pgx.ErrNoRows
	}
	if refreshExpires.After(record.RefreshExpiresAt) {
		refreshExpires = record.RefreshExpiresAt
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET previous_refresh_hash=refresh_token_hash,
			previous_refresh_hashes=(ARRAY[refresh_token_hash] || previous_refresh_hashes)[1:5],
			access_token_hash=$2,refresh_token_hash=$3,access_expires_at=$4,refresh_expires_at=$5,last_seen_at=$6 WHERE id=$1`,
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

// RevokeSessionByRefreshToken revokes the session whose current refresh
// digest matches. It is the cookie-transport logout path: the httpOnly
// refresh cookie names the session directly, without a bearer access token.
func (store *Store) RevokeSessionByRefreshToken(ctx context.Context, refreshHash []byte) error {
	command, err := store.pool.Exec(ctx, `UPDATE sessions SET revoked_at=now() WHERE refresh_token_hash=$1 AND revoked_at IS NULL`, refreshHash)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
