# Installation

## One-command install (released build)

The fastest path on a supported host — no Go toolchain, no compiler:

```sh
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

The installer (`deployments/dist/install.sh` in the repository):

- verifies Linux x86_64, downloads `shiftgate-linux-amd64.tar.gz`, and refuses to
  install unless its SHA-256 matches the release's `.sha256` sidecar;
- installs `shiftgate`, `shift-agent`, and `shift-control` under `/usr/local/bin`
  (sudo is used only when the target prefix needs it — `--prefix` a writable
  directory and no password is ever asked);
- installs CRIU through the distribution's package manager when it is missing;
- creates the `shift` system group, adds the installing user to it, and the
  agent hands its Unix socket to that group — so the CLI works unprivileged
  after one re-login (or `newgrp shift`);
- writes a local-only `/etc/shift/agent.json` — a Unix-socket API with no
  remote listener, so no TLS material is needed — and enables the agent as a
  systemd service.

Pass `--no-service` for binaries only, `--prefix DIR` to choose the location,
`--url URL` to fetch from a different release location (or set
`SHIFT_RELEASE_URL`). The same options work when piping from curl by exporting
the environment variable instead of passing the flag.

To accept migrations from other machines, the agent needs a remote listener,
which requires mutual TLS. Set `remote_listen` and the `tls` block in
`/etc/shift/agent.json` following `deployments/systemd/agent.json.example`,
then `systemctl restart shift-agent`.

## Publishing a release

A maintainer cuts a release with:

```sh
make dist                                                   # dist/shiftgate-linux-amd64.tar.gz + .sha256 + .txt
gh release create vX.Y.Z dist/shiftgate-linux-amd64.tar.gz \
                     dist/shiftgate-linux-amd64.tar.gz.sha256 \
                     dist/shiftgate-linux-amd64.txt \
                     deployments/dist/install.sh
