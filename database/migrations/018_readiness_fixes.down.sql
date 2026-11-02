-- DESTRUCTIVE rollback: drops migration_jobs.result (recorded restore
-- durations) and the unique constraints; backup restores recover the data.
-- Order mirrors the up file: constraints and derived indexes first.

ALTER TABLE entitlements DROP CONSTRAINT IF EXISTS entitlements_billing_interval_check;
ALTER TABLE entitlements DROP CONSTRAINT IF EXISTS entitlements_seats_included_check;
ALTER TABLE entitlements DROP CONSTRAINT IF EXISTS entitlements_seats_check;

CREATE TABLE IF NOT EXISTS pricing_experiments (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    plan TEXT NOT NULL,
    cohort TEXT NOT NULL DEFAULT 'signup_week_parity',
    stripe_price_id TEXT NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DROP INDEX IF EXISTS referral_events_referred_org_idx;
DROP INDEX IF EXISTS referral_codes_org_idx;
CREATE INDEX IF NOT EXISTS referral_codes_org_idx ON referral_codes(organization_id);

CREATE INDEX IF NOT EXISTS migration_events_migration_idx ON migration_events(migration_id, sequence);

DROP INDEX IF EXISTS entitlements_trial_expiry_idx;
DROP INDEX IF EXISTS stripe_events_backlog_idx;
DROP INDEX IF EXISTS api_keys_user_idx;
DROP INDEX IF EXISTS audit_events_resource_id_idx;
DROP INDEX IF EXISTS audit_events_actor_user_idx;
DROP INDEX IF EXISTS migration_jobs_workload_idx;
DROP INDEX IF EXISTS migration_jobs_result_duration_idx;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS relay_endpoint;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS route;
ALTER TABLE migration_jobs DROP COLUMN IF EXISTS result;
