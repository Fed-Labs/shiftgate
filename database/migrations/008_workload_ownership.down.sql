-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 008: drop durable workload ownership.
DROP TABLE IF EXISTS workload_ownership;
