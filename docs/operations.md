# SHIFT Operations

## Agent installation

1. Install Go-built `shift-agent`, CRIU, GNU tar, and the Linux cgroup v2 prerequisites.
2. Copy `deployments/systemd/agent.json.example` to `/etc/shift/agent.json`.
3. Provision the agent certificate and CA files referenced by that configuration.
4. Copy `deployments/systemd/agent.env.example` to `/etc/shift/agent.env` only when
   configuring object storage; keep it root-owned and mode `0600`.
5. Enable `deployments/systemd/shift-agent.service` and run `shiftgate doctor`.

The Unix socket should be owned by root and the intended local operator group. Keep the
state directory on a filesystem with enough space for the largest checkpoint plus a
rollback copy.

## Agent updates

Updates are off until a machine is configured with both a release feed and the release
signing keys it trusts. To enable them, add an `updates` block to the agent configuration:

```json
{
  "updates": {
    "enabled": true,
    "channel": "stable",
    "policy": "mandatory",
    "feed_url": "https://releases.example/shift/feed.json",
    "trusted_keys_file": "/etc/shift/release-keys.json",
    "signature_threshold": 1,
    "check_interval": 21600000000000
  }
}
```

Durations in JSON configuration are raw nanosecond integers (Go
`time.Duration` encoding): `"check_interval": 21600000000000` is 6 hours
(2160000000000 would be 36 minutes). The
[config reference](configuration.md) lists every key with its default.

`feed_file` replaces `feed_url` for an air-gapped fleet: an operator copies the feed and
its artifacts onto the machine, and the artifacts reference absolute `file://` paths.
Verification is identical either way — every release must carry detached Ed25519
signatures from at least `signature_threshold` distinct trusted keys over its canonical
form, and the digests of both the published bytes and the executable are checked.

