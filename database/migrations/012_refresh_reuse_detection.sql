-- Refresh-token reuse detection: remember the previous refresh digest for one
-- generation so a rotated-out token can be recognized as a replay and revoke
-- the session chain instead of merely failing the lookup.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS previous_refresh_hash BYTEA;
CREATE INDEX IF NOT EXISTS sessions_previous_refresh_hash_idx ON sessions(previous_refresh_hash)
    WHERE previous_refresh_hash IS NOT NULL;
