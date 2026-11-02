-- DESTRUCTIVE rollback: drops the handoff-code table and the GitHub identity
-- columns (and the account links recorded in them); restore from backup to
-- recover the links. Order mirrors the up file: derived table first, then
-- indexes and columns.
DROP TABLE IF EXISTS github_auth_codes;
DROP INDEX IF EXISTS users_github_id_idx;
ALTER TABLE users DROP COLUMN IF EXISTS github_login;
ALTER TABLE users DROP COLUMN IF EXISTS github_id;
