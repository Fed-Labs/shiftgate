# The CI-burst proof kit

**CI without cold starts, on hardware you own.** This page is one reproducible
path: prepare one warm runner-like workload, checkpoint it once, clone it into
N independent workers, move it to a second machine, and deliberately fail a
move to show that the source survives. Every step prints its measured duration.
The same commands appear on the web page at `https://shiftgate.dev/docs/ci-burst`
and are driven end-to-end by `scripts/demo-ci-burst.sh` — every command below
is one the script runs (the script adds `--json` variants of two read-only
commands to read ids and counts for its assertions, and prints a measured
duration for each).

The promise this proves: **state moves, and a failed move never destroys the
source.**

Honest limits, before you run it:

- Linux x86_64; a privileged agent and a CRIU-capable kernel are required
  (`shiftgate doctor` reports real gaps; `criu check` must pass). Certified
  lines: [docs/support-matrix.json](support-matrix.json).
- A live move is **not** zero downtime — it is a freeze, measured and reported
  as `downtime` in the migration record. On the benchmark workloads in
  [docs/benchmarks.md](benchmarks.md) that freeze plus restore was 3.2–3.5 s
  (live) vs 3.7–5.5 s (cold); the transfer inside the freeze was 107–116 ms
  on loopback (a cold move transfers 215–223 ms while the workload stays
  stopped).
- `clone` is **same-machine** (each worker gets its own root and identity,
  reflink-copied where the filesystem allows). Cloning across machines is a
  migrate, not a clone.
- `--mode live` is a **paid plan feature on every path, including
  peer-to-peer**. The free tier is cold-only (2 machines, 10 GB hosted
  storage). Cold moves, checkpoints, restores, same-machine clones, and
  dry-run/preflight are free.
- Pre-1.0 (`v0.2.x`): a MINOR bump may break automation — see
  [the versioning promise](versioning.md). Full boundary list:
  [docs/limitations.md](limitations.md).

## The path (identical to the demo script)

Target: a fresh Ubuntu 24.04 box with `scripts/demo-ci-burst.sh` completing
with N=10 in ≤ 10 minutes wall, zero manual steps. Every step below is run by
the script and prints its own measured time in the script output.

### 0. Install (once)

```bash
curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
```

The installer verifies the release SHA-256, installs binaries (including
`shift-gc`), installs CRIU through your package manager when it is missing,
and enables the agent as a systemd service with a local-only configuration.

### 1. Check the machine

```bash
shiftgate doctor
```

CRIU health, kernel features, archive tooling, and privileges — real checks,
no fake checkpoints. Exit code 3 means a required check failed; fix that
first.

### 2. Create a runner-like workload

A warmed CI worker is just a long-lived process with hot caches and a fetched
dependency tree. For the proof, a stateful writer stands in for it (same
methodology as the benchmarks: ~100 MiB heap, line per second):

```bash
mkdir -p ~/ci-demo
cat > ~/ci-demo/writer.py <<'PY'
import time
arena = bytearray(100 * 1024 * 1024)
i = 0
while True:
    i += 1
    arena[i % len(arena)] = i % 251
    print(f"progress {i}", flush=True)
    time.sleep(1)
PY
shiftgate workload create runner --path ~/ci-demo --start -- python3 ~/ci-demo/writer.py
```

Only the approved root (`~/ci-demo`) travels with the checkpoint. Files a
workload holds open outside its root are resolved, counted, and reported —
they do not silently travel (see [Limitations](limitations.md)).

### 3. Checkpoint the warm state once

```bash
shiftgate checkpoint create runner --leave-running
shiftgate checkpoint list --workload runner
```

Measured on the bench box: **959–995 ms** for a full checkpoint of a running
(`--leave-running`) workload; the stopped-workload figure is 914–973 ms
([docs/benchmarks.md](benchmarks.md)). The process resumes automatically
(`--leave-running` is the default). The checkpoint is encrypted,
content-addressed, and immutable. The demo script reads the checkpoint id
from `shiftgate checkpoint list --workload runner --json`.

### 4. Clone it into N workers

```bash
shiftgate clone CHECKPOINT_ID --count 10 --prefix ci
shiftgate clone list
```

Ten independent running workloads from one warm state — each with its own root
and identity, no re-warm. This is the burst: worker 1..N are ready in the time
one cold boot takes to POST. Same machine (see limits above).

### 5. Fail a move on purpose — the source survives

