# Development Guide

How to build, test, and debug SHIFT locally — including the local test
environment that runs the real CRIU end-to-end suite on one machine.

## Layout

```
cmd/shift           CLI client (local agent + fleet operations)
cmd/shift-agent     the agent daemon
cmd/shift-control   the control-plane server
internal/agent      local API (Unix socket), peer API (TLS), service wiring
internal/checkpoint CRIU engine, manifests, restore transactions, forks
internal/chunkstore content-addressed encrypted chunk storage
internal/migration  the migration orchestrator and stage machine
internal/transfer   peer transfer sessions (reserve → key → chunks → verify)
internal/runtime    process manager, cgroups
internal/filesystem snapshot/changed-file tracking/dependency discovery
internal/network    port reservation, forwarding, network plans
internal/device     GPU/device detection and compatibility tiers
internal/controlplane  HTTP API, sessions, machines, billing
internal/database   PostgreSQL store and migrations
internal/observability logging, metrics, tracing, diagnostics
apps/desktop        Tauri v2 + React desktop client
tests/integration   the real-workload e2e and stress suites
```

## Build and unit tests

```bash
make build        # all three binaries into ./bin
make test         # unit tests
make test-race    # race detector
make vet
make lint         # fmt + vet + test
make lint-ci      # what CI enforces (no tests)
```

## Local test environment

### Control plane (optional, for control-plane work)

Unit tests that hit PostgreSQL read `SHIFT_TEST_DATABASE_URL` and skip with
an explicit message when it is unset. The easiest local setup:

```bash
docker run -d --name shift-test-pg -p 5432:5432 \
  -e POSTGRES_PASSWORD=shift-test -e POSTGRES_DB=shift_test postgres:16
export SHIFT_TEST_DATABASE_URL="postgres://postgres:shift-test@localhost:5432/shift_test?sslmode=disable"
go test ./internal/controlplane/... -count=1
```

CI starts the same service container (see `.github/workflows/ci.yml`).

### End-to-end: real CRIU, real workloads

The suite in `tests/integration/` boots whole agents on this machine — local
Unix-socket API, TLS peer listener with a development self-signed
certificate, encrypted chunk store — and runs real workloads through real
checkpoint/restore and real two-agent migrations over loopback TLS.

Requirements:

- root (`sudo`/root shell): CRIU dump/restore needs it
- `criu` on PATH and healthy (`criu check`)
- `python3` and `node` on PATH for the interpreter scenarios (they skip
  otherwise)

```bash
sudo SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2E -v -count=1
```

What it covers:

| Scenario | Test |
|---|---|
| shell start → checkpoint → restore, counter resumes | `TestE2EShellCheckpointRestore` |
| Python process checkpoint/restore, variable state intact | `TestE2EPythonProcess` |
| Node.js application checkpoint/restore | `TestE2ENodeApplication` |
| web server with a declared port, in-memory hit counter resumes | `TestE2EWebServer` |
| multi-process tree checkpoint/restore | `TestE2EMultiProcessWorkload` |
| PID-namespaced (container-style) workload checkpoint/restore, namespace recreated | `TestE2EContainerizedWorkload` |
| two-agent migration completes, source cleaned up | `TestE2EMigrationCompletes` |
| unreachable destination → rollback, source resumes | `TestE2EMigrationUnreachableDestination` |
| destination dies mid-transfer → failure, source resumes | `TestE2EMigrationDestinationDiesMidTransfer` |
| checkpoint → corrupt chunk bytes → restore refuses | `TestE2ECheckpointCorruptionDetected` |
| agent restart over the same state → checkpoint still restores | `TestE2EAgentRestartResume` |

### Stress suite

```bash
sudo SHIFT_TEST_E2E=1 SHIFT_TEST_STRESS=1 \
  go test ./tests/integration/ -run TestStress -v -count=1 -timeout 45m
```

Huge memory arena (default 512 MiB, shrink with `SHIFT_TEST_STRESS_BYTES`),
1500-process trees, a 150 ms-latency TCP proxy in the migration path, source
agent death mid-transfer with journal recovery, disk exhaustion on a 128 MiB
tmpfs, and a cgroup OOM kill.

### Desktop client

```bash
make desktop        # frontend type-check+bundle, then cargo build
make test-desktop   # the Rust agent-transport tests (HTTP/1.1 over the socket)
python3 scripts/make-icon.py   # regenerate src-tauri/icons/icon.png if needed
```

The icon script needs no third-party packages — it is a stdlib PNG writer.

## Debugging tips

- Agent logs are slog-based; set `"log_level": "debug"` in the agent config
  for stage-by-stage migration output.
- `shiftgate doctor` runs the same health checks the API exposes at
  `/v1/doctor`.
- A migration's event log (`GET /v1/migrations/{id}`) records every stage
  transition, failure code, and byte count — start there before reading
  code.
- Restore transactions persist under `state_dir/metadata/restores.enc.json`;
  a `ROLLED_BACK` record's `error` field names the failing step.

## Where to add things

- A new local API route: `internal/agent/api.go` (route + handler), a client
  method in `internal/agentclient/client.go`, then the CLI surface in
  `cmd/shift/main.go`.
- A new migration stage: extend the stage constants and transition map in
  `internal/model/types.go` first — the map is the contract the orchestrator
  and every test rely on.
- A new environment variable: document it in
  `deployments/systemd/agent.env.example` and `docs/installation.md`.
