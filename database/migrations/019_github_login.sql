-- GitHub sign-in ("Sign in / Sign up with GitHub"): users carry an optional
-- GitHub identity alongside the existing password and SSO ones. github_id is
-- nullable — every pre-existing account predates it — and unique when present
-- (partial index), so one GitHub account can never open two local users.
-- github_login is the human-readable handle at link time; it is a display
-- convenience (and an erasure target), never a key.
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_id BIGINT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS github_login TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS users_github_id_idx ON users (github_id) WHERE github_id IS NOT NULL;

-- One-time browser handoff codes. The OAuth callback resolves the GitHub
-- identity server-side and then redirects the browser to the caller's
-- redirect_uri with a bare ?github_code= — the code names this row (only its
-- peppered digest is stored, like every other token here) and the row carries
-- the resolved identity for the redeem call. Single use (consumed_at) and
-- short-lived (expires_at) so a leaked redirect URL cannot be replayed into
-- a session, and the redirect itself never carries GitHub tokens.
CREATE TABLE IF NOT EXISTS github_auth_codes (
    digest BYTEA PRIMARY KEY,
    github_id BIGINT NOT NULL,
    github_login TEXT NOT NULL,
    email TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
