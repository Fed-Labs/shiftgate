-- SHIFT Compute inventory. These tables hold real resource offers and real
-- capacity reservations. Public trading is a separate control-plane
-- configuration gate: when it is closed the same inventory still schedules
-- placements inside an organization, so nothing here is a placeholder.
CREATE TABLE IF NOT EXISTS compute_offers (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    machine_id TEXT NOT NULL,
    machine_name TEXT NOT NULL DEFAULT '',
    agent_url TEXT NOT NULL DEFAULT '',
    visibility TEXT NOT NULL DEFAULT 'organization' CHECK (visibility IN ('private', 'organization', 'public')),
    status TEXT NOT NULL DEFAULT 'offline' CHECK (status IN ('available', 'reserved', 'draining', 'offline', 'withdrawn')),
    trust TEXT NOT NULL DEFAULT 'unverified' CHECK (trust IN ('unverified', 'community', 'organization', 'verified')),
    identity_verified BOOLEAN NOT NULL DEFAULT false,
    region TEXT NOT NULL DEFAULT '',
    country TEXT NOT NULL DEFAULT '',
    exposed JSONB NOT NULL DEFAULT '{}'::jsonb,
    pricing JSONB NOT NULL DEFAULT '{}'::jsonb,
    geography JSONB NOT NULL DEFAULT '{}'::jsonb,
    policy JSONB NOT NULL DEFAULT '{}'::jsonb,
    availability JSONB NOT NULL DEFAULT '{}'::jsonb,
    capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
    latency JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_seen_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    withdrawn_at TIMESTAMPTZ,
    UNIQUE (organization_id, machine_id)
);
CREATE INDEX IF NOT EXISTS compute_offers_org_status_idx ON compute_offers(organization_id, status);
CREATE INDEX IF NOT EXISTS compute_offers_visibility_idx ON compute_offers(visibility, status) WHERE withdrawn_at IS NULL;

CREATE TABLE IF NOT EXISTS compute_reservations (
    id TEXT PRIMARY KEY,
    offer_id TEXT NOT NULL REFERENCES compute_offers(id) ON DELETE CASCADE,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    machine_id TEXT NOT NULL DEFAULT '',
    workload_id TEXT NOT NULL DEFAULT '',
    migration_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'PENDING' CHECK (state IN ('PENDING', 'ACTIVE', 'RELEASED', 'EXPIRED', 'FAILED')),
    requested JSONB NOT NULL DEFAULT '{}'::jsonb,
    hourly_micros BIGINT NOT NULL DEFAULT 0 CHECK (hourly_micros >= 0),
    currency TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS compute_reservations_offer_state_idx ON compute_reservations(offer_id, state);
CREATE INDEX IF NOT EXISTS compute_reservations_org_idx ON compute_reservations(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS compute_reservations_expiry_idx ON compute_reservations(expires_at) WHERE state IN ('PENDING', 'ACTIVE');
