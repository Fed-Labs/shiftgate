-- Machine capabilities as a relational projection, and per-organization
-- retention policies. The JSONB column on machines remains the source of
-- truth for the full capability document an agent reports; machine_capabilities
-- mirrors the scalar leaves into rows so fleet queries ("which machines have
-- cuda >= 9.0") filter and sort through indexes instead of a JSONB scan.
CREATE TABLE IF NOT EXISTS machine_capabilities (
    machine_record_id TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    capability TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('number', 'text', 'boolean')),
    number_value DOUBLE PRECISION,
    text_value TEXT,
    boolean_value BOOLEAN,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (machine_record_id, capability)
);
CREATE INDEX IF NOT EXISTS machine_capabilities_lookup_idx ON machine_capabilities(capability, kind);
CREATE INDEX IF NOT EXISTS machine_capabilities_number_idx ON machine_capabilities(capability) WHERE kind = 'number';

-- Retention windows. Days, not timestamps, so a policy change re-dates every
-- pending deletion on the next enforcement pass. Zero keeps records forever.
CREATE TABLE IF NOT EXISTS retention_policies (
    organization_id TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    audit_retention_days INTEGER NOT NULL DEFAULT 365 CHECK (audit_retention_days >= 0),
    checkpoint_retention_days INTEGER NOT NULL DEFAULT 90 CHECK (checkpoint_retention_days >= 0),
    deleted_storage_retention_days INTEGER NOT NULL DEFAULT 30 CHECK (deleted_storage_retention_days >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backfill the projection for machines registered before this migration, so
-- capability queries work fleet-wide from the first minute, not gradually as
-- machines next report. Objects and arrays stay in the JSONB document only.
INSERT INTO machine_capabilities(machine_record_id,capability,kind,number_value,text_value,boolean_value,updated_at)
SELECT m.id,
       kv.key,
       CASE jsonb_typeof(kv.value) WHEN 'number' THEN 'number' WHEN 'string' THEN 'text' WHEN 'boolean' THEN 'boolean' END,
       CASE WHEN jsonb_typeof(kv.value) = 'number' THEN (kv.value #>> '{}')::double precision END,
       CASE WHEN jsonb_typeof(kv.value) = 'string' THEN kv.value #>> '{}' END,
       CASE WHEN jsonb_typeof(kv.value) = 'boolean' THEN (kv.value #>> '{}')::boolean END,
       now()
FROM machines m
CROSS JOIN jsonb_each(m.capabilities) kv
WHERE jsonb_typeof(kv.value) IN ('number', 'string', 'boolean')
ON CONFLICT (machine_record_id, capability) DO NOTHING;
