-- Durable relay tickets prevent replay across relay restarts and replicas.
CREATE TABLE IF NOT EXISTS relay_tickets (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    migration_id TEXT NOT NULL REFERENCES migration_jobs(id) ON DELETE CASCADE,
    source_machine_id TEXT NOT NULL,
    destination_machine_id TEXT NOT NULL,
    nonce TEXT NOT NULL UNIQUE,
    token_hash BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL DEFAULT 'issued' CHECK (state IN ('issued','consumed','revoked','expired')),
    consumed_at TIMESTAMPTZ,
    relay_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS relay_tickets_lookup_idx ON relay_tickets(organization_id, migration_id, state, expires_at);
CREATE INDEX IF NOT EXISTS relay_tickets_expiry_idx ON relay_tickets(state, expires_at);
