-- Replay-safe per-leg claims and machine-bound outbound command delivery.
ALTER TABLE relay_tickets ADD COLUMN IF NOT EXISTS workload_id TEXT NOT NULL DEFAULT '';
ALTER TABLE relay_tickets ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT '';
ALTER TABLE relay_tickets ADD COLUMN IF NOT EXISTS source_claimed_at TIMESTAMPTZ;
ALTER TABLE relay_tickets ADD COLUMN IF NOT EXISTS destination_claimed_at TIMESTAMPTZ;
ALTER TABLE agent_commands ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE agent_commands ADD COLUMN IF NOT EXISTS claim_token TEXT;
CREATE TABLE IF NOT EXISTS relay_request_nonces (
 machine_id TEXT NOT NULL,
 nonce TEXT NOT NULL,
 expires_at TIMESTAMPTZ NOT NULL,
 PRIMARY KEY(machine_id, nonce)
);
CREATE INDEX IF NOT EXISTS relay_request_nonces_expiry ON relay_request_nonces(expires_at);
