# SHIFT Operations

## Agent installation

1. Install Go-built `shift-agent`, CRIU, GNU tar, and the Linux cgroup v2 prerequisites.
2. Copy `deployments/systemd/agent.json.example` to `/etc/shift/agent.json`.
3. Provision the agent certificate and CA files referenced by that configuration.
4. Copy `deployments/systemd/agent.env.example` to `/etc/shift/agent.env` only when
   configuring object storage; keep it root-owned and mode `0600`.
5. Enable `deployments/systemd/shift-agent.service` and run `shift doctor`.

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
    "check_interval": 2160000000000
  }
}
```

`feed_file` replaces `feed_url` for an air-gapped fleet: an operator copies the feed and
its artifacts onto the machine, and the artifacts reference absolute `file://` paths.
Verification is identical either way — every release must carry detached Ed25519
signatures from at least `signature_threshold` distinct trusted keys over its canonical
form, and the digests of both the published bytes and the executable are checked.

The `policy` sets what the machine may do on its own: `manual` reports available releases
and waits, `mandatory` installs releases the publisher marked mandatory, and `automatic`
installs every applicable release once its rollout reaches this machine.

An update is never swapped in blindly. The staged binary must answer `--version` with the
version the release signed, the machine must not be running a migration, restore, fork, or
incoming transfer, and a release that cannot speak the control plane's current protocol or
read the on-disk configuration and state schemas is refused. The previous binaries are
preserved — three by default — so `shift update rollback` restores one without network
access, and the version it undid is blocked so the loop cannot immediately reinstall it.
A release whose install failed is blocked the same way; `shift update unblock VERSION`
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
after fixing credentials or connectivity with `shift checkpoint mirror CHECKPOINT_ID`.
The command is idempotent and verifies objects already present. Stale multipart uploads
can be inspected and cleaned through the object-store implementation's lifecycle tooling;
the agent also exposes the cleanup operation internally for scheduled maintenance.

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
database backups. The service runs migrations at startup and exits if a migration fails.

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
shift fleet sso --enforce example.com    # claim the domain for single sign-on
shift fleet sso                          # read the enforcement state
shift fleet sso --disable                # release the claim
```

From that moment the domain's users authenticate with `shift login --sso`, and password
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
`shift fleet retention [--audit-days N] [--checkpoint-days N] [--deleted-storage-days N]`
(admin role required); an omitted flag leaves its window unchanged and `0` is the
deliberate keep-forever policy. Every change lands in the audit trail as
`retention.update`.

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
