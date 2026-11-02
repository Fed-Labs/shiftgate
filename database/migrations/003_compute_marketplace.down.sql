-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 003: drop the compute marketplace tables.
DROP TABLE IF EXISTS compute_reservations;
DROP TABLE IF EXISTS compute_offers;
