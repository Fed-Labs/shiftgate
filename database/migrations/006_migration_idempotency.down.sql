-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 006: drop the managed-migration queue columns and their
-- indexes. Existing jobs keep their history rows.
DROP INDEX IF EXISTS migration_jobs_org_idempotency_idx;
DROP INDEX IF EXISTS migration_jobs_lease_idx;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS timeout_seconds;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS next_attempt_at;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS cancel_requested;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS managed;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS agent_migration_id;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS lease_expires_at;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS lease_owner;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS attempt_count;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS idempotency_key;
