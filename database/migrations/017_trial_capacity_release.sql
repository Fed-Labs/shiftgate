-- Trial expiry capacity repair (one-shot data fix).
--
-- StartTrial raises max_storage_bytes / max_machines to Business capacity for
-- the 14-day window, but the release path (ReleaseTrialLimits) was never
-- wired before this wave, so every expired, unconverted trial kept unlimited
-- machines and 600 GiB storage forever. ReleaseTrialLimits now runs from the
-- entitlement read path's expiry handling and reverts future expiries; this
-- migration repairs the rows that already expired before upgrade.
--
-- The predicate is ReleaseTrialLimits's own: only lapsed trials that never
-- converted (no Stripe subscription id) are reverted. A converted trial's
-- limits come from its subscription and are never touched. Data is preserved
-- either way — only the capacity caps move back to the free tier's values
-- (2 machines, 10 GiB: the entitlements table defaults).

UPDATE entitlements
SET trial_expires_at = NULL,
    max_storage_bytes = 10737418240,
    max_machines = 2,
    updated_at = now()
WHERE trial_expires_at IS NOT NULL
  AND trial_expires_at <= now()
  AND (stripe_subscription_id IS NULL OR stripe_subscription_id = '');
