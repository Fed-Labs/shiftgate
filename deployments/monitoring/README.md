# SHIFT monitoring & operations guidance

This directory ships the alert rules (`alerts.yaml`) and the operator guidance
that pairs with them. It lives under `deployments/` on purpose: the long-form
docs in `docs/` are maintained separately, and this file is the ops companion
to the Helm chart and the metrics the control plane exposes.

## Service level objectives

| SLO | Target | Signal |
|---|---|---|
| Control-plane availability | 99.9% of API requests return non-5xx over 30 days | `shift_http_responses_total{status=~"5.."}` vs `shift_http_requests_total` |
| Control-plane latency | p99 < 2s over 30 days | `shift_http_request_duration_seconds_bucket` |
| Probe correctness | `/health` and `/ready` never rate limited (hard invariant) | probes bypass the limiter by design; alert on `up` instead |
| Migration queue freshness | no managed job queued > 15 min during business hours | `shift_migration_oldest_queued_seconds` |
| Schema convergence | all replicas report `migrations: applied` within 10 min of a release | `/ready` body + `shift_schema_migration_failed` |

Burn-rate alerting: pair the availability SLO with a fast burn (2% over 1h →
page) and a slow burn (5% over 6h → ticket); `alerts.yaml` encodes the simple
threshold forms of these.

## Required exporters

The bundle assumes three scrape sources; an alert whose source is missing is
silently inactive, so wire all three (see `prometheus-scrape.yml` for the
scrape jobs and `blackbox.yml` for the probe module):

| Source | Produces | Alerts that go dead without it |
|---|---|---|
| control plane `/metrics` | `shift_*` series | everything in `shift-control-plane` |
| node_exporter | `node_filesystem_*` | `ShiftChunkStoreDiskLow` |
| node_exporter textfile collector (`SHIFT_BACKUP_TEXTFILE_DIR`) | `shift_backup_*` | `ShiftBackupFailed`, `ShiftBackupStale` |
| blackbox-exporter (`blackbox.yml`) | `probe_success{job="shift-control",healthcheck="ready"}` | `ShiftControlPlaneNotReady` |

The blackbox probe passes a deploy-time `pending` migration and fails on
`SCHEMA_MIGRATION_FAILED` / `DATABASE_UNAVAILABLE`, so the readiness page
means an actual failure, not a rolling deploy.

## Metrics reference (control plane)

- `shift_http_request_duration_seconds` — histogram (`_bucket`/`_sum`/`_count`)
- `shift_http_requests_total`, `shift_http_errors_total`, `shift_http_responses_total{status}`
- `shift_db_pool_max_conns`, `shift_db_pool_total_conns`, `shift_db_pool_idle_conns`,
  `shift_db_pool_acquire_count`, `shift_db_pool_acquire_seconds_total`
- `shift_migration_queue_depth`, `shift_migration_oldest_queued_seconds`
- `shift_schema_migration_failed`, `shift_machine_presence_lag_seconds`
- agent side: `shift_transfer_throughput_bytes_per_second` histogram, migration
  outcome/duration/downtime series from `shift_diagnostics`
- backup/DR drill (textfile collector, see below):
  `shift_backup_last_run_timestamp_seconds`, `shift_backup_last_run_success`,
  `shift_backup_last_success_timestamp_seconds`

Scrape authentication: set `control.metricsTokenSecret` in the Helm values (or
`metrics_token` in the config file) to require a bearer token on `/metrics`;
the chart's ServiceMonitor presents the token automatically
(`bearerTokenSecret` on the endpoint). The chart's NetworkPolicy admits only
the same namespace and the monitoring namespace regardless.

## Readiness semantics

`/health` is liveness: process up, always cheap, exempt from rate limiting.
`/ready` reports two independent facts:

```json
{"status": "ready", "migrations": "applied"}
{"status": "not_ready", "migrations": "pending"}
```

with distinct failure codes `DATABASE_UNAVAILABLE` and
`SCHEMA_MIGRATION_FAILED`. Schema migrations run in the background after the
listener opens — a rolling deploy never parks every replica unready behind one
long index build. A replica showing `pending` is waiting on the advisory lock
or still applying; `failed` needs the runbook below.

## Migration runner

- Files in `database/migrations/*.sql` apply in name order, each in one
  transaction recorded in `schema_migrations`.
