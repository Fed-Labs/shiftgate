
# SHIFT

**CI without cold starts, on hardware you own.** SHIFT prepares one warm runner
checkpoint and turns it into burst capacity: `shiftgate clone CHECKPOINT_ID
--count 10` starts N independent workers from one warmed state, and the same
machinery evacuates a maintenance-window host without losing the job running on
it. The same commit gate makes every move safe — **state moves, and a failed
move never destroys the source.**

```bash
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

Every byte this fetches is SHA-256-verified before it runs or installs (the
installer against its published `install.sh.sha256`, the release tarball
against its `.tar.gz.sha256`). For a tamper-evident install, pin the
installer hash from a trusted channel (`SHIFT_INSTALL_SHA256=... bash
install.sh`) or run the two-step form from
[docs/installation.md](docs/installation.md).

Then run the reproducible proof (every step prints its measured time):
**[docs/ci-burst.md](docs/ci-burst.md)** — install → checkpoint a runner-like
workload → clone N workers → one move to a second machine → one deliberately
failed move that leaves the source alive.

SHIFT captures a managed workload with CRIU, packages the process and
explicitly-scoped filesystem state into content-addressed, encrypted chunks,
transfers only missing chunks, restores on a compatible destination, and
commits the move only after destination health validation.

Current release: `v0.2.0` · pre-1.0 — what that means for stability, state-format
compatibility, and upgrade windows is in the
[versioning and stability promise](docs/versioning.md). A live move is not
"zero downtime": it is a freeze, measured and reported as `downtime`
([benchmarks](docs/benchmarks.md)). Live migration is a paid plan feature on
every path, including peer-to-peer; the free tier is cold-only (2 machines,
10 GB) and always includes checkpoints, restores, same-machine clones, and
dry-run/preflight.

The repository builds and ships six binaries (plus the release tooling in
`cmd/shift-release`):

- `shift-agent`: privileged local workload, checkpoint, restore, and migration daemon.
- `shiftgate`: local and fleet command-line client. Named after the gate it opens — the bare name `shift` is a POSIX shell builtin, so a binary called `shift` can never be typed.
- `shift-control`: authenticated fleet metadata and orchestration API backed by PostgreSQL.
- `shift-gc`: offline chunk garbage collector, run with the agent stopped (see [Operations](docs/operations.md#garbage-collecting-checkpoint-chunks)).
- `shift-verify`: encrypted-state / support-matrix verifier used by the backup drill (see [the backup drill](docs/backup-drill.md)).
- `shift-relay`: migration relay for relay-mediated moves (see [Relay](docs/relay.md)).

Two clients ship alongside them: the web dashboard in `apps/web/` (Next.js) and
the Tauri desktop app in `apps/desktop/`.

The primary supported target is Linux x86_64. Other operating systems are represented by
interfaces only and are not reported as supported.

## Quick start

### The CI-burst proof (first runnable example)

One command downloads the release build, verifies its SHA-256, installs the
binaries, installs CRIU through your package manager when it is missing, and
enables the agent as a systemd service with a local-only configuration:

```bash
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

The service needs root, so the installer uses sudo when it must; everything
afterwards is the unprivileged `shiftgate` CLI. After it finishes — the
warm-once-run-many path, with measured times at every step in
[docs/ci-burst.md](docs/ci-burst.md):

```bash
shiftgate doctor                                             # verify the machine can checkpoint
mkdir -p ~/demo
shiftgate workload create demo --path ~/demo -- python3 -m http.server 8080
shiftgate workload start demo
shiftgate checkpoint create demo --leave-running             # warm state captured; process keeps running
shiftgate checkpoint list                                    # find the checkpoint id
shiftgate clone CHECKPOINT_ID --count 10 --prefix ci         # one warm checkpoint → 10 independent workers
shiftgate restore CHECKPOINT_ID                              # local round-trip check
```

`scripts/demo-ci-burst.sh` runs the same path end-to-end and prints a metrics
table (the docs page contains the identical commands).

The safety half of the proof is the failed move: point a migration at an
unreachable peer (`shiftgate migrate demo --to https://127.0.0.1:1`) and the
migration state machine refuses to commit — the source keeps running. That is
the invariant the e2e suite enforces: **a failed move never destroys the
source.**

The agent answers on `/run/shift/agent.sock`; CLI commands find it there by
default. A second machine with its own install is a migration target:
`shiftgate migrate demo --to https://host:8443` (see below for the peer TLS that
a remote listener requires). `--mode live` runs CRIU pre-dump passes while the
workload keeps running and freezes it only for the final delta. That second
machine can also hold a warm standby (a paid entitlement — HA Pack on Pro,
included on Business; free and Pro-without-HA accounts are refused with
`FAIL_OVER_PLAN_REQUIRED`):
`shiftgate workload failover demo --to https://standby:8443` replicates every
scheduled checkpoint to it, and it restores automatically when the source is
provably dead — unreachable, silent on the control plane, and positively
fenced by a configured fencing provider (none is configured by default, and
without one automatic restore is blocked) — or on `shiftgate standby
trigger`.

