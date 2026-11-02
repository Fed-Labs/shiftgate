#!/usr/bin/env bash
set -euo pipefail

# Backup & restore drill. Archives agent state and the control-plane database,
# restores both into explicitly separate targets, and verifies the restored
# artifacts end to end. The source agent directory and source database are
# read-only inputs to this drill.
#
# Key boundary: the state archive deliberately EXCLUDES keys/. Master keys and
# workload keys are exported into a separate keys archive (--keys-output) that
# must be escrowed apart from the state archive — see docs/backup-drill.md.
# Bundling keys into the state archive makes it self-decrypting and is refused
# unless the explicit unsafe --bundle-keys flag is passed.
#
# The drill fails on empty, truncated, or corrupt inputs: it asserts the source
# state contains a keyring, a checkpoint index, and real checkpoint/chunk
# content, extracts the archive for real, requires the restored inventory to
# match the source exactly, and runs shift-verify state (authenticated
# decryption of every manifest and asset chunk) over the restored tree.

state_dir=''
database_url=''
output=''
keys_output=''
restore_state_dir=''
restore_database_url=''
expect_checkpoints=''
bundle_keys=0
allow_plaintext_keys=0
# The encrypted-state verifier ships as bin/shift-verify (`make build`);
# fall back to a PATH lookup for installs that put it on PATH.
verify_bin=${SHIFT_VERIFY_BIN:-}
if [[ -z "$verify_bin" ]]; then
  repo_bin="$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd)/bin/shift-verify"
  if [[ -x "$repo_bin" ]]; then
    verify_bin=$repo_bin
  else
    verify_bin=shift-verify
  fi
fi
usage() {
  cat >&2 <<'EOF'
usage: backup-verify.sh --state-dir DIR --database-url URL --output DIR
       --keys-output DIR --restore-state-dir DIR --restore-database-url URL
       [--expect-checkpoints N] [--bundle-keys] [--allow-plaintext-keys]

  --state-dir              agent state directory (read-only input)
  --database-url           control-plane database to dump (read-only input)
  --output                 directory for shift-state.tar.gz and shift.dump
  --keys-output            directory for the separate keys escrow archive
                           (shift-keys.tar.gz); required unless --bundle-keys,
                           and must never live inside --output or --state-dir
  --restore-state-dir      empty/absent directory to extract the archive into
  --restore-database-url   empty database to restore the dump into
  --expect-checkpoints N   fail unless the source state holds exactly N
                           checkpoint manifests
  --bundle-keys            UNSAFE: bundle keys/ inside the state archive
                           (self-decrypting artifact; lab use only)
  --allow-plaintext-keys   UNSAFE: run even when the master keyring stores raw
                           unprotected master keys

Environment: SHIFT_VERIFY_BIN, SHIFT_MASTER_KEY_FILE (keyfile for a sealed
keyring; escrowed separately, never inside any archive),
SHIFT_BACKUP_TEXTFILE_DIR (node_exporter textfile collector directory; on
completion the drill writes shift_backup_* series for the monitoring bundle).
EOF
  exit 2
}
die() { echo "$*" >&2; exit 2; }
fail() { echo "$*" >&2; exit 1; }
while [[ $# -gt 0 ]]; do
  case "$1" in
    --state-dir) state_dir=${2:-}; shift 2 ;;
    --database-url) database_url=${2:-}; shift 2 ;;
    --output) output=${2:-}; shift 2 ;;
    --keys-output) keys_output=${2:-}; shift 2 ;;
    --restore-state-dir) restore_state_dir=${2:-}; shift 2 ;;
    --restore-database-url) restore_database_url=${2:-}; shift 2 ;;
    --expect-checkpoints) expect_checkpoints=${2:-}; shift 2 ;;
    --bundle-keys) bundle_keys=1; shift ;;
    --allow-plaintext-keys) allow_plaintext_keys=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done
[[ -d "$state_dir" ]] || die '--state-dir must be an existing directory'
[[ -n "$database_url" && -n "$output" && -n "$restore_state_dir" && -n "$restore_database_url" ]] ||
  die '--database-url, --output, --restore-state-dir, and --restore-database-url are required'
if [[ $bundle_keys -eq 1 ]]; then
  [[ -z "$keys_output" ]] || die '--keys-output cannot be combined with --bundle-keys'
else
  [[ -n "$keys_output" ]] || die '--keys-output is required (the keyring must never ship inside the state archive); pass the unsafe --bundle-keys only for throwaway labs'
fi
if [[ -n "$expect_checkpoints" && ! "$expect_checkpoints" =~ ^[1-9][0-9]*$ ]]; then
  die '--expect-checkpoints must be a positive integer'