- `-- shift:no-transaction` on the lines before a statement runs it outside any
  transaction block. Use it for `CREATE INDEX CONCURRENTLY` so rolling deploys
  do not hold `ACCESS EXCLUSIVE` locks on large tables:

  ```sql
  -- shift:no-transaction
  CREATE INDEX CONCURRENTLY IF NOT EXISTS big_table_idx ON big_table(created_at);
  ```

- Every migration ships a `NNN_name.down.sql` companion; `MigrateDown`
  reverts newest-first and refuses to revert a migration without one. It is
  callable from code (`Store.MigrateDown(ctx, steps)`) but **no operator
  command exposes it yet**, so the supported schema rollback story is
  restore-from-backup: runbooks must not promise `migrate down`. Keep a
  tested backup (see `scripts/backup-verify.sh`) in front of every migration
  train.
- Statement splitting is quote-aware: semicolons inside string literals,
  dollar-quoted function bodies, and comments never split statements.

## Chunk-store reclaim (disk exhaustion)

1. Watch `node_filesystem_avail_bytes` against the chunk-store mount
   (`ShiftChunkStoreDiskLow` at 15%).
2. **Online sweep** (preferred): the GC collector can run inside the agent via
   `gc.OpenOnline(stateDir, repo, chunks, guard)`, where `guard` is the same
   in-process lock the agent's checkpoint/repository/transfer writers hold.
   The two-phase grace period is unchanged: a chunk must be observed
   unreferenced twice before it is deleted, and every candidate is
   re-validated under the guard immediately before deletion.
3. **Offline maintenance** (fallback): stop the agent and run the standalone GC
   (`shift-gc`), which holds the cross-process state lock.
4. Content-addressed chunks shared by multiple checkpoints only disappear when
   *no* checkpoint references them; deleting old checkpoints (retention policy)
   is what makes chunks eligible in the first place.

## Backup & disaster-recovery drill

`scripts/backup-verify.sh` (procedure and escrow rules: `docs/backup-drill.md`)
is the restore-tested backup the launch checklist requires. Schedule it at
least daily next to the backup job; it performs a destructive restore into
explicit separate targets and fails on empty, truncated, or corrupt backups.

- The drill writes `shift_backup_last_run_timestamp_seconds`,
  `shift_backup_last_run_success` (1/0), and — on success —
  `shift_backup_last_success_timestamp_seconds` into
  `SHIFT_BACKUP_TEXTFILE_DIR`. Point that at the node_exporter textfile
  collector (`node_exporter --collector.textfile.directory=...`) so the series
  are scraped; the drill writes the file atomically on every run, success or
  failure.
- `ShiftBackupFailed` (page): the last run failed — rerun the drill by hand;
  its output names the failing leg. The newest backup is unverified until a
  run passes.
- `ShiftBackupStale` (page): no successful drill in 48h, or the series is
  absent entirely (drill never wired). A silently failing `pg_dump`, a stale
  backup directory, or a drill that stopped running raises exactly this.
- Keep the drill's keys escrow (`shift-keys.tar.gz`) and the
  `SHIFT_MASTER_KEY_FILE` keyfile out of the backup location the drill's
  metrics point at; see `docs/backup-drill.md` for the escrow procedure and
  the keyfile-loss policy.

## Trusted proxies

The control plane honors `X-Forwarded-For` / `X-Real-IP` **only** when the
socket peer is listed in `trusted_proxies` (Helm: `control.trustedProxies`).
Behind the TLS reverse proxy the deployment guide mandates, set it to the
proxy's pod/CIDR — otherwise all clients share one rate-limit bucket and audit
rows record the proxy address. The rightmost untrusted hop is the client, so a
client cannot forge its own address.

## Database pool

`SHIFT_DB_MAX_CONNS` (default 20), `SHIFT_DB_MIN_CONNS`,
`SHIFT_DB_MAX_CONN_LIFETIME`, `SHIFT_DB_MAX_CONN_IDLE_TIME`,
`SHIFT_DB_STATEMENT_TIMEOUT` (default 60s), `SHIFT_DB_LOCK_TIMEOUT` (default
10s). Migration sessions raise both timeouts again while applying DDL. Watch
`shift_db_pool_*` before raising the pool: saturation usually means slow
queries, not too few connections.
