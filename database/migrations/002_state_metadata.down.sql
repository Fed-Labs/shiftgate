-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 002: drop API keys, checkpoints, migration events, and
-- usage records.
DROP TABLE IF EXISTS usage_records;
DROP TABLE IF EXISTS migration_events;
DROP TABLE IF EXISTS checkpoints;
DROP TABLE IF EXISTS api_keys;
