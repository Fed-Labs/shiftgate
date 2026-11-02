-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 001: drop the initial schema. Children first so foreign
-- keys never block a revert.
DROP TABLE IF EXISTS stripe_events;
DROP TABLE IF EXISTS storage_objects;
DROP TABLE IF EXISTS entitlements;
DROP TABLE IF EXISTS migration_jobs;
DROP TABLE IF EXISTS workloads;
DROP TABLE IF EXISTS machines;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS organization_members;
DROP TABLE IF EXISTS organizations;
DROP TABLE IF EXISTS users;
