-- kpi.sql — the $0 ops team (plan 4 BR-06). Run weekly into the ops log:
--   psql "$SHIFT_DATABASE_URL" -f scripts/kpi.sql
-- Rate constants for every money query: storage $0.020/GB-mo · transfer
-- $0.020/GB · Stripe 2.9% + $0.30/charge · founder $75/hr.
-- Sources: entitlements, usage_records, migration_jobs, machines,
-- stripe_events, audit_events — no new tables.

\echo '=== KPI-1  Subscriptions and gross MRR by plan (enterprise priced out-of-band)'
SELECT plan,
       COUNT(*)                                     AS orgs,
       COALESCE(SUM(GREATEST(seats, 1)), 0)         AS billable_seats,
       CASE plan
         WHEN 'pro'      THEN COUNT(*) * 29.00
         WHEN 'business' THEN SUM(99.00 * GREATEST(seats, 1))
         ELSE 0
       END                                          AS mrr_usd
FROM entitlements
WHERE status = 'active'
GROUP BY plan
ORDER BY plan;

\echo '=== KPI-2  Net MRR after Stripe (self-serve; enterprise = contract value, manual line)'
SELECT ROUND(SUM(mrr_usd), 2)                            AS gross_mrr,
       ROUND(SUM(mrr_usd) * 0.029 + 0.30 * COUNT(*), 2)  AS stripe_fees,
       ROUND(SUM(mrr_usd) * 0.971 - 0.30 * COUNT(*), 2)  AS net_mrr
FROM (
  SELECT organization_id,
         CASE plan WHEN 'pro' THEN 29.00
                   WHEN 'business' THEN 99.00 * GREATEST(seats, 1) END AS mrr_usd
  FROM entitlements
  WHERE status = 'active' AND plan IN ('pro', 'business')
) t;

\echo '=== KPI-3  Hosted-storage COGS (bucket-truth gauge in entitlements.used_storage_bytes)'
SELECT ROUND(SUM(used_storage_bytes) / 1073741824.0, 2)          AS gb_hosted,
       ROUND(SUM(used_storage_bytes) / 1073741824.0 * 0.020, 2)  AS usd_per_month
FROM entitlements;

\echo '=== KPI-4  Transfer volume (30d) by channel — the fair-use watch'
SELECT COALESCE(metadata->>'channel', '?')              AS channel,
       ROUND(SUM(quantity) / 1073741824.0, 2)           AS gb_30d,
       ROUND(SUM(quantity) / 1073741824.0 * 0.020, 2)   AS usd_30d
FROM usage_records
WHERE kind = 'transfer_bytes'
  AND period_start >= (now()::date - 30)
GROUP BY 1
ORDER BY 2 DESC;

\echo '=== KPI-5  Fair-use breaches (30d transfer > 2x included storage) — throttle candidates'
SELECT u.organization_id,
       ROUND(SUM(u.quantity) / 1073741824.0, 2)     AS gb_30d,
       ROUND(e.max_storage_bytes / 1073741824.0, 2) AS quota_gb
FROM usage_records u
JOIN entitlements e USING (organization_id)
WHERE u.kind = 'transfer_bytes'
  AND u.period_start >= (now()::date - 30)
GROUP BY u.organization_id, e.max_storage_bytes
HAVING SUM(u.quantity) > 2 * e.max_storage_bytes
ORDER BY gb_30d DESC;

\echo '=== KPI-6  Migration health (7d) — reliability drives conversion and support load'
SELECT mode, status, COUNT(*) AS jobs,
       ROUND(100.0 * COUNT(*) / SUM(COUNT(*)) OVER (PARTITION BY mode), 1) AS pct
FROM migration_jobs
WHERE created_at >= now() - interval '7 days'
GROUP BY mode, status
ORDER BY mode, status;

\echo '=== KPI-7  Fleet activity'
SELECT COUNT(*) FILTER (WHERE status = 'online')                         AS online_now,
       COUNT(*) FILTER (WHERE last_seen_at >= now() - interval '7 days') AS seen_7d,
       COUNT(*)                                                          AS registered
FROM machines;

\echo '=== KPI-8  Funnel (30d): signup -> org -> machine -> migration'
SELECT (SELECT COUNT(*) FROM users          WHERE created_at >= now() - interval '30 days') AS new_users,
       (SELECT COUNT(*) FROM organizations  WHERE created_at >= now() - interval '30 days') AS new_orgs,
       (SELECT COUNT(*) FROM machines       WHERE created_at >= now() - interval '30 days') AS new_machines,
       (SELECT COUNT(*) FROM migration_jobs WHERE created_at >= now() - interval '30 days') AS migrations_30d;

\echo '=== KPI-9  Conversion and churn (Stripe event truth)'
SELECT date_trunc('month', received_at) AS month, event_type, COUNT(*)
FROM stripe_events
WHERE event_type IN ('checkout.session.completed',
                     'customer.subscription.deleted',
                     'invoice.payment_failed')
GROUP BY 1, 2
ORDER BY 1 DESC, 2;

\echo '=== KPI-10 Entitlement drift (ARR leakage both ways) — must return 0 rows daily'
SELECT organization_id, plan, status, stripe_subscription_id
FROM entitlements
WHERE stripe_subscription_id <> ''
  AND (status <> 'active' OR plan = 'free');

\echo '=== KPI-11 Top-10 org COGS (support + abuse watchlist)'
SELECT e.organization_id, e.plan,
       ROUND(e.used_storage_bytes / 1073741824.0, 2)                     AS gb,
       ROUND(COALESCE(t.bytes30, 0) / 1073741824.0, 2)                   AS transfer_gb_30d,
       ROUND(e.used_storage_bytes / 1073741824.0 * 0.020
           + COALESCE(t.bytes30, 0) / 1073741824.0 * 0.020, 2)           AS cogs_usd
FROM entitlements e
LEFT JOIN (
  SELECT organization_id, SUM(quantity) AS bytes30
  FROM usage_records
  WHERE kind = 'transfer_bytes' AND period_start >= (now()::date - 30)
  GROUP BY 1
) t USING (organization_id)
ORDER BY cogs_usd DESC
LIMIT 10;

\echo '=== KPI-12 Webhook backlog (entitlement-sync health) — alert if > 0 older than 24h'
SELECT COUNT(*) FROM stripe_events
WHERE processed_at IS NULL AND received_at < now() - interval '24 hours';

\echo '=== KPI-13 Storage growth (daily gauge rows written by the reconciler)'
SELECT period_start, ROUND(SUM(quantity) / 1073741824.0, 2) AS gb
FROM usage_records
WHERE kind = 'checkpoint_storage'
GROUP BY 1
ORDER BY 1 DESC
LIMIT 30;

\echo '=== KPI-14 Ops-effort proxy (90d): audit volume by action family'
SELECT date_trunc('week', created_at) AS week, action, COUNT(*)
FROM audit_events
WHERE created_at >= now() - interval '90 days'
GROUP BY 1, 2
ORDER BY 1 DESC, 3 DESC;