fi

path_inside() {
  local child parent
  child=$(realpath -m "$1")
  parent=$(realpath -m "$2")
  [[ "$child" == "$parent" || "$child" == "$parent"/* ]]
}
no_overlap() {
  if path_inside "$1" "$2" || path_inside "$2" "$1"; then
    die "$1 and $2 must not overlap (neither may live inside the other)"
  fi
}
no_overlap "$state_dir" "$output"
no_overlap "$state_dir" "$restore_state_dir"
no_overlap "$output" "$restore_state_dir"
if [[ -n "$keys_output" ]]; then
  no_overlap "$keys_output" "$state_dir"
  no_overlap "$keys_output" "$output"
  no_overlap "$keys_output" "$restore_state_dir"
fi

for tool in pg_dump pg_restore psql tar realpath find grep; do command -v "$tool" >/dev/null || die "$tool is required"; done
if [[ -e "$restore_state_dir" ]] && [[ -n "$(find "$restore_state_dir" -mindepth 1 -print -quit 2>/dev/null)" ]]; then
  die 'restore state directory must be absent or empty'
fi
if ! psql "$restore_database_url" -Atqc "SELECT 1" >/dev/null 2>&1; then
  die 'restore database is unreachable; refusing to create or mutate an unknown database'
fi
if [[ "$(psql "$restore_database_url" -Atqc "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind IN ('r','p','v','m') AND n.nspname NOT LIKE 'pg_%' AND n.nspname <> 'information_schema'")" != "0" ]]; then
  die 'restore database must be empty; refusing to overwrite existing tables'
fi

count_manifests() {
  { find "$1/checkpoints" -mindepth 2 -maxdepth 2 -type f -name 'manifest.enc.json' 2>/dev/null || true; } | wc -l
}
count_chunks() {
  { find "$1/objects" -type f -name '*.chunk' 2>/dev/null || true; } | wc -l
}

# Refuse empty or wrong inputs before any work happens: a drill pointed at the
# wrong --state-dir, or at a state tree that lost its checkpoints, must never
# verify green.
keyring="$state_dir/keys/master-keyring.json"
[[ -f "$keyring" ]] || fail "source state has no keys/master-keyring.json; refusing to drill an empty or wrong --state-dir"
[[ -f "$state_dir/checkpoints/index.enc.json" ]] || fail "source state has no checkpoints/index.enc.json; refusing to certify an empty backup"
source_manifests=$(count_manifests "$state_dir")
source_chunks=$(count_chunks "$state_dir")
[[ "$source_manifests" -ge 1 ]] || fail "source state contains no checkpoint manifests; refusing to certify an empty backup"
[[ "$source_chunks" -ge 1 ]] || fail "source state contains no chunk objects; refusing to certify an empty backup"
if [[ -n "$expect_checkpoints" && "$source_manifests" -ne "$expect_checkpoints" ]]; then
  fail "source state holds $source_manifests checkpoint manifests, expected $expect_checkpoints"
fi

# Key-boundary guard: a keyring whose master keys sit unprotected in
# keys/master-keyring.json makes every artifact that carries it self-
# decrypting. Require the explicit unsafe override before going anywhere.
keyring_records=$(grep -c '"key"' "$keyring" || true)
keyring_sealed=$(grep -c '"protection"' "$keyring" || true)
[[ "$keyring_records" -ge 1 ]] || fail "keys/master-keyring.json holds no master key records; corrupted keyring"
if [[ "$keyring_sealed" -gt 0 && "$keyring_sealed" -lt "$keyring_records" ]]; then
  fail "master keyring mixes sealed and unprotected records; rotate to a fully sealed keyring before backing up"
fi
if [[ "$keyring_sealed" -eq 0 ]]; then
  [[ "$allow_plaintext_keys" -eq 1 ]] ||
    fail "refusing to run: keys/master-keyring.json stores master keys unprotected (plaintext base64). Configure SHIFT_MASTER_KEY_FILE so the keyring is sealed under an external keyfile, or pass the unsafe --allow-plaintext-keys for a throwaway lab."
  echo 'WARNING: --allow-plaintext-keys: the master keyring stores raw unprotected master keys; anything carrying it decrypts every checkpoint.' >&2
else
  [[ -r "${SHIFT_MASTER_KEY_FILE:-}" ]] ||
    fail "master keyring is sealed under an external key file; export SHIFT_MASTER_KEY_FILE to a readable keyfile (the keyfile is escrowed separately and is inside no archive)"
fi

write_metrics() {
  local status=$1
  set +e
  [[ "${drill_started:-0}" == 1 ]] || return 0
  local dir=${SHIFT_BACKUP_TEXTFILE_DIR:-}
  [[ -n "$dir" && -d "$dir" ]] || return 0
  local now file tmp
  now=$(date +%s)
  file="$dir/shift-backup-drill.prom"
  tmp="$file.$$"
  {
    echo '# HELP shift_backup_last_run_timestamp_seconds Unix timestamp of the most recent scripts/backup-verify.sh run.'
    echo '# TYPE shift_backup_last_run_timestamp_seconds gauge'
    echo "shift_backup_last_run_timestamp_seconds $now"
    echo '# HELP shift_backup_last_run_success 1 when the most recent scripts/backup-verify.sh run completed, 0 when it failed.'
    echo '# TYPE shift_backup_last_run_success gauge'
    if [[ "$status" -eq 0 ]]; then
      echo 'shift_backup_last_run_success 1'
      echo '# HELP shift_backup_last_success_timestamp_seconds Unix timestamp of the most recent successful scripts/backup-verify.sh run.'
      echo '# TYPE shift_backup_last_success_timestamp_seconds gauge'
      echo "shift_backup_last_success_timestamp_seconds $now"
    else
      echo 'shift_backup_last_run_success 0'
    fi
  } >"$tmp" && mv -f "$tmp" "$file" || rm -f "$tmp"
  return 0
}
listing=''
keys_listing=''
cleanup() {
  local status=$?
  rm -f "$listing" "$keys_listing"
  write_metrics "$status"
}
drill_started=1
trap cleanup EXIT

umask 077
mkdir -p "$output" "$restore_state_dir"
[[ -z "$keys_output" ]] || mkdir -p "$keys_output"

if [[ "$bundle_keys" -eq 1 ]]; then
  echo 'WARNING: --bundle-keys: keys/ will ship INSIDE shift-state.tar.gz next to the ciphertext; this archive is self-decrypting. Never store it as your production backup.' >&2
  tar --xattrs --acls --numeric-owner -C "$state_dir" -czf "$output/shift-state.tar.gz" .
else
  tar --xattrs --acls --numeric-owner --exclude=./keys -C "$state_dir" -czf "$output/shift-state.tar.gz" .
  tar --xattrs --acls --numeric-owner -C "$state_dir" -czf "$keys_output/shift-keys.tar.gz" keys
  echo "KEYS EXPORTED SEPARATELY: $keys_output/shift-keys.tar.gz holds the master keyring and workload keys." >&2
  echo 'ESCROW IT APART FROM THE STATE ARCHIVE (separate location/access control). A backup that carries both archives is self-decrypting. See docs/backup-drill.md.' >&2
fi
pg_dump --format=custom --no-owner --file="$output/shift.dump" "$database_url"

# Real end-to-end archive verification: listing catches truncated or corrupt
# archives, member assertions catch archives that lost their content, and the
# keys assertion proves the keyring did not ride along in the state tar.
listing=$(mktemp)
keys_listing=$(mktemp)
tar -tzf "$output/shift-state.tar.gz" >"$listing" || fail 'state archive is truncated or corrupt (tar listing failed)'
[[ -s "$listing" ]] || fail 'state archive lists no members'
grep -q 'checkpoints/index\.enc\.json$' "$listing" || fail 'state archive does not contain checkpoints/index.enc.json'
listed_manifests=$(grep -c 'manifest\.enc\.json$' "$listing" || true)
listed_chunks=$(grep -c '\.chunk$' "$listing" || true)
[[ "$listed_manifests" -ge 1 ]] || fail 'state archive contains no checkpoint manifests'
[[ "$listed_chunks" -ge 1 ]] || fail 'state archive contains no chunk objects'
if [[ "$bundle_keys" -eq 1 ]]; then
  grep -q 'keys/master-keyring\.json$' "$listing" || fail 'state archive is missing keys/master-keyring.json although keys were bundled'
else
  if grep -Eq '(^|/)keys/' "$listing"; then
    fail 'state archive contains keys/ although keys were not bundled; refusing a self-decrypting artifact'
  fi
  tar -tzf "$keys_output/shift-keys.tar.gz" >"$keys_listing" || fail 'keys escrow archive is truncated or corrupt (tar listing failed)'
  grep -q 'keys/master-keyring\.json$' "$keys_listing" || fail 'keys escrow archive does not contain keys/master-keyring.json'
fi
rm -f "$listing" "$keys_listing"
listing=''
keys_listing=''

tar -xzf "$output/shift-state.tar.gz" -C "$restore_state_dir" || fail 'state archive extraction failed (truncated or corrupt archive)'
if [[ "$bundle_keys" -eq 0 ]]; then
  tar -xzf "$keys_output/shift-keys.tar.gz" -C "$restore_state_dir" || fail 'keys escrow archive extraction failed'
fi
pg_restore --exit-on-error --no-owner --dbname="$restore_database_url" "$output/shift.dump"

# Restored inventory must match the source exactly, and the keyring must be
# present before the verifier runs: shift-verify state would otherwise mint a
# fresh keyring inside the restore target and pass on an empty tree.
[[ -f "$restore_state_dir/keys/master-keyring.json" ]] ||
  fail 'restored state is missing keys/master-keyring.json (the keyring must come from the keys escrow archive); refusing to let verification mint a fresh one'
[[ -f "$restore_state_dir/checkpoints/index.enc.json" ]] ||
  fail 'restored state is missing checkpoints/index.enc.json'
restored_manifests=$(count_manifests "$restore_state_dir")
restored_chunks=$(count_chunks "$restore_state_dir")
[[ "$restored_manifests" -eq "$source_manifests" ]] ||
  fail "restored state holds $restored_manifests checkpoint manifests, source holds $source_manifests"
[[ "$restored_chunks" -eq "$source_chunks" ]] ||
  fail "restored state holds $restored_chunks chunk objects, source holds $source_chunks"

command -v "$verify_bin" >/dev/null || fail "encrypted state verifier not found: $verify_bin"
"$verify_bin" state --state-dir "$restore_state_dir" || fail 'shift-verify state rejected the restored state (authentication/decryption failed)'

pg_restore --list "$output/shift.dump" >/dev/null || fail 'postgres dump catalog is unreadable (truncated or corrupt dump)'

query() {
  local value
  if ! value=$(psql "$1" -Atqc "$2"); then
    fail "database query failed ($2)"
  fi
  printf '%s' "$value"
}
source_migrations=$(query "$database_url" "SELECT count(*) FROM schema_migrations")
restored_migrations=$(query "$restore_database_url" "SELECT count(*) FROM schema_migrations")
[[ "$source_migrations" =~ ^[1-9][0-9]*$ ]] || fail "source database has no applied migrations recorded in schema_migrations"
[[ "$source_migrations" == "$restored_migrations" ]] ||
  fail "restored schema_migrations has $restored_migrations rows, source has $source_migrations"
source_head=$(query "$database_url" "SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1")
restored_head=$(query "$restore_database_url" "SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1")
[[ -n "$source_head" && "$source_head" == "$restored_head" ]] ||
  fail "restored migration head ($restored_head) does not match the source head ($source_head)"
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." 2>/dev/null && pwd)
migrations_dir="$repo_root/database/migrations"
if [[ -d "$migrations_dir" ]]; then
  code_head=$(find "$migrations_dir" -maxdepth 1 -type f -name '*.sql' ! -name '*.down.sql' -printf '%f\n' | sort | tail -n 1)
  [[ -n "$code_head" && "$code_head" == "$source_head" ]] ||
    fail "migration head $source_head does not match the code's migration set ($code_head); run shift-control migrations before drilling"
else
  echo "note: database/migrations not found next to this script; skipped the migration-head-vs-code check" >&2
fi
for table in organizations machines; do
  source_rows=$(query "$database_url" "SELECT count(*) FROM $table")
  restored_rows=$(query "$restore_database_url" "SELECT count(*) FROM $table")
  [[ "$source_rows" == "$restored_rows" ]] ||
    fail "$table row count differs: source $source_rows, restored $restored_rows"
done

printf 'backup restore verification passed\nstate_archive=%s\nkeys_escrow_archive=%s\nrestored_state=%s\npostgres_dump=%s\nrestored_schema_migrations=%s\nmigration_head=%s\ncheckpoint_manifests=%s\nchunk_objects=%s\n' \
  "$output/shift-state.tar.gz" \
  "$([[ "$bundle_keys" -eq 1 ]] && echo 'bundled INSIDE state_archive (--bundle-keys)' || echo "$keys_output/shift-keys.tar.gz")" \
  "$restore_state_dir" "$output/shift.dump" "$restored_migrations" "$restored_head" "$source_manifests" "$source_chunks"
if [[ "$bundle_keys" -eq 0 ]]; then
  echo "reminder: store $keys_output/shift-keys.tar.gz (and the SHIFT_MASTER_KEY_FILE keyfile, when used) apart from $output/shift-state.tar.gz; a backup that carries both is self-decrypting." >&2
fi