A checkpoint is more than a migration step. `shiftgate restore CHECKPOINT_ID --lazy`
starts the process before its memory is fully loaded and streams pages in on demand,
and `shiftgate clone CHECKPOINT_ID --count 50 --prefix worker` turns one warm
checkpoint into fifty independent running workloads on the same machine — each with
its own root and identity, reflink-copied where the filesystem allows. See
[the CLI reference](docs/cli.md).

### From a repository checkout

Prerequisites: Go 1.25+, GNU tar, and Linux with checkpoint/restore enabled.
CRIU policy: CRIU 3.17+ or 4.x; `criu check` must pass; certified lines are
listed in [docs/support-matrix.json](docs/support-matrix.json) (the
[benchmarks](docs/benchmarks.md) ran on CRIU 3.17.1; see
[Limitations](docs/limitations.md)).

`sudo ./install.sh` builds from the checkout and ends in the same state as the
release installer: binaries in `/usr/local/bin` (including `shift-gc`), the
`shift` system group with your user enrolled, a local-only `/etc/shift/agent.json`
(no TLS material), and the agent enabled as a systemd service. Pass
`--no-service` for binaries only. To run everything by hand instead:

```bash
make build
sudo ./bin/shift-agent --state-dir /var/lib/shift --listen unix:///run/shift/agent.sock
./bin/shiftgate doctor
./bin/shiftgate workload create demo --path "$PWD" -- /usr/bin/python3 -m http.server 8080
./bin/shiftgate workload start demo
./bin/shiftgate checkpoint create demo
```

Optional checkpoint mirroring can use local disk or an S3-compatible store. Configure the
`object_store` block in the agent JSON, or set the `SHIFT_OBJECTSTORE_*` and `SHIFT_S3_*`
variables shown in [the environment example](deployments/systemd/agent.env.example). Chunk
ciphertext and the encrypted manifest envelope are uploaded; workload keys remain in agent
state. If a checkpoint is saved locally but mirroring fails, retry it with:

```bash
./bin/shiftgate checkpoint mirror CHECKPOINT_ID
```

