-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 009: drop relay tickets.
DROP TABLE IF EXISTS relay_tickets;
