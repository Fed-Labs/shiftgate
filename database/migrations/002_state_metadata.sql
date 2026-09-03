CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    prefix TEXT NOT NULL,
    secret_hash BYTEA NOT NULL,
    scopes JSONB NOT NULL DEFAULT '[]'::jsonb,
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS api_keys_secret_hash_idx ON api_keys(secret_hash);
CREATE INDEX IF NOT EXISTS api_keys_org_idx ON api_keys(organization_id, created_at DESC);

CREATE TABLE IF NOT EXISTS checkpoints (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    workload_id TEXT NOT NULL REFERENCES workloads(id) ON DELETE CASCADE,
    machine_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('full', 'incremental')),
    parent_id TEXT,
    manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
    plain_bytes BIGINT NOT NULL DEFAULT 0 CHECK (plain_bytes >= 0),
    stored_bytes BIGINT NOT NULL DEFAULT 0 CHECK (stored_bytes >= 0),
    chunk_count INTEGER NOT NULL DEFAULT 0 CHECK (chunk_count >= 0),
    status TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('creating', 'available', 'corrupt', 'deleted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS checkpoints_workload_idx ON checkpoints(organization_id, workload_id, created_at DESC);

CREATE TABLE IF NOT EXISTS migration_events (
    id TEXT PRIMARY KEY,
    migration_id TEXT NOT NULL REFERENCES migration_jobs(id) ON DELETE CASCADE,
    sequence BIGINT NOT NULL,
    stage TEXT NOT NULL,
    message TEXT NOT NULL,
    progress DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 1),
    bytes_done BIGINT NOT NULL DEFAULT 0,
    bytes_total BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (migration_id, sequence)
);
CREATE INDEX IF NOT EXISTS migration_events_migration_idx ON migration_events(migration_id, sequence);

CREATE TABLE IF NOT EXISTS usage_records (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('checkpoint_storage', 'transfer_bytes', 'machine_hours', 'api_requests')),
    quantity BIGINT NOT NULL CHECK (quantity >= 0),
    period_start DATE NOT NULL,
    period_end DATE NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS usage_records_org_period_idx ON usage_records(organization_id, period_start, kind);
