-- Billing v2: entitlement columns the plan catalog v2 and the metering loop
-- read. Defaults preserve today's behavior for existing rows: no trial, no
-- overage spend cap, monthly interval, no SourceGuard add-on.

ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS trial_expires_at TIMESTAMPTZ;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS overage_cap_cents INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS billing_interval TEXT NOT NULL DEFAULT 'month';
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS seats_included INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS sourceguard BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS failover BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS grandfathered BOOLEAN NOT NULL DEFAULT false;

-- Billing lifecycle events (churn, dunning, win-back) kept for the win-back
-- queue and the funnel experiments' success metrics. Append-only.
CREATE TABLE IF NOT EXISTS billing_events (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS billing_events_org_idx ON billing_events(organization_id, at);

-- Product funnel events (paywall views, upgrade clicks, trial starts,
-- refusals). Keyed by org where known; append-only; no workload contents.
CREATE TABLE IF NOT EXISTS funnel_events (
    id TEXT PRIMARY KEY,
    organization_id TEXT,
    kind TEXT NOT NULL,
    surface TEXT NOT NULL DEFAULT '',
    feature TEXT NOT NULL DEFAULT '',
    at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS funnel_events_kind_idx ON funnel_events(kind, at);

-- Pricing experiments: cohort price overrides applied at checkout only. The
-- catalog always shows list price; existing subscriptions keep their Stripe
-- price (grandfathering is Stripe truth).
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

-- Referral codes and events (conversion loop: give a month, get a month).
CREATE TABLE IF NOT EXISTS referral_codes (
    code TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS referral_codes_org_idx ON referral_codes(organization_id);

CREATE TABLE IF NOT EXISTS referral_events (
    id TEXT PRIMARY KEY,
    ref_code TEXT NOT NULL,
    referred_org_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    activated_at TIMESTAMPTZ,
    reward_granted BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS referral_events_code_idx ON referral_events(ref_code);

ALTER TABLE organizations ADD COLUMN IF NOT EXISTS referred_by TEXT NOT NULL DEFAULT '';
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS signup_source JSONB NOT NULL DEFAULT '{}'::jsonb;
