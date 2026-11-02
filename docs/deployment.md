# Deployment Guide

What production looks like: agents on every migration-capable Linux host, a
control plane behind TLS with PostgreSQL, and nothing else in between —
agents exchange encrypted checkpoint chunks directly with each other.

The security contract is in [Security](security.md) and
[Threat Model](threat-model.md); this document is about the moving parts.

## Topology

```
                     ┌────────────────────┐
 workstations ──────▶│  shift-control     │◀── reporting agents (presence,
 (CLI, desktop)      │  + PostgreSQL      │     inventory only)
                     └────────────────────┘
                              │  no checkpoint data ever crosses this line
   ┌──────────────────────────┼──────────────────────────┐
   ▼                          ▼                          ▼
 agent (host A) ◀── mTLS ──▶ agent (host B) ◀── mTLS ─▶ agent (host C)
```

- Agent ↔ agent: mutual TLS on `remote_listen` (e.g. 8443, as in
  `agent.json.example` — there is no code default; unset means no remote
  listener at all). Migrations,
  key transfer, chunk upload — all peer traffic.
- CLI/desktop ↔ agent: the local Unix socket on the same machine.
- Agent ↔ control plane: HTTPS presence reporting, API-key scoped. The
  control plane never receives workload keys or checkpoint data. Control-plane
  migration jobs prefer an explicitly configured `local_agent_url` Unix
  socket on a co-located worker. Managed dispatch may also fall back to the
  machine's public peer `agent_url` when `allow_remote_agent_dispatch`
  (`SHIFT_ALLOW_REMOTE_AGENT_DISPATCH`) is explicitly set to `true` — a
  security-relevant widening. It defaults to `false` in both `shift-control`
  and the shipped Helm chart: dispatch only through the local socket, the
  contract this document describes. Keep it `false` unless you deliberately
  want commands crossing the public peer listener; see
  [the config reference](configuration.md#control-plane).

## Agent host

Requirements per machine:

- Linux x86_64, kernel with CRIU support (`criu check` must pass)
- CRIU 3.17+ or 4.x; `criu check` must pass; certified lines are listed in
  [support-matrix.json](support-matrix.json). GNU tar; btrfs-progs only if you
  want atomic subvolume snapshots
- Root (the agent runs privileged: process management, CRIU, cgroups)

```bash
install -d -m 0755 /var/lib/shift
install -d -m 0755 /run/shift
install -m 0755 shift-agent /usr/local/bin/shift-agent
install -m 0644 deployments/systemd/shift-agent.service /etc/systemd/system/
# Mode 0600 is mandatory: the agent's loader refuses any config readable by
# group or other ("chmod 600") and crash-loops on anything wider.
install -m 0600 agent.json /etc/shift/agent.json
install -m 0600 agent.env /etc/shift/agent.env   # object store / reporting, if used
systemctl enable --now shift-agent
```

### Certificates

Production peer traffic requires three files, configured in the `tls` block:

| File | Role |
|---|---|
| `certificate_file` | this machine's peer server certificate |
| `private_key_file` | its private key |
| `client_ca_file` | CA that signs certificates other agents present as clients |

One internal CA for the fleet is the simple shape: every agent holds a
certificate from it, and every agent trusts it as the client CA. The
machine identity (the long-term signing key the agent keeps in its state
directory) is separate from these certificates and never leaves the host.

`insecure_development` (self-signed certificates, relaxed peer identity,
TLS verification skipped on the peer client) is for loopback evaluation
only. It is never a production posture.

### Verify a host is migration-capable

```bash
shiftgate doctor
sudo criu check
```

A host where `criu check` fails cannot checkpoint or restore. The agent
will run workloads there and report the gap honestly — it will never
produce a fake checkpoint — but migrations cannot originate or land on it.

## Control plane

```bash
install -m 0755 shift-control /usr/local/bin/shift-control
install -m 0644 deployments/systemd/shift-control.service /etc/systemd/system/

# The unit runs as the 'shift' system user and reads two files
# (deployments/systemd/control.env.example, control.json.example):
useradd --system --home-dir /var/lib/shift --shell /usr/sbin/nologin shift   # if missing
install -m 0600 -o root -g root control.env /etc/shift/control.env
install -m 0600 -o shift -g shift control.json /etc/shift/control.json
systemctl enable --now shift-control
```

`/etc/shift/control.env` carries `SHIFT_DATABASE_URL`, `SHIFT_TOKEN_PEPPER`,
`SHIFT_PASSWORD_PEPPER` (and any other `SHIFT_*` override — see
[the config reference](configuration.md)); `/etc/shift/control.json` is the
JSON configuration (0600 — the loader refuses any wider mode; keep it owned
by `shift`, the unit's user). The relay unit (`shift-relay.service`) runs as
the same user and additionally needs `relay.env` (0600 root-owned —
see [Relay](relay.md)).

Mandatory in production:

- **PostgreSQL over TLS** (`sslmode=verify-full` in the database URL). The
  schema holds user credentials (peppered hashes), sessions, and machine
  inventory. `verify-full` validates the server certificate against your CA
  and its hostname: put the CA in the host trust store or point the DSN at
  it (`sslrootcert=/path/to/ca.pem`), or the driver fails with "unknown
  authority" instead of connecting.
- **Two independent peppers**: `SHIFT_TOKEN_PEPPER` and
  `SHIFT_PASSWORD_PEPPER` (`openssl rand -hex 32` each), stored in a secret
  manager, not in the unit file. Rotation is not symmetric (see
  [Security — pepper rotation](security.md#pepper-rotation)): the token
  pepper can be rotated at the cost of a fleet-wide re-login; rotating the
  password pepper makes every stored password unverifiable and requires
  forced password resets. There is no re-hash migration — treat both as
  long-lived.
- **TLS termination** in front of the service (it serves plain HTTP); two
  replicas minimum behind it. Sessions are short-lived with transactional
  refresh, so replicas share nothing but the database.
- **Database backups**: migrations run in the background after the listener
  opens, so a rolling deploy never parks every replica unready behind one
  long index build. A replica whose migration **fails** keeps serving the
  old schema and reports `SCHEMA_MIGRATION_FAILED` on `/ready` (and
  `shift_schema_migration_failed` on `/metrics`) — it does not exit, so
  watch readiness, not crash loops, during upgrades. Restores are a
  database restore plus a service start.
- **`SHIFT_ALLOWED_ORIGINS`**: the comma-separated browser origins the web
  dashboard is served from (for example
  `https://app.example.com`), enabling CORS for exactly those origins. The
  dashboard's browser calls fail without an entry; the CLI, agents, and any
  other origin are unaffected.

Every SHIFT client defaults to the platform's control plane at
`https://shiftgate.dev` (`config.DefaultControlPlaneURL` in
`internal/config`; the dashboard carries the same built-in default) — the
way a hosted service's SDK ships its endpoint. Pointing a CLI or agent at
this private deployment instead is `--control-url` / `SHIFT_CONTROL_URL`;
the dashboard takes `NEXT_PUBLIC_API_URL`.

The local `deployments/docker/compose.yml` stack intentionally disables
PostgreSQL TLS for loopback development only. The solo/production stack
(`compose.solo.yml`) requires `./postgres-tls/{ca.crt,server.crt,server.key}`,
enables PostgreSQL TLS, and uses `sslmode=verify-full` with `postgres` as the
certificate hostname. Generate/provision those files from the organization's
CA; do not use self-signed throwaway material in production. The solo stack
also requires an off-host `RCLONE_REMOTE` and a mounted `rclone.conf`; set
`BACKUP_REMOTE_REQUIRED=true` (the default) so a local-only dump is not treated
as a production backup. The scheduled sidecar uploads nightly and retains
local dumps for the configured retention window.

Docker Compose in `deployments/docker/` builds the same two images for
evaluation; Helm charts are under `deployments/helm/`. Every `v*` tag runs
`.github/workflows/images.yml`, which publishes `shift-control`,
`shift-agent`, and `shift-relay` images to `ghcr.io/fed-labs/` — the chart's
default `image.repository` — so `helm install` with defaults works after the
first tagged release (override `image.repository`/`image.tag` for a private
mirror or a self-built image).

## Networking

Open between agent hosts, both directions:

| Port | Protocol | Purpose |
|---|---|---|
| 8443 (or your `remote_listen`) | TLS | peer migrations and chunk transfer |

The control plane needs its HTTPS port open to agents (outbound from the
agents' side) and to operator workstations. PostgreSQL is control-plane
internal. Nothing else: the local Unix socket is not a network listener,
and there is no data-plane path through the control plane to open.

High-latency links work — chunk transfer resumes missing-chunk negotiation
and the migration timeout is configurable (`--timeout`, default 7200 s) —
but size the timeout to the link: the e2e stress suite covers a 150 ms
latency path explicitly.

## Object storage (optional checkpoint mirroring)

When enabled, agents stream encrypted chunks and the encrypted manifest
envelope to S3 or a local backend. The backend never receives workload
keys; it stores ciphertext only. Mirrors serve deduplication and
availability for *new* transfers — restore reads local agent state, and
fetching a checkpoint back out of the mirror is not implemented, so object
storage is **not** a backup (run [the backup drill](backup-drill.md)). Configure with `SHIFT_OBJECTSTORE_*` /
`SHIFT_S3_*` (see `deployments/systemd/agent.env.example`) or the
`object_store` block in the agent JSON. Bucket-level encryption at rest and
a lifecycle policy on your side are complementary, not substitutes for the
agent-side encryption.

## Hosting checkpoint storage (platform-side mirroring)

The plans sell hosted checkpoint storage (10 GiB free, 120 GiB pro,
500 GiB + 100 GiB per seat business, hard-capped at 2 TiB — the live catalog
`GET /v1/plans` serves). To actually deliver it, the control plane brokers access to one
S3-compatible store it operates — MinIO in the compose deployment or your
own S3-compatible service in production:

```bash
# control-plane environment
SHIFT_STORAGE_ENABLED=true
SHIFT_STORAGE_ENDPOINT=http://minio.internal:9000     # what the control plane signs against
SHIFT_STORAGE_PUBLIC_ENDPOINT=https://storage.example.com   # what agents dial (defaults to ENDPOINT)
SHIFT_STORAGE_REGION=us-east-1
SHIFT_STORAGE_BUCKET=shift-checkpoints
SHIFT_STORAGE_ACCESS_KEY_ID=...                       # the single parent credential
SHIFT_STORAGE_SECRET_ACCESS_KEY=...
SHIFT_STORAGE_CREDENTIAL_TTL=1h                       # 15m–7d
SHIFT_STORAGE_RECONCILE_INTERVAL=5m
```

How it works:

- **One bucket, one prefix per organization.** Agents write only under
  `org/{organizationID}/`. They never see the parent credential: the control
  plane mints short-lived, prefix-scoped credentials via STS `AssumeRole`
  (MinIO STS or S3-compatible equivalent) and hands them to agents that ask
  with their machines-scope API key. Every issuance is audited
  (`storage.credentials_issued`) with the principal only — never a secret.
- **Usage is the bucket's truth.** A background reconciler lists each
  organization's prefix and sums object bytes into `used_storage_bytes`;
  agent claims are never trusted. A status read refreshes synchronously
  when the stored figures are older than the reconcile interval, and an
  admin can force a recount with
  `POST /v1/organizations/{org}/storage/reconcile`.
- **Quota is enforced fail-closed.** Over the plan limit, the control plane
  stops issuing credentials (403 `STORAGE_QUOTA_EXCEEDED`), so new
  checkpoint mirrors fail — and a checkpoint create whose mirror fails
  reports failure while the local checkpoint persists. Local checkpointing
  on the agent never stops. Restores read local agent state and do not
  depend on the mirror (downloading a checkpoint back out of object storage
  is not implemented — see [limitations](limitations.md)); the mirror is
  not a recovery path and must not be treated as a backup.
- **Agents opt in** with `object_store.backend: control-plane` (see
  [Installation](installation.md#hosted-checkpoint-mirroring)).

In compose, `--profile storage` starts MinIO and the bucket-creation job
alongside the control plane (pinned images, dedicated `shift-minio`
volume); the MinIO root user and password must match
`SHIFT_STORAGE_ACCESS_KEY_ID` / `SHIFT_STORAGE_SECRET_ACCESS_KEY`. Hosting
is opt-in: without the profile, the storage routes answer
`STORAGE_NOT_CONFIGURED` and existing behavior is unchanged.

The data plane is unchanged: the control plane brokers access but never
sees plaintext or workload keys — objects are AEAD ciphertext before they
leave the agent.

## Fleet rollout of a new agent version

Use the update system rather than reimaging:

1. Cut and sign the release (see [Release Process](release.md)).
2. Machines install per their update policy (`mandatory` or staged with a
   percentage) and swap the binary at a safe point — never mid-migration.
3. Watch versions in the control-plane machine inventory;
   `shiftgate update status` on a host shows pending/installed/blocked.
4. A bad release is undone with `shiftgate update rollback`; the undone version
   is blocked from reinstalling itself.

## Support matrix and evidence

The release support contract is versioned in [support-matrix.json](support-matrix.json).
It is evidence-based: `scripts/verify-support-matrix.sh` reports the observed kernel,
architecture, and CRIU probe result, while GPU rows require an external hardware runner.
Unknown combinations are not silently treated as compatible.

Run [the backup drill](backup-drill.md) on every release train and retain its result with
the support-matrix version. A release is not production-certified until the drill and the
real-CRIU integration lanes have run successfully. The drill verifies restored
encrypted state with `shift-verify`, which `make build` ships as
`bin/shift-verify` and the release tarball installs alongside the other
binaries (`scripts/backup-verify.sh` finds the repo build automatically;
override with `SHIFT_VERIFY_BIN`).

## Day-2 checklist

- [ ] `criu check` green on every agent host (part of `shiftgate doctor`).
- [ ] Certificates: expiry dates on the fleet's peer certificates tracked
      like any other TLS estate; one CA, one rotation story.
- [ ] Database backups scheduled and restore-tested.
- [ ] Object-store lifecycle: stale multipart uploads cleaned (the agent
      runs cleanup internally on a schedule; verify the bucket).
- [ ] Pepper rotation procedure documented for your secret manager —
      follow [Security — pepper rotation](security.md#pepper-rotation); the
      password pepper has no non-disruptive rotation path.
- [ ] Monitoring on `/health` and `/metrics` for the control plane, agent
      doctor failures, and migration failure rates from the diagnostics
      series.
