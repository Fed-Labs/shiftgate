-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 011: drop replay-safe relay delivery state.
DROP TABLE IF EXISTS relay_request_nonces;
ALTER TABLE agent_commands DROP COLUMN IF EXISTS claim_token;
ALTER TABLE agent_commands DROP COLUMN IF EXISTS expires_at;
ALTER TABLE relay_tickets DROP COLUMN IF EXISTS destination_claimed_at;
ALTER TABLE relay_tickets DROP COLUMN IF EXISTS source_claimed_at;
ALTER TABLE relay_tickets DROP COLUMN IF EXISTS audience;
ALTER TABLE relay_tickets DROP COLUMN IF EXISTS workload_id;
