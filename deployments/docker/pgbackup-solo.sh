#!/bin/sh
# pgbackup-solo.sh — nightly pg_dump + optional rclone upload (plan 4 BR-14).
# Runs inside the pgbackup sidecar. Dump lands in /backups; when an rclone
# remote is configured (RCLONE_REMOTE set) the artifact is uploaded too. A
# failed dump or upload exits non-zero so the orchestrator/gotify alert fires;
# the artifact stays in the volume either way.
set -eu

RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-14}"
TARGET="${POSTGRES_USER:-shift}@postgres:5432/${POSTGRES_DB:-shift}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DUMP="/backups/shift-${STAMP}.sql.gz"

echo "pgbackup: dumping ${TARGET} -> ${DUMP}"
# POSIX sh has no pipefail: `pg_dump | gzip > file` reports success on a
# partial dump (gzip exits 0), and `test -s` passes on any non-empty
# fragment — a truncated backup would then be uploaded as good. Dump to a
# temp file first so pg_dump's exit status is observed directly, gzip only
# after success, and never leave a partial artifact under the final name.
DUMP_TMP="${DUMP}.tmp"
pg_dump -h postgres -U "${POSTGRES_USER:-shift}" "${POSTGRES_DB:-shift}" -f "${DUMP_TMP}" || {
  rm -f "${DUMP_TMP}"
  echo "pgbackup: pg_dump failed; no artifact written" >&2
  exit 1
}
test -s "${DUMP_TMP}" || {
  rm -f "${DUMP_TMP}"
  echo "pgbackup: pg_dump produced an empty dump; refusing to call it a backup" >&2
  exit 1
}
gzip -f "${DUMP_TMP}" || {
  rm -f "${DUMP_TMP}" "${DUMP_TMP}.gz"
  echo "pgbackup: compression failed; no artifact written" >&2
  exit 1
}
mv "${DUMP_TMP}.gz" "${DUMP}"
echo "pgbackup: dump ok ($(wc -c < "${DUMP}") bytes)"

if [ -n "${RCLONE_REMOTE:-}" ]; then
  echo "pgbackup: uploading to ${RCLONE_REMOTE}"
  rclone copy "${DUMP}" "${RCLONE_REMOTE}" --s3-no-check-bucket
  echo "pgbackup: upload ok"
else
  if [ "${BACKUP_REMOTE_REQUIRED:-true}" = "true" ]; then
    echo "pgbackup: RCLONE_REMOTE is required for this production schedule; refusing local-only backup" >&2
    exit 1
  fi
  echo "pgbackup: RCLONE_REMOTE not set; dump kept local only (developer mode)"
fi

find /backups -name 'shift-*.sql.gz' -mtime "+${RETENTION_DAYS}" -delete

echo "pgbackup: next run in 24h"
sleep 24h
exec "$0" "$@"