```

Uploading `install.sh` itself as an asset is what makes the one-command URL
above work — `releases/latest/download/install.sh` always resolves to the
newest release's copy. The signing ceremony for agent-driven updates is
described in [the release process](release.md); it is independent of this
installer, which pins nothing and relies on TLS plus the checksum sidecar.

## One-command install (from sources)

From a repository checkout on a supported host:

```sh
./install.sh
```

The installer verifies Linux x86_64, installs distribution packages for Go/CRIU/build tools,
builds from local sources, verifies every binary checksum, and installs `shiftgate`,
`shift-agent`, and `shift-control` under `/usr/local/bin`. It does not generate private
TLS material or expose a remote listener.

To install the systemd unit as well, first place the certificate, key, and peer CA at the
paths in `deployments/systemd/agent.json.example`, then run:

```sh
sudo ./install.sh --systemd
```

Use `--prefix /custom/path` for a non-system prefix. From the moment the
installer begins replacing binaries, any failure — a failed step as much as a
failed service activation — restores the prior installation exactly: replaced
binaries come back from their `.pre-shift` aside copies, binaries the run
itself added are removed, and a replaced systemd unit is put back.

For an unprivileged test install outside the protected system paths, set both the prefix
and the explicit opt-in:

```sh
./install.sh --prefix "$HOME/.local/opt/shift" # with SHIFT_ALLOW_USER_PREFIX=1
```

## Linux agent

Build the binaries with Go 1.24 or newer:

```sh
make build
sudo install -m 0755 bin/shift-agent /usr/local/bin/shift-agent
install -m 0755 bin/shiftgate "$HOME/.local/bin/shiftgate"
sudo install -d -m 0750 /etc/shift/tls
sudo install -m 0644 deployments/systemd/agent.json.example /etc/shift/agent.json
# Optional: configure S3/local checkpoint mirroring.
sudo install -m 0600 deployments/systemd/agent.env.example /etc/shift/agent.env
sudo systemctl enable --now shift-agent.service
```

Before enabling the service, replace the example TLS paths with certificates issued by
the private agent CA. For an unprivileged evaluation, use a writable state directory and
Unix socket; CRIU will report missing capabilities accurately.

Leave `/etc/shift/agent.env` absent or keep `SHIFT_OBJECTSTORE_ENABLED` unset to disable
remote checkpoint mirroring. When enabled, provide an S3-compatible endpoint and
credentials in that file; do not put access keys in the agent JSON committed to source
control.

### Hosted checkpoint mirroring

When the control plane hosts storage (see
[Deployment — hosting checkpoint storage](deployment.md#hosting-checkpoint-storage-platform-side-mirroring)),
agents can mirror without any storage credentials of their own. Add to
`/etc/shift/agent.env` alongside the control-plane reporter settings above:

```sh
SHIFT_OBJECTSTORE_ENABLED=true
SHIFT_OBJECTSTORE_BACKEND=control-plane
SHIFT_OBJECTSTORE_STATE_DIR=/var/lib/shift/objectstore-state
```

The agent fetches short-lived, organization-scoped credentials from the
control plane with the machines-scope API key it already carries, refreshing
at half the credential lifetime — no storage secrets ever sit on the agent.
Before the first fetch, mirroring (and therefore checkpoint create) reports
`credentials not yet available` rather than silently skipping the cloud copy.
If the organization goes over its storage quota, credential issuance stops and
checkpoints report a quota failure while local checkpointing continues
untouched; mirroring resumes once usage drops below the plan limit (the
dashboard's reconcile brings usage back to the bucket's truth) or the plan is
upgraded.

## Connecting an agent host to the dashboard

The web app talks to `shift-control`; the CLI talks to the local `shift-agent`. For
dashboard visibility of a CLI-managed host, first register that machine in the web app,
create an organization API key scoped to `machines`, then add these settings to
`/etc/shift/agent.env`:

```sh
# Optional: unset means the platform's control plane — the endpoint every
# SHIFT client dials by default. Set it only when the dashboard runs on a
# private control plane.
# SHIFT_CONTROL_URL=https://control.example.test
SHIFT_CONTROL_ORGANIZATION_ID=ORGANIZATION_ID
SHIFT_CONTROL_MACHINE_ID=machine_id_from_inventory
SHIFT_CONTROL_MACHINE_NAME=edge-01
# Set this only when the agent has a mutually authenticated remote peer listener.
SHIFT_CONTROL_AGENT_URL=https://edge-01.example.test:8443
SHIFT_CONTROL_API_KEY=shift_ak_replace_me
SHIFT_CONTROL_REPORT_INTERVAL=30s
```

Restart `shift-agent.service` afterwards. The key authorizes presence and inventory
reporting; the dashboard dispatches workload commands (including one-click migrations
from the Workloads page) through the agent's remote peer listener — set
`SHIFT_CONTROL_AGENT_URL` for that, and machines without a reachable listener stay
report-only. The reporter does not reconcile CLI-created
workload or checkpoint records into the dashboard.

## Control plane

PostgreSQL 14 or newer is required. Set `SHIFT_DATABASE_URL`,
`SHIFT_TOKEN_PEPPER`, and `SHIFT_PASSWORD_PEPPER` (each pepper must contain at least 32
characters), then run:

```sh
./bin/shift-control --listen 127.0.0.1:8090
```

Running a private control plane: every SHIFT client dials the platform
endpoint (`config.DefaultControlPlaneURL`) by default — the way a hosted
service's SDK ships its endpoint — so to point clients at this private
instance, set `--control-url` / `SHIFT_CONTROL_URL` (CLI, agents) or
`NEXT_PUBLIC_API_URL` (dashboard) to its address (for a local instance,
`http://127.0.0.1:8090`, matching the stock listen address above).

The service applies `database/migrations/*.sql` transactionally at startup. Do not expose
the development HTTP listener directly to the public internet; terminate TLS at a
reverse proxy and enforce network policy there.

### Billing (optional)

Stripe subscription billing is off until configured. Three settings turn it on:

```json
{
  "stripe_secret_key": "sk_live_...",
  "stripe_webhook_key": "whsec_...",
  "stripe_prices": {
    "pro": "price_...",
    "business": "price_..."
  }
}
```

The secret key lets the control plane open Stripe-hosted checkout and billing-portal
sessions; the webhook key verifies subscription events at `/v1/webhooks/stripe`; each
price id enables self-service checkout for one plan. The same values can be supplied with
`-stripe-secret-key`, `-stripe-webhook-key`, and repeatable
`-stripe-price plan=price_id` flags, or `SHIFT_STRIPE_PRICE_<PLAN>` environment variables.
Unconfigured billing answers checkout and portal requests with `503 BILLING_NOT_CONFIGURED`
— it never simulates a purchase. Plan limits always come from the server-side catalog, not
from the frontend or webhook payloads.
