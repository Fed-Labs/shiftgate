-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 004: drop retention policies and the capability
-- projection (the machines JSONB document remains the source of truth).
DROP TABLE IF EXISTS retention_policies;
DROP TABLE IF EXISTS machine_capabilities;
