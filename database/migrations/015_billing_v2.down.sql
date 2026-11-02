-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Rollback for 015_billing_v2.sql. Columns and tables are additive; dropping
-- them is safe because pre-v2 code never reads them. Order: indexes drop with
-- their tables.

ALTER TABLE organizations DROP COLUMN IF EXISTS signup_source;
ALTER TABLE organizations DROP COLUMN IF EXISTS referred_by;

DROP TABLE IF EXISTS referral_events;
DROP TABLE IF EXISTS referral_codes;
DROP TABLE IF EXISTS pricing_experiments;
DROP TABLE IF EXISTS funnel_events;
DROP TABLE IF EXISTS billing_events;

ALTER TABLE entitlements DROP COLUMN IF EXISTS grandfathered;
ALTER TABLE entitlements DROP COLUMN IF EXISTS failover;
ALTER TABLE entitlements DROP COLUMN IF EXISTS sourceguard;
ALTER TABLE entitlements DROP COLUMN IF EXISTS seats_included;
ALTER TABLE entitlements DROP COLUMN IF EXISTS billing_interval;
ALTER TABLE entitlements DROP COLUMN IF EXISTS overage_cap_cents;
ALTER TABLE entitlements DROP COLUMN IF EXISTS trial_expires_at;
