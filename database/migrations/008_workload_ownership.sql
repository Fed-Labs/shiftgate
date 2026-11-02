-- One durable ownership record per organization workload. The epoch is monotone;
-- token_hash is the only persisted form of the opaque lease token.
CREATE TABLE IF NOT EXISTS workload_ownership (
    workload_id TEXT PRIMARY KEY REFERENCES workloads(id) ON DELETE CASCADE,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    epoch BIGINT NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    owner_machine_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'unowned' CHECK (state IN ('unowned','owned','fenced','conflict')),
    token_hash BYTEA,
    lease_expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS workload_ownership_org_idx ON workload_ownership(organization_id, state);
