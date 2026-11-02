-- Readiness fixes: migration_jobs.result for restore-duration stats, the
-- indexes the hot queries need, referral uniqueness (one claim per referred
-- org, one code per org), billing CHECK constraints, and removal of the
-- unused pricing_experiments table. Index and constraint creation is
-- idempotent so a retried half-applied file converges.

ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS result JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS route TEXT NOT NULL DEFAULT '';
ALTER TABLE migration_jobs ADD COLUMN IF NOT EXISTS relay_endpoint TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS migration_jobs_result_duration_idx ON migration_jobs (((result->>'duration_ms')::bigint)) WHERE result ? 'duration_ms';

CREATE INDEX IF NOT EXISTS migration_jobs_workload_idx ON migration_jobs(workload_id);
CREATE INDEX IF NOT EXISTS audit_events_actor_user_idx ON audit_events(actor_user_id);
CREATE INDEX IF NOT EXISTS audit_events_resource_id_idx ON audit_events(resource_id);
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys(user_id);
CREATE INDEX IF NOT EXISTS stripe_events_backlog_idx ON stripe_events(received_at) WHERE processed_at IS NULL;
CREATE INDEX IF NOT EXISTS entitlements_trial_expiry_idx ON entitlements(trial_expires_at) WHERE trial_expires_at IS NOT NULL;

DROP INDEX IF EXISTS migration_events_migration_idx;

DROP INDEX IF EXISTS referral_codes_org_idx;
CREATE UNIQUE INDEX IF NOT EXISTS referral_codes_org_idx ON referral_codes(organization_id);
CREATE UNIQUE INDEX IF NOT EXISTS referral_events_referred_org_idx ON referral_events(referred_org_id);

DROP TABLE IF EXISTS pricing_experiments;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'entitlements_seats_check' AND conrelid = 'entitlements'::regclass) THEN
        ALTER TABLE entitlements ADD CONSTRAINT entitlements_seats_check CHECK (seats >= 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'entitlements_seats_included_check' AND conrelid = 'entitlements'::regclass) THEN
        ALTER TABLE entitlements ADD CONSTRAINT entitlements_seats_included_check CHECK (seats_included >= 0);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'entitlements_billing_interval_check' AND conrelid = 'entitlements'::regclass) THEN
        ALTER TABLE entitlements ADD CONSTRAINT entitlements_billing_interval_check CHECK (billing_interval IN ('month', 'year'));
    END IF;
END
$$;
