-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 005: drop SSO/SCIM identity columns and indexes.
DROP INDEX IF EXISTS organizations_sso_domain_idx;
DROP INDEX IF EXISTS users_external_id_idx;
DROP INDEX IF EXISTS users_sso_subject_idx;
ALTER TABLE organizations DROP COLUMN IF EXISTS sso_email_domain;
ALTER TABLE organizations DROP COLUMN IF EXISTS sso_enforced;
ALTER TABLE users DROP COLUMN IF EXISTS external_id;
ALTER TABLE users DROP COLUMN IF EXISTS sso_subject;
ALTER TABLE users DROP COLUMN IF EXISTS sso_issuer;
