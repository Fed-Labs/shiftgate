#!/usr/bin/env bash
set -euo pipefail

: "${SHIFT_CONTROL_URL:?set SHIFT_CONTROL_URL}"
: "${SHIFT_CONTROL_TOKEN:?set SHIFT_CONTROL_TOKEN}"
: "${SHIFT_ORGANIZATION_ID:?set SHIFT_ORGANIZATION_ID}"

# The org bearer token must never travel in cleartext — same floor as the Go
# client (SHIFT_CONTROL_URL must use https://).
if [[ "$SHIFT_CONTROL_URL" != https://* ]]; then
  echo "shift-slurm: SHIFT_CONTROL_URL must use https://" >&2
  exit 2
fi

operation=${1:?operation: checkpoint|migrate|cancel}
job_id=${2:?Slurm job id}
workload_id=${3:-}
base="${SHIFT_CONTROL_URL%/}/v1/organizations/${SHIFT_ORGANIZATION_ID}"
headers=(-H "Authorization: Bearer ${SHIFT_CONTROL_TOKEN}" -H 'Content-Type: application/json')
state_dir=${SHIFT_SLURM_STATE_DIR:-/tmp/shift-slurm}
# The control plane grants a checkpoint command at most ten minutes for the
# agent's synchronous CRIU dump; ask for the full budget and outwait it on
# the client side. A 30-second default aborts every real workload's dump.
checkpoint_timeout_seconds=${SHIFT_CHECKPOINT_TIMEOUT_SECONDS:-600}

json_string() {
  local value=$1
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//$'\n'/\\n}
  value=${value//$'\r'/\\r}
  value=${value//$'\t'/\\t}
  printf '%s' "$value"
}

require_workload() {
  [[ -n "$workload_id" ]] || { echo "shift-slurm: workload id is required" >&2; exit 2; }
}

# Job ids appear in state file names; keep them inside the state directory.
safe_job_id=${job_id//\//_}
state_file="$state_dir/${safe_job_id}.env"

case "$operation" in
  checkpoint)
    machine_id=${4:?source machine id}
    require_workload
    body=$(printf '{"action":"checkpoint","workload_id":"%s","timeout_seconds":%d}' \
      "$(json_string "$workload_id")" "$checkpoint_timeout_seconds")
    if ! response=$(curl --fail-with-body --silent --show-error \
        --max-time "$((checkpoint_timeout_seconds + 60))" "${headers[@]}" \
        -X POST "${base}/machines/${machine_id}/commands" \
        -H "Idempotency-Key: shift-slurm-checkpoint-${job_id}" -d "$body"); then
      echo "shift-slurm: checkpoint of workload ${workload_id} on ${machine_id} failed (no warm state recorded): ${response}" >&2
      exit 1
    fi
    printf '%s\n' "$response"
    ;;
  migrate)
    source_machine=${4:?source machine id}
    destination_machine=${5:?destination machine id}
    mode=${6:-cold}
    require_workload
    body=$(printf '{"workload_id":"%s","source_machine_id":"%s","destination_machine_id":"%s","mode":"%s"}' \
      "$(json_string "$workload_id")" "$(json_string "$source_machine")" \
      "$(json_string "$destination_machine")" "$(json_string "$mode")")
    if ! response=$(curl --fail-with-body --silent --show-error "${headers[@]}" \
        -X POST "${base}/migrations" \
        -H "Idempotency-Key: shift-slurm-migrate-${job_id}" -d "$body"); then
      echo "shift-slurm: migrate for job ${job_id} failed: ${response}" >&2
      exit 1
    fi
    printf '%s\n' "$response"
    migration_id=$(printf '%s' "$response" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
    if [[ -n "$migration_id" ]]; then
      mkdir -p "$state_dir"
      printf 'job_id=%s\nworkload_id=%s\nmigration_id=%s\n' \
        "$job_id" "$workload_id" "$migration_id" \
        >"$state_file"
    fi
    ;;
  cancel)
    # Job-end cleanup (Slurm epilog / kill-wait): cancel the job's in-flight
    # migration so the workload is not locked by a leaked active migration and
    # the destination machine's capacity is released. When no migration id is
    # passed, the one recorded by `migrate` for this job is used.
    migration_id=${4:-}
    if [[ -z "$migration_id" && -f "$state_file" ]]; then
      migration_id=$(sed -n 's/^migration_id=//p' "$state_file" | head -n1)
    fi
    if [[ -z "$migration_id" ]]; then
      echo "shift-slurm: no in-flight migration recorded for job ${job_id}; nothing to clean up"
      exit 0
    fi
    status_code=$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
      --max-time 60 "${headers[@]}" \
      -X POST "${base}/migrations/${migration_id}/cancel" \
      -H "Idempotency-Key: shift-slurm-cancel-${job_id}" -d '{}' || true)
    case "$status_code" in
      2*|404|409)
        # 404/409 mean the migration already settled — cleanup is complete.
        rm -f "$state_file"
        echo "shift-slurm: migration ${migration_id} for job ${job_id} is settled (HTTP ${status_code:-000})"
        ;;
      *)
        echo "shift-slurm: cancel of migration ${migration_id} for job ${job_id} failed (HTTP ${status_code:-000}); keeping state for a retry" >&2
        exit 1
        ;;
    esac
    ;;
  *) echo "unknown operation: $operation" >&2; exit 2 ;;
esac
