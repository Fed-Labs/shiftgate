-- Trial engine: one 14-day no-card Business trial per organization, ever.
-- The fingerprint supports abuse forensics (org + owner email domain +
-- registered machine ids + creation week); a second start is refused at the
-- route regardless of row state.

CREATE TABLE IF NOT EXISTS organization_trials (
    organization_id TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    fingerprint TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    converted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
