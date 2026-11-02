-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 012: drop the reuse-detection column. Existing sessions
-- lose replay detection but keep working.
DROP INDEX IF EXISTS sessions_previous_refresh_hash_idx;
ALTER TABLE sessions DROP COLUMN IF EXISTS previous_refresh_hash;
