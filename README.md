# SHIFT

SHIFT is a Linux-first computation mobility platform. It captures a managed workload with
CRIU, packages the process and explicitly-scoped filesystem state into content-addressed,
encrypted chunks, transfers only missing chunks, restores on a compatible destination, and
commits the move only after destination health validation.

The repository contains three runnable programs:

- `shift-agent`: privileged local workload, checkpoint, restore, and migration daemon.
- `shiftgate`: local and fleet command-line client. Named after the gate it opens — the bare name `shift` is a POSIX shell builtin, so a binary called `shift` can never be typed.
- `shift-control`: authenticated fleet metadata and orchestration API backed by PostgreSQL.

The primary supported target is Linux x86_64. Other operating systems are represented by
interfaces only and are not reported as supported.

## Quick start

### From a release (no toolchain needed)

One command downloads the release build, verifies its SHA-256, installs the
binaries, installs CRIU through your package manager when it is missing, and
enables the agent as a systemd service with a local-only configuration:

```bash
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

The service needs root, so the installer uses sudo when it must; everything
afterward is the unprivileged `shiftgate` CLI. After it finishes:

```bash
shiftgate doctor                                             # verify the machine can checkpoint
shiftgate workload create demo --path ~/demo -- python3 -m http.server 8080
shiftgate workload start demo
shiftgate checkpoint create demo                             # freeze to an encrypted snapshot
shiftgate checkpoint list                                    # find the snapshot id
shiftgate restore CHECKPOINT_ID                              # bring it back
```

The agent answers on `/run/shift/agent.sock`; CLI commands find it there by
default. A second machine with its own install is a migration target:
`shiftgate migrate demo --to https://host:8443` (see below for the peer TLS that
a remote listener requires).

### From a repository checkout

Prerequisites: Go 1.24+, CRIU 4+, GNU tar, and Linux with checkpoint/restore enabled.

```bash
make build
sudo ./bin/shift-agent --state-dir /var/lib/shift --listen unix:///run/shift/agent.sock
./bin/shiftgatedoctor
./bin/shiftgateworkload create demo --path "$PWD" -- /usr/bin/python3 -m http.server 8080
./bin/shiftgateworkload start demo
./bin/shiftgatecheckpoint create demo
```

Optional checkpoint mirroring can use local disk or an S3-compatible store. Configure the
`object_store` block in the agent JSON, or set the `SHIFT_OBJECTSTORE_*` and `SHIFT_S3_*`
variables shown in [the environment example](deployments/systemd/agent.env.example). Chunk
ciphertext and the encrypted manifest envelope are uploaded; workload keys remain in agent
state. If a checkpoint is saved locally but mirroring fails, retry it with:

```bash
./bin/shiftgatecheckpoint mirror CHECKPOINT_ID
```

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

The API contract is [OpenAPI](api/openapi/shift.yaml); the service stores metadata and
authorization state only. Agents continue to exchange encrypted checkpoint chunks directly.

To make CLI-managed hosts visible in the web dashboard, register the machine in the control
plane, create an API key with the `machines` scope, and configure the optional reporter:

```bash
export SHIFT_CONTROL_URL="http://127.0.0.1:8090"
export SHIFT_CONTROL_ORGANIZATION_ID="ORGANIZATION_ID"
export SHIFT_CONTROL_MACHINE_ID="machine-source"
export SHIFT_CONTROL_AGENT_URL=""
export SHIFT_CONTROL_API_KEY="shift_ak_..."
```

The agent sends machine presence and inventory only. It does not upload workload keys,
checkpoint data, or grant the dashboard command execution. The dashboard still cannot
create or dispatch CLI workload operations.

```bash
mkdir -p ./data
./bin/shift-agent --state-dir ./data --listen unix://./data/agent.sock
./bin/shiftgate--agent unix://./data/agent.sock doctor
```

The same `shiftgate` binary manages a control-plane session and the fleet. Login stores the
session under `~/.config/shift/cli-session.json` (0600) keyed to the control plane's URL;
the password is prompted with echo disabled and never accepted as a flag:

```bash
./bin/shiftgate--control-url http://127.0.0.1:8090 login --email operator@example.com
./bin/shiftgatemachines                 # fleet view while a control plane is configured
./bin/shiftgatefleet entitlement
./bin/shiftgatemarketplace inventory
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

See [Architecture](docs/architecture.md), [Security](docs/security.md), [Operations](docs/operations.md),
[Installation](docs/installation.md), [Deployment](docs/deployment.md), [API](docs/api.md), [CLI](docs/cli.md),
[Release Process](docs/release.md), [Troubleshooting](docs/troubleshooting.md), and
[Limitations](docs/limitations.md) for the complete contracts and deployment model.
[Development](docs/development.md) and [Contributing](CONTRIBUTING.md) cover the local test
environment — including the real-CRIU end-to-end suite — and the rules for changes.

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
