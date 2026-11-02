-- DESTRUCTIVE rollback: drops data; restore from backup to recover it.
-- Down migration for 013: drop the purchased seat quantity. The member
-- ceiling reverts to being unenforced.
ALTER TABLE entitlements DROP COLUMN IF EXISTS stripe_item_id;
ALTER TABLE entitlements DROP COLUMN IF EXISTS seats;
