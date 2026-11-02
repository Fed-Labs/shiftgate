-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 014: reuse detection falls back to the single
-- previous_refresh_hash generation (migration 012).
DROP INDEX IF EXISTS users_disabled_at_idx;
DROP INDEX IF EXISTS sessions_previous_refresh_hashes_idx;
ALTER TABLE sessions DROP COLUMN IF EXISTS previous_refresh_hashes;
