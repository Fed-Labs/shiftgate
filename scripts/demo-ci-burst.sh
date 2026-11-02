#!/usr/bin/env bash
# CI-burst proof kit — one prepared workload, N warmed workers, one move, one
# proven rollback (docs/ci-burst.md). Plan 5 BR-01.
#
# Target (fresh Ubuntu 24.04): N=10 workers in <= 10 minutes wall, zero manual
# steps, metrics table printed, exit 0. Every step prints its measured duration.
#
#   CLONE_COUNT=10 ./scripts/demo-ci-burst.sh
#   DEST_URL=https://second-host:8443 DEST_MACHINE_ID=machine-ci-2 ./scripts/demo-ci-burst.sh
#
# The path below is the path docs/ci-burst.md documents, step for step:
# doctor, workload, checkpoint, clone, a forced-failure move the source must
# survive, and — with DEST_URL + DEST_MACHINE_ID — the cross-machine move
# (preflight, migrate, status). Machine-readable variants (--json) run beside
# the documented commands only to read ids and counts for the assertions.
#
# Honest limits (docs/ci-burst.md): Linux x86_64 + privileged agent + CRIU
# kernel; clone is same-machine; a live move is a measured freeze reported as
# `downtime`, not zero downtime; --mode live is a paid plan feature on every
# path (free tier is cold-only); a failed move never destroys the source.

set -euo pipefail

WORKLOAD="${WORKLOAD:-runner}"
ROOT="${ROOT:-$HOME/ci-demo}"
CLONE_COUNT="${CLONE_COUNT:-10}"
DEST_URL="${DEST_URL:-}"
DEST_MACHINE_ID="${DEST_MACHINE_ID:-}"
PREFIX="${PREFIX:-ci}"
STARTED_AT=$(date +%s)

METRICS_FILE="$(mktemp)"
trap 'rm -f "$METRICS_FILE"' EXIT

record() {
  printf '%s\t%s\n' "$1" "$2" >> "$METRICS_FILE"
  printf -- '--- %s: %s ms\n' "$1" "$2"
}

# step TIMES a documented command, printing its own measured duration.
step() {
  local name="$1"
  shift
  local t0 t1
  t0=$(date +%s%3N)
  "$@"
  t1=$(date +%s%3N)
  record "$name" "$((t1 - t0))"
}

# step_capture TIMES a command and captures its stdout (machine-readable
# variants of the documented commands); the duration line goes to stderr so
# the captured stdout stays parseable.
step_capture() {
  local name="$1"
  shift
  local t0 t1 out
  t0=$(date +%s%3N)
  out="$("$@")"
  t1=$(date +%s%3N)
  printf '%s\t%s\n' "$name" "$((t1 - t0))" >> "$METRICS_FILE"
  printf -- '--- %s: %s ms\n' "$name" "$((t1 - t0))" >&2
  printf '%s' "$out"
}

fail() { echo "FAIL: $*" >&2; exit 1; }

command -v python3 >/dev/null 2>&1 || fail "python3 is required (the demo workload and the JSON assertions)"
command -v shiftgate >/dev/null 2>&1 || fail "shiftgate is not on PATH (run the installer first, docs/ci-burst.md step 0)"

if [ -n "$DEST_URL" ] && [ -z "$DEST_MACHINE_ID" ]; then
  fail "DEST_URL is set but DEST_MACHINE_ID is empty — 'shiftgate connect' requires --machine-id: run 'shiftgate machine' on the destination and copy its identity"
fi

echo "== CI-burst proof: workload=$WORKLOAD workers=$CLONE_COUNT =="

step doctor shiftgate doctor

mkdir -p "$ROOT"
cat > "$ROOT/writer.py" <<'PY'
import time
arena = bytearray(100 * 1024 * 1024)  # ~100 MiB heap, docs/benchmarks.md method
i = 0
while True:
    i += 1
    arena[i % len(arena)] = i % 251
    print(f"progress {i}", flush=True)
    time.sleep(1)
PY

step workload-create shiftgate workload create "$WORKLOAD" --path "$ROOT" --start -- python3 "$ROOT/writer.py"

step checkpoint shiftgate checkpoint create "$WORKLOAD" --leave-running

