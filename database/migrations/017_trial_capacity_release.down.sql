-- Irreversible data repair: the capacity this migration reverted (expired
-- trial markers and their elevated max_* values) is exactly the leaked state
-- it exists to remove, so there is nothing to restore. Re-running the up
-- migration is a no-op on already-repaired rows. No schema change to undo.
SELECT 1;
