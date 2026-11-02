-- The public agent_url remains the peer endpoint. local_agent_url is an
-- explicitly configured control-plane dispatch path; it is never inferred from
-- a public peer URL, so the control plane cannot weaken mTLS boundaries.
ALTER TABLE machines ADD COLUMN IF NOT EXISTS local_agent_url TEXT NOT NULL DEFAULT '';
