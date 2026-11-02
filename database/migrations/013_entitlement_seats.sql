-- Purchased seat quantity for per-seat plans, kept alongside the Stripe
-- subscription so the member ceiling and the Stripe quantity stay in step.
-- seats = 0 means "no seat purchase recorded" and enforces no ceiling; a
-- checkout or subscription webhook records the real quantity.
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS seats INTEGER NOT NULL DEFAULT 0;
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS stripe_item_id TEXT NOT NULL DEFAULT '';