**Until the key ceremony runs** (see
[release.md](release.md#key-ceremony)) `deployments/release-keys.json` holds
only PLACEHOLDER entries and no signature will verify against it: updates
enabled with that key ring fail closed for every machine, deliberately.
Either run the ceremony and pin the real key before enabling this block, or
keep `updates.enabled` false for the launch build.

The `policy` sets what the machine may do on its own: `manual` reports available releases
and waits, `mandatory` installs releases the publisher marked mandatory, and `automatic`
installs every applicable release once its rollout reaches this machine.

An update is never swapped in blindly. The staged binary must answer `--version` with the
version the release signed, the machine must not be running a migration, restore, fork, or
incoming transfer, and a release that cannot speak the control plane's current protocol or
read the on-disk configuration and state schemas is refused. The previous binaries are
preserved — three by default — so `shiftgate update rollback` restores one without network
access, and the version it undid is blocked so the loop cannot immediately reinstall it.
A release whose install failed is blocked the same way; `shiftgate update unblock VERSION`
re-arms it.

Publishing is done with `shift-release`: `keygen` creates a signing key pair, `sign` signs
one release document, `feed` assembles signed releases into a feed, and `verify` checks
the finished feed against a key file the same way an installer will. Run `verify` as the
last step before publication.

## Checkpoint object storage

Mirroring is disabled by default. The local backend is useful for a second filesystem or a
self-hosted deployment; the S3 backend accepts AWS S3 and compatible endpoints. The agent
streams encrypted chunks and the encrypted manifest envelope directly to the backend and
verifies size and SHA-256 metadata on upload and download. The backend does not receive
workload keys or plaintext workload environment metadata.

Checkpoint creation commits the encrypted checkpoint locally before attempting the mirror.
A transient mirror failure therefore does not discard the checkpoint. Retry publication
after fixing credentials or connectivity with `shiftgate checkpoint mirror CHECKPOINT_ID`.
The command is idempotent and verifies objects already present. Stale multipart uploads
can be inspected and cleaned through the object-store implementation's lifecycle tooling;
the agent also exposes the cleanup operation internally for scheduled maintenance.

## Garbage collecting checkpoint chunks

Chunk-store growth is reclaimed with garbage collection, and **online GC is
shipped**: `shiftgate gc` drives the collector inside the running agent
(`gc.OpenOnline`), which reclaims unreferenced chunks while the agent keeps
serving. Deletion is guarded by the same in-process write lock the agent's
checkpoint/repository/transfer writers hold, so no chunk can disappear
between a writer resolving it and installing its reference. GC requires root
(the CLI talks to the agent's local socket as root; without it the agent
answers `GC_ROOT_REQUIRED`).

```sh
sudo shiftgate gc --grace 24h --dry-run        # plan only
sudo shiftgate gc --grace 24h --dry-run=false  # delete orphans past the grace period
```

`--grace` (default 24h) is the minimum age an orphaned chunk must reach
before deletion: the sweeper records its first observation of an orphan and
only deletes once the grace period has elapsed across a persisted
observation, so a chunk orphaned by a running checkpoint or restore is never
swept in the same breath. The sweeper deletes only content-addressed chunks
that no retained checkpoint manifest references; manifests, checkpoint
directories, workload keys, and any mirrored cloud objects are untouched
(cloud-object cleanup is operator-side — see [Limitations](limitations.md)).

**Offline sweep (fallback)**: if the agent is down or cannot open its state,
stop it and run the standalone `shift-gc`, which holds the cross-process
state lock:

```sh
sudo systemctl stop shift-agent
sudo shift-gc --state-dir /var/lib/shift --grace 24h --dry-run        # plan only
sudo shift-gc --state-dir /var/lib/shift --grace 24h --dry-run=false  # delete orphans past the grace period
sudo systemctl start shift-agent
```

Point `--state-dir` at the agent's `state_dir` when it is not
`/var/lib/shift`. `shiftgate gc` prints these offline commands when the
agent refuses an online sweep.

## Durable control-plane migrations

The migration API is a durable queue, not a synchronous proxy. Register each machine with
both its public peer `agent_url` and, when using platform-mediated migrations, an explicit
`local_agent_url` Unix socket reachable by the control-plane worker on the same host. The
worker never derives or guesses this path from a public URL. It may fall
back to the machine's public peer `agent_url` as the command endpoint only
when `allow_remote_agent_dispatch` (`SHIFT_ALLOW_REMOTE_AGENT_DISPATCH`) is
explicitly set to `true` — a security-relevant widening. It defaults to
`false` (in `shift-control` and the shipped Helm chart), which guarantees the
contract above (dispatch only through `local_agent_url`; the peer listener is
never a command endpoint). Jobs created by older deployments without a local endpoint remain
marked failed with an actionable error; register the endpoint and submit a new migration job.

Clients should send an `Idempotency-Key` on migration creation. A retry with the same key
and the same workload/source/destination/mode returns the original job; reusing it with
different parameters is rejected. Jobs carry a lease and attempt count, and a control-plane
restart allows another worker to reclaim an expired lease. Agent migration state remains the
data-plane authority; the control-plane record is the durable coordinator view.

## Control plane installation

For local development:

```sh
export POSTGRES_PASSWORD='change-this'
export SHIFT_TOKEN_PEPPER="$(openssl rand -hex 32)"
export SHIFT_PASSWORD_PEPPER="$(openssl rand -hex 32)"
docker compose -f deployments/docker/compose.yml up --build
```

For production, use PostgreSQL TLS (`sslmode=verify-full`), store peppers in a secret
manager, run at least two control replicas behind a TLS reverse proxy, and apply regular
database backups. On managed platforms the platform layer takes the database backups
(point-in-time recovery and volume-backup schedules are platform features there);
the restore-test discipline of [backup-drill.md](backup-drill.md) still applies. Migrations run in the background after the listener opens: a replica
whose migration fails keeps serving the old schema and reports
`SCHEMA_MIGRATION_FAILED` on `/ready` (and `shift_schema_migration_failed` on
`/metrics`) — it does **not** exit. Treat `/ready`, not a crash loop, as the
signal; see [Troubleshooting](troubleshooting.md) and the migration runner notes
in `deployments/monitoring/README.md`.

## Single sign-on (OIDC) and SCIM provisioning

Configure one OIDC issuer for the whole control plane — all three values together or
none, in the control-plane configuration file or environment:

```sh
export SHIFT_OIDC_ISSUER='https://sso.example.com'
export SHIFT_OIDC_CLIENT_ID='shift-control'
export SHIFT_OIDC_CLIENT_SECRET='...'   # from your secret manager, never source control
```

The issuer must be an `https` URL; the control plane fetches its discovery document and
JWKS on first use and verifies ID tokens itself (RS256 and ES256). Register the control
plane as a **confidential, server-side** client — the secret is used only at the token
endpoint as HTTP Basic authentication, and redirect URIs are the CLI's loopback listener,
so no web redirect needs to be registered.

Once configured, an organization admin claims an email domain:

```sh
shiftgate fleet sso --enforce example.com    # claim the domain for single sign-on
shiftgate fleet sso                          # read the enforcement state
shiftgate fleet sso --disable                # release the claim
```

From that moment the domain's users authenticate with `shiftgate login --sso`, and password
login and self-service registration for the domain are refused. A domain can be claimed by
one organization at a time; a federated login automatically joins the enforcing
organization as a viewer.

For directory provisioning, create an API key with the `scim` scope and configure the
identity provider's SCIM connector to `https://control.example.com/v1/scim/v2` with that
key as a bearer token. Point the connector's user mapping at `userName` (the account's
email) and `externalId`. Only the Users resource is implemented — Groups is not — and
`DELETE` deactivates the account rather than destroying it, so re-enabling in the IdP
re-activates the same account with its history intact.

## Retention

Each organization owns three retention windows, in days: audit events, checkpoints, and
the grace period for already-deleted storage objects. Read and change them with
`shiftgate fleet retention [--audit-days N] [--checkpoint-days N] [--deleted-storage-days N]`
(admin role required); an omitted flag leaves its window unchanged and `0` is the
deliberate keep-forever policy — on every plan tier. Every change lands in the
audit trail as `retention.update`.

Windows are plan-capped: Free and Pro organizations may set at most 365
days per window (`402 RETENTION_PLAN_LIMIT` above it); Business and
Enterprise (and negotiated keys) may run to the 36500-day ceiling. `0`
(keep forever) is available on every tier. The caps are enforced by
`PUT /v1/organizations/{id}/retention` — a window above the cap is refused
with the error above, never silently shortened.

The control plane enforces the windows on a background sweep — `retention_sweep_interval`
in the control-plane configuration, hourly by default, `0` disables it. Audit rows past
their window are deleted outright. Checkpoints past theirs are marked `deleted`: the row
and its lineage survive so history stays answerable, but the checkpoint is no longer
restorable. Storage objects that were already soft-deleted are purged after their grace
period. A sweep is three set-based statements, so its cost tracks what is actually
expiring; a failed sweep logs a warning and retries on the next tick, and because
retention is monotone a skipped pass only delays deletion.

## Incident handling

Inspect `/health`, `/metrics`, agent `doctor`, migration event history, and audit events in
that order. Never delete an agent state directory while a migration or restore transaction
is active. A source marked `SourcePreserved` is intentionally retained until an operator
confirms destination state and cleanup.
