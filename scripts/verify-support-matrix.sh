#!/usr/bin/env bash
set -euo pipefail

# Verify the local machine against the release support matrix and enforce the
# matrix's own policy block before certifying a line or enabling automatic
# operations.
#
# Documented probe exit codes (cmd/shift-verify/main.go) are the enforcement
# surface: 0 certified (or explicitly accepted experimental), 2 usage error or
# unreadable matrix, 3 not certified, 4 unsupported, 1 probe crash. `go run`
# collapses every non-zero program exit to 1 and destroys that contract, so
# this script builds the probe binary and runs it directly, propagating the
# real exit status.

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

matrix=${1:-docs/support-matrix.json}
[[ -f "$matrix" ]] || { echo "support matrix not found: $matrix" >&2; exit 2; }
command -v go >/dev/null || { echo "go is required for the evidence probe" >&2; exit 2; }

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

if ! go build -o "$workdir/shift-verify" ./cmd/shift-verify; then
  echo "failed to build the evidence probe (cmd/shift-verify)" >&2
  exit 2
fi

evidence="$workdir/support-evidence.json"
set +e
"$workdir/shift-verify" support-probe --matrix "$matrix" --json | tee "$evidence"
status=${PIPESTATUS[0]}
set -e

case "$status" in
  0|2|3|4) ;;
  *)
    echo "evidence probe crashed (exit $status)" >&2
    exit 1
    ;;
esac

# A run that produced no probe evidence proved nothing: never let an empty
# run masquerade as a verdict (the exit code is only meaningful if the probe
# actually executed against this machine).
if [[ "$status" -ne 2 ]] && ! grep -q '"platform_status"' "$evidence"; then
  echo "evidence probe produced no platform evidence; refusing to report a verdict" >&2
  exit 2
fi

exit "$status"