```bash
shiftgate migrate runner --to https://127.0.0.1:1
```

The destination is unreachable; the state machine refuses to commit, the move
rolls back, and the source workload keeps running. No state is destroyed at
any stage — that is the tested invariant ("a failed move never destroys the
source"; e2e `TestE2EMigrationUnreachableDestination` fails the move this way
on purpose). The demo script asserts the workload is still registered and
still *running* after this step (not merely present).

This beat runs before the real move so the invariant it proves is the one it
claims: the workload it checks is the one that was just migrated-from, not
one that a completed move already relocated.

### 6. Move the workload to a second machine

Second machine runs its own install and passes `shiftgate doctor`. Give it an
alias (or pass the peer URL directly):

```bash
shiftgate connect ci-2 --to https://second-host:8443 --machine-id MACHINE_ID
shiftgate migrate runner --to ci-2 --preflight        # dry run: admitted or not, nothing moves
shiftgate migrate runner --to ci-2                    # cold move (free tier path)
# shiftgate migrate runner --to ci-2 --mode live --pre-copy-passes 4   # paid plans: freeze only for the final delta
shiftgate status MIGRATION_ID
```

`connect` needs both the peer URL and the destination's machine identity
(`shiftgate machine` prints it on the destination); the demo script takes
`DEST_URL` and `DEST_MACHINE_ID` for the two.

The move is a persisted state machine — CREATED → DISCOVER → VALIDATE →
SNAPSHOT → PREPARE → TRANSFER → VERIFY → RESTORE → POST_VALIDATE → SWITCH →
COMMIT — and every transition is persisted before the next stage begins. The
`downtime` value (freeze instant → health-validated restore) is reported in
the migration record; `shiftgate migrate --json` prints that record (the
script feeds its `id` to `shiftgate status MIGRATION_ID`).

## Run it unattended

```bash
CLONE_COUNT=10 ./scripts/demo-ci-burst.sh
```

The script prints a metrics table (per-step duration in ms) and exits 0 when:
N workers are running from one checkpoint (it counts them and fails if the
count is wrong), the forced-failure move rolled back with the source alive
and running, and — when a second machine was configured — the cross-machine
move completed. Without a second machine the move is skipped with a notice.

`DEST_URL=https://host:8443` adds the cross-machine move, and
`DEST_MACHINE_ID` names the destination's identity (from `shiftgate machine`
on that host) — both are required together, since `shiftgate connect` will
not guess which machine it is talking to:

```bash
DEST_URL=https://second-host:8443 DEST_MACHINE_ID=MACHINE_ID CLONE_COUNT=10 ./scripts/demo-ci-burst.sh
```

## What the numbers say (and do not say)

| Measure | Bench box | Source |
|---|---|---|
| Full checkpoint (running workload, `--leave-running`) | 959 – 995 ms | [benchmarks.md](benchmarks.md) |
| Full checkpoint (stopped workload) | 914 – 973 ms | [benchmarks.md](benchmarks.md) |
| Restore wall (incl. resume + health validation) | 2.27 – 2.77 s | [benchmarks.md](benchmarks.md) |
| Live move downtime (~17 MB state, 4 pre-copy passes) | 3.20 – 3.29 s | [benchmarks.md](benchmarks.md) |
| Cold move downtime (~17 MB state) | 3.75 – 3.91 s | [benchmarks.md](benchmarks.md) |
| Transfer inside the freeze (live) | 107 – 116 ms | [benchmarks.md](benchmarks.md) |
| Transfer (cold, workload stopped for the whole move) | 215 – 223 ms | [benchmarks.md](benchmarks.md) |

These are single-box loopback numbers from the documented harness — rerun the
commands in [docs/benchmarks.md](benchmarks.md) on your own hardware before
you plan around them. The migration state machine does not promise a constant;
it promises to report what happened and to never commit a move the destination
did not survive.

## Next

- [Quickstart](quickstart.md) — policies, lazy restore, standby/failover.
- [CLI reference](cli.md) — every command and flag used here.
- [The maintenance window playbook](https://shiftgate.dev/guides/linux-maintenance-window-without-downtime-cost)
  and [clone-into-N-workers](https://shiftgate.dev/guides/clone-checkpoint-into-n-workers)
  guides — the two workflows this proof is built from.
- Something break? Open an issue with `shiftgate doctor --json` output — the
  proof kit is meant to be run by skeptics.