step checkpoint-list shiftgate checkpoint list --workload "$WORKLOAD"

CKPT_JSON=$(step_capture checkpoint-list-json shiftgate checkpoint list --workload "$WORKLOAD" --json)
CKPT=$(printf '%s' "$CKPT_JSON" | python3 -c 'import json,sys
data = json.load(sys.stdin)
if not data:
    sys.exit("checkpoint list is empty")
print(max(data, key=lambda c: c["created_at"])["id"])') \
  || fail "could not read the checkpoint id from 'shiftgate checkpoint list --json'"
[ -n "$CKPT" ] || fail "could not read the checkpoint id from 'shiftgate checkpoint list --json'"
echo "checkpoint: $CKPT"

step clone shiftgate clone "$CKPT" --count "$CLONE_COUNT" --prefix "$PREFIX"

step clone-list shiftgate clone list

CLONE_LIST_JSON=$(step_capture clone-list-json shiftgate clone list --json)
RUNNING=$(printf '%s' "$CLONE_LIST_JSON" | python3 -c 'import json,sys
prefix = sys.argv[1]
sets = [r for r in json.load(sys.stdin) if r.get("name_prefix") == prefix]
if not sets:
    sys.exit("no clone set with prefix %r" % prefix)
latest = max(sets, key=lambda r: r["created_at"])
running = sum(1 for m in latest.get("members", []) if m.get("pid"))
print(running)' "$PREFIX")
echo "clone members running: $RUNNING (expected prefix '$PREFIX' x $CLONE_COUNT)"
[ "$RUNNING" -eq "$CLONE_COUNT" ] \
  || fail "expected $CLONE_COUNT running clone members, found $RUNNING — the burst is incomplete"

echo "== rollback beat: a move that must fail, a source that must survive =="
t0=$(date +%s%3N)
if shiftgate migrate "$WORKLOAD" --to https://127.0.0.1:1; then
  fail "the forced-failure migration succeeded — the demo assumptions are wrong"
fi
t1=$(date +%s%3N)
record "forced-fail-migrate" "$((t1 - t0))"
echo "the migration state machine refused to commit; rollback reported."
shiftgate workload inspect "$WORKLOAD" >/dev/null || fail "source workload is gone after the failed move — invariant violated"
WSTATUS=$(shiftgate workload inspect "$WORKLOAD" --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
[ "$WSTATUS" = "running" ] || fail "source workload is not running after the failed move (status: $WSTATUS)"
echo "source workload still registered and running (status: $WSTATUS)."

MOVED=skipped
if [ -n "$DEST_URL" ]; then
  step connect shiftgate connect ci-2 --to "$DEST_URL" --machine-id "$DEST_MACHINE_ID"
  step preflight shiftgate migrate "$WORKLOAD" --to ci-2 --preflight
  MIG_JSON=$(step_capture migrate shiftgate migrate "$WORKLOAD" --to ci-2 --json)
  MIGRATION_ID=$(printf '%s' "$MIG_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  [ -n "$MIGRATION_ID" ] || fail "could not read the migration id"
  step status shiftgate status "$MIGRATION_ID"
  MOVED=done
else
  echo "note: DEST_URL not set — cross-machine move skipped (the rollback beat above still ran)"
fi

END_TO_END=$(( $(date +%s) - STARTED_AT ))
{
  echo
  echo "== metrics (per-step ms; wall ${END_TO_END}s) =="
  printf '%-22s %s\n' "STEP" "MS"
  while IFS=$'\t' read -r name ms; do printf '%-22s %s\n' "$name" "$ms"; done < "$METRICS_FILE"
  printf '%-22s %s\n' "clone-members" "$RUNNING"
  printf '%-22s %s\n' "cross-machine" "$MOVED"
  printf '%-22s %s\n' "forced-fail" "rolled-back"
  printf '%-22s %s\n' "source-alive" "yes"
}

if [ "$END_TO_END" -gt 600 ]; then
  echo "note: wall ${END_TO_END}s exceeded the 10-minute target — see docs/ci-burst.md" >&2
fi
echo "PASS: CI-burst proof complete (docs/ci-burst.md)."