A hosted-control-plane deployment can also host checkpoint storage for its
organizations: agents set `SHIFT_OBJECTSTORE_BACKEND=control-plane` and fetch
short-lived, organization-scoped credentials from the control plane with the
machines-scope API key they already carry — no storage secrets on the agent.
Usage is metered from the bucket itself and the plan quota (10 GiB free /
120 GiB Pro / 500 GiB + 100 GiB per seat Business, hard-capped at 2 TiB) is
enforced fail-closed: over quota, credential issuance stops and new
checkpoints report the quota failure while local checkpointing continues.
See [the deployment guide](docs/deployment.md#hosting-checkpoint-storage-platform-side-mirroring).

For an unprivileged local evaluation, use a writable socket and state directory. Process
launch and inventory work normally; CRIU will accurately report missing kernel capabilities
instead of returning a fake checkpoint.

The control plane can be started with Docker Compose. Set a PostgreSQL password and two
independent peppers first:

```bash
export POSTGRES_PASSWORD="change-me"
export SHIFT_TOKEN_PEPPER="$(openssl rand -hex 32)"
export SHIFT_PASSWORD_PEPPER="$(openssl rand -hex 32)"
docker compose -f deployments/docker/compose.yml up --build
```

The web dashboard runs against it with `cd apps/web && npm install && cp
.env.example .env && npm run dev` — the control plane must list the
dashboard's origin in `SHIFT_ALLOWED_ORIGINS` or the browser blocks every
call (`SHIFT_ALLOWED_ORIGINS=http://localhost:3000`).

The API contract is [OpenAPI](api/openapi/shift.yaml); the service stores metadata and
authorization state only. Agents continue to exchange encrypted checkpoint chunks directly.

To make CLI-managed hosts visible in the web dashboard, register the machine in the control
plane, create an API key with the `machines` scope, and configure the optional reporter:

```bash
# Optional: unset means the platform's control plane. Set it only when this
# dashboard runs on a private control plane.
export SHIFT_CONTROL_URL="http://127.0.0.1:8090"
export SHIFT_CONTROL_ORGANIZATION_ID="ORGANIZATION_ID"
export SHIFT_CONTROL_MACHINE_ID="machine-source"
export SHIFT_CONTROL_AGENT_URL=""
export SHIFT_CONTROL_API_KEY="shift_ak_..."
```

The agent sends machine presence and inventory only. It does not upload workload keys,
checkpoint data, or grant the dashboard command execution. The dashboard dispatches a
bounded set of agent commands through the control plane — workload start/pause/resume/
stop/delete, checkpoint, and one-click migrations from the Workloads page or a workload's
detail page — using the machine's registered `agent_url`; it cannot create workloads or
run arbitrary commands.

```bash
mkdir -p ./data
./bin/shift-agent --state-dir ./data --listen unix://./data/agent.sock
./bin/shiftgate --agent unix://./data/agent.sock doctor
```

The same `shiftgate` binary manages a control-plane session and the fleet. Login stores the
session under `~/.config/shift/cli-session.json` (0600) keyed to the control plane's URL;
the password is prompted with echo disabled and never accepted as a flag:

```bash
./bin/shiftgate login --email operator@example.com   # the platform endpoint by default
./bin/shiftgate machines                 # fleet view while SHIFT_CONTROL_URL is set
./bin/shiftgate fleet entitlement
./bin/shiftgate marketplace inventory
```

See [the CLI documentation](docs/cli.md) for the fleet, marketplace, and update commands.

## Safety invariants

- Source termination is only legal after destination restore and health validation.
- Migration state and every transition are persisted before the next stage begins.
- Checkpoints are immutable. Corrupt or unauthenticated chunks fail closed.
- Filesystem capture is limited to paths explicitly attached to a workload.
- TCP and device state are marked portable, adaptable, or external; unsupported state is
  rejected or surfaced as a limitation.
- Remote agent listeners require mutual TLS unless explicitly started in development mode.

See [the CI-burst proof kit](docs/ci-burst.md) for the measured warm-pool and
rollback path, [Quickstart](docs/quickstart.md) for the fast path from install to a live
migration, then [Architecture](docs/architecture.md), [Security](docs/security.md), [Operations](docs/operations.md),
[Installation](docs/installation.md), [Deployment](docs/deployment.md), [API](docs/api.md), [CLI](docs/cli.md),
[Release Process](docs/release.md), [Troubleshooting](docs/troubleshooting.md), and
[Limitations](docs/limitations.md) for the complete contracts and deployment model.
[Versioning](docs/versioning.md) is the stability and compatibility promise;
[Positioning](docs/positioning.md) records who SHIFT is for and how it is framed.
[Development](docs/development.md) and [Contributing](CONTRIBUTING.md) cover the local test
environment — including the real-CRIU end-to-end suite — and the rules for changes.

Policies: [Support](docs/legal/support-policy.md), [Privacy](docs/legal/privacy-policy.md),
[Terms](docs/legal/terms.md), [Licensing](docs/legal/licensing.md), and the
[encryption export classification](docs/legal/export-classification.md).

## Licensing

Everything that runs on your machines is Apache-2.0. Everything that
coordinates a fleet is source-available (Elastic License 2.0). The hosted
service is the commercial product.

| Layer | Components | License |
|---|---|---|
| Runs on your machines | `shiftgate` CLI, `shift-agent`, `shift-gc`, `shift-verify`, K8s/Slurm integrations, desktop client, installers | Apache-2.0 |
| Coordinates a fleet | `shift-control`, `shift-relay`, the web dashboard, control-plane deployments | Elastic License 2.0 (source-available: internal use fine; offering it as a managed service is not) |
| Operated by us | The hosted control plane + hosted checkpoint storage, release signing | Commercial (the business) |

The one-liner you can repeat: "The agent and CLI are Apache-2.0; the fleet
control plane is source-available (ELv2); the hosted service is the business."
Live migration is an entitlement gate inside the Apache agent/CLI — a plan
feature, not a license change. The full map, per-directory breakdown, and FAQ
("Can I self-host the control plane?" — yes, under an Enterprise agreement)
are in [docs/legal/licensing.md](docs/legal/licensing.md).

## Development

```bash
make test
make test-race
make vet
docker compose -f deployments/docker/compose.yml up --build
```

The API contract is in `api/openapi/shift.yaml`; database migrations are under
`database/migrations`.

### End-to-end and stress tests

The suites in `tests/integration/` run real agents — Unix-socket local API,
TLS peer listener, encrypted chunk store, real CRIU — against real workloads:
shell, Python, Node.js, a web server with a declared port, multi-process
trees, and two-agent migrations over loopback TLS. They skip with an explicit
reason when the environment cannot run them; they never fake a pass.

```bash
# Everything CRIU-backed: start→checkpoint→restore, start→migrate→resume,
# migrate→failure→rollback, checkpoint→corrupt→detect, machine restart→resume.
sudo SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2E -v

# The heavyweight scenarios: huge memory states, 1500-process trees, a
# 150ms-latency proxy in the migration path, source death mid-transfer,
# disk exhaustion (tmpfs), and cgroup OOM.
sudo SHIFT_TEST_STRESS=1 SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestStress -v
```

Requirements: root (CRIU dump/restore), `criu` on PATH and healthy
(`criu check`), and — for the interpreter scenarios — `python3`/`node` on
PATH. `SHIFT_TEST_STRESS_BYTES` shrinks the huge-memory arena on slow
machines without changing what the scenario proves.

The desktop client's agent transport (Rust, hand-written HTTP/1.1 over the
agent's Unix socket) carries its own test module:

```bash
cargo test --manifest-path apps/desktop/src-tauri/Cargo.toml
```
