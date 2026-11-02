-- Refresh-token reuse detection widens to a bounded generation window: a
-- rotated-out refresh digest is remembered for the last five generations, so
-- a stolen token replayed after its owner has rotated more than once is still
-- recognized as a replay and revokes the session chain. The window is bounded
-- (no unbounded token history), and the one-generation previous_refresh_hash
-- column remains for the newest retired digest.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS previous_refresh_hashes BYTEA[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS sessions_previous_refresh_hashes_idx ON sessions USING GIN (previous_refresh_hashes);
-- Deactivated-account sweeps look up by disabled_at; without this the
-- anonymization pass seq-scans users on every retention tick.
CREATE INDEX IF NOT EXISTS users_disabled_at_idx ON users (disabled_at) WHERE disabled_at IS NOT NULL;
