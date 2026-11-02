#!/bin/sh
# weekly-score.sh — the weekly scoreboard readout (master BR-05). Prints the
# authenticated counters plus the §7 gate table columns for the current week.
# Privacy-minimal: counts only.
#
# Usage:
#   SHIFT_CONTROL_URL=https://... SHIFT_TOKEN=$(cat ~/.shift/token) \
#     sh scripts/weekly-score.sh [--org ORG_ID]
#
# Falls back to `psql "$SHIFT_DATABASE_URL" -f scripts/kpi.sql` when no
# control-plane session is available.
set -eu

CONTROL_URL="${SHIFT_CONTROL_URL:-https://api.shiftgate.dev}"
TOKEN="${SHIFT_TOKEN:-}"
if [ -n "${SHIFT_TOKEN_FILE:-}" ] && [ -z "${TOKEN}" ]; then
  TOKEN="$(cat "${SHIFT_TOKEN_FILE}")"
fi

if [ -n "${TOKEN}" ]; then
  echo "=== Weekly scoreboard (${CONTROL_URL})"
  curl -fsS -H "Authorization: Bearer ${TOKEN}" "${CONTROL_URL}/v1/admin/scoreboard"
  echo
else
  echo "=== No SHIFT_TOKEN; falling back to kpi.sql"
  if [ -z "${SHIFT_DATABASE_URL:-}" ]; then
    echo "error: set SHIFT_TOKEN (control plane) or SHIFT_DATABASE_URL (psql fallback)" >&2
    exit 1
  fi
  psql "${SHIFT_DATABASE_URL}" -f "$(dirname "$0")/kpi.sql"
fi

echo "=== Public numbers (what the numbers page ships)"
curl -fsS "${CONTROL_URL}/v1/public/stats"
echo
echo "Gate table checks: assert KPI-10 (entitlement drift) and KPI-12 (webhook backlog) are clean; see plan §4 triggers."
