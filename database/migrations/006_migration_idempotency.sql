-- Existing migration jobs are not silently redispatched on upgrade. Only new
-- coordinator-managed jobs enter the durable worker queue.
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS idempotency_key TEXT;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS lease_owner TEXT;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS agent_migration_id TEXT;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS managed BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS cancel_requested BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS timeout_seconds INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS migration_jobs_lease_idx ON migration_jobs(next_attempt_at, lease_expires_at)
    WHERE managed AND status IN ('queued','running');
CREATE UNIQUE INDEX IF NOT EXISTS migration_jobs_org_idempotency_idx
    ON migration_jobs(organization_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
