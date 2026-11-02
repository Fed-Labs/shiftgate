-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 010: drop the agent command queue.
DROP TABLE IF EXISTS agent_commands;
