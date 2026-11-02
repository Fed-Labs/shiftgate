-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 007: drop the control-plane dispatch URL.
ALTER TABLE machines DROP COLUMN IF EXISTS local_agent_url;
