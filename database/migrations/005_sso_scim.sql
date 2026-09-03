-- Single sign-on and SCIM provisioning for organizations that enforce them.
--
-- SSO: an organization claims an email domain; while enforcement is on, that
-- domain's users authenticate through the configured OIDC issuer instead of
-- local passwords. Users federated this way carry the issuer and subject that
-- authenticated them, and an empty password hash — argon2 verification of an
-- empty string fails, so no password can ever open the account.
ALTER TABLE users ADD COLUMN IF NOT EXISTS sso_issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS sso_subject TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS external_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS users_sso_subject_idx ON users (sso_issuer, sso_subject) WHERE sso_issuer <> '';
CREATE UNIQUE INDEX IF NOT EXISTS users_external_id_idx ON users (external_id) WHERE external_id <> '';

ALTER TABLE organizations ADD COLUMN IF NOT EXISTS sso_enforced BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS sso_email_domain TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS organizations_sso_domain_idx ON organizations (lower(sso_email_domain)) WHERE sso_email_domain <> '';
