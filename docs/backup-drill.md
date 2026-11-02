# Backup and restore drill

The backup drill is intentionally executable instead of a checklist-only
claim. Run it from a trusted operator host. It needs the `shift-verify`
binary (build it with `make build`, or `go build -o bin/shift-verify
./cmd/shift-verify`) and the PostgreSQL client tools (`pg_dump`, `pg_restore`,
`psql`); point `SHIFT_VERIFY_BIN` at the verifier when it is not on your
`PATH`:

```bash
export SHIFT_MASTER_KEY_FILE=/var/lib/shift-keyfile/master.key   # sealed keyrings only
./scripts/backup-verify.sh \
  --state-dir /var/lib/shift \
  --database-url "$SHIFT_DATABASE_URL" \
  --output /var/lib/shift-backups/$(date -u +%Y%m%dT%H%M%SZ) \
  --keys-output /var/lib/shift-key-escrow/$(date -u +%Y%m%dT%H%M%SZ) \
  --restore-state-dir /var/lib/shift-restore-test \
  --restore-database-url "$SHIFT_RESTORE_DATABASE_URL"
```

## What the drill produces

A run leaves exactly three artifacts, and the split is deliberate:

1. `shift-state.tar.gz` (in `--output`) — encrypted agent state: chunkstore
   ciphertext (`objects/`), the encrypted checkpoint index and manifests
   (`checkpoints/`), and machine identity (`identity/`). **It never contains
   `keys/`.** The archive still holds the machine's Ed25519 identity key and
   TLS key, so store it as confidential material.
2. `shift-keys.tar.gz` (in `--keys-output`) — the master keyring and workload
   keys (`keys/`), exported **separately** so no single artifact can decrypt
   the checkpoints. `--keys-output` must never live inside `--output` or the
   state directory.
3. `shift.dump` (in `--output`) — a PostgreSQL custom-format dump of the
   control-plane database.

## The key boundary

Master keys and workload keys are never bundled with the ciphertext:

- Without `SHIFT_MASTER_KEY_FILE`, `keys/master-keyring.json` stores master
  keys as raw base64. The drill **refuses to run** against such an unprotected
  keyring unless you pass the unsafe `--allow-plaintext-keys` (lab use only).
- With `SHIFT_MASTER_KEY_FILE` set, master keys are sealed under the external
  keyfile. The keyfile lives outside the state directory and is inside **no**
  archive; the drill requires it to be readable (it is needed again at restore
  time) and refuses to run without it.
- Bundling `keys/` into `shift-state.tar.gz` requires the explicit unsafe
  `--bundle-keys`; the drill prints loud warnings, and the artifact is
  self-decrypting. Never use it for a production backup.

Escrow procedure for every drill/backup run:

1. Write `shift-keys.tar.gz` to a location **separate** from the state
   archive — different volume/host/administrative domain, access-controlled
   (a secrets manager is appropriate).
2. Keep the `SHIFT_MASTER_KEY_FILE` keyfile in a third location, again with
   its own access control.
3. Store the state archive and DB dump in the backup location that operations
   can reach, but a backup that carries both the state archive and either key
   artifact is equivalent to plaintext checkpoint data.

## What the script verifies

1. Refuses empty or wrong inputs up front: `keys/master-keyring.json`,
   `checkpoints/index.enc.json`, at least one checkpoint manifest and one
   chunk object must exist in the source state (`--expect-checkpoints N`
   asserts an exact manifest count), and the keyring must be sealed (or
   explicitly accepted via `--allow-plaintext-keys`).
2. Refuses overlapping paths (`--output`, `--keys-output`, and the restore
   targets must not nest inside each other or the source state directory) and
   a non-empty restore database.
3. Archives the state (without `keys/`), exports `keys/` separately (unless
   `--bundle-keys`), and creates the `pg_dump` custom-format dump.
4. Lists the archive and fails on empty, truncated, or corrupt archives;
   asserts the archive really contains the checkpoint index, manifests, and
   chunks — and, unless keys were bundled, contains **no** `keys/` while the
   escrow archive does contain the keyring.
5. Extracts the archive and (from escrow) the keys archive into the restore
   target, restores the dump into the restore database.
6. Requires the restored inventory to match the source exactly (manifest and
   chunk counts) and the keyring to be present before verification, then runs
   `shift-verify state` over the restored tree: every checkpoint manifest is
   authenticated and every referenced asset chunk is decrypted and
   hash-checked. For a sealed keyring this needs `SHIFT_MASTER_KEY_FILE`.
7. Verifies the database leg: `schema_migrations` must be non-empty and match
   the source row count, the migration head must match the source head and —
   when run from a checkout — the head of `database/migrations/`, and row
   counts of `organizations` and `machines` must match the source.
8. On completion writes `shift_backup_*` metrics to
   `SHIFT_BACKUP_TEXTFILE_DIR` when set (see "Monitoring").

## Secrets to escrow with each backup

A fresh machine can only be rebuilt from the backup plus these items, none of
which are inside `shift-state.tar.gz`:

| Secret | Where it lives | Escrow |
|---|---|---|
| Keys archive (`shift-keys.tar.gz`) | written to `--keys-output` by the drill | separate location from the state archive |
| Master-key keyfile (`SHIFT_MASTER_KEY_FILE`) | operator-provisioned, outside the state dir | third location; never inside any archive |
| Agent reporter config | agent config file/env (`api_key`, `machine_id`, control URL) | secrets manager or config backup |
| Database credentials (`SHIFT_DATABASE_URL`) | agent/control-plane environment | secrets manager |

## Keyfile loss is data loss

Losing the `SHIFT_MASTER_KEY_FILE` keyfile makes every sealed keyring — and
with it every checkpoint — permanently undecryptable. This is by design; there
is no escrow, split-knowledge, or recovery mechanism inside SHIFT. Therefore:

- Escrow the keyfile with every backup, in a location separate from both
  archives, and record the escrow location in the release evidence bundle.
- Master-key rotation (which re-wraps workload keys and retires superseded
  master keys in place) keeps older backups readable **as long as the same
  keyfile is kept**. If the keyfile itself is rotated, retain the old keyfile
  for as long as any backup sealed under it exists.
- If the keyfile is lost, declare the checkpoint data unrecoverable and
  restore workloads from source; do not present the backup as restorable.

## Production restore (fresh machine)

A production restore is a controlled exercise; never restore an agent state
archive over a live state directory. With the escrow items in hand:

1. Provision the host and install the SHIFT binaries and PostgreSQL.
2. Restore `shift.dump` into a fresh database (`pg_restore --exit-on-error`),
   then run `shift-control` migrations so the schema reaches the code head.
3. Restore `shift-state.tar.gz` into the agent state directory; extract
   `shift-keys.tar.gz` (keys escrow) into the same state directory, or
   provision the `SHIFT_MASTER_KEY_FILE` keyfile and export the variable.
4. Recreate the agent configuration (control URL, organization, `machine_id`,
   reporter `api_key`) from the escrowed config secrets.
5. Run `shift-verify state --state-dir <state-dir>` — it must print
   `encrypted state verification passed`.
6. Start the agent and run `shiftgate doctor` plus checkpoint
   manifest/chunk verification before returning the host to service.

## Object-storage mirrors are not a recovery path

Mirroring uploads checkpoints to object storage, but the download/restore
path (`Mirror.Download`) has **no production entry point** — no CLI command
and no agent endpoint invokes it. Treat the mirror as a write-only extra
copy. The only supported recovery path is the artifacts this drill produces
(state archive + keys escrow + DB dump). Do not rely on mirroring as your
backup strategy until a restore command ships.

## Monitoring

The drill writes `shift_backup_last_run_timestamp_seconds`, `shift_backup_last_run_success`, and `shift_backup_last_success_timestamp_seconds`
into `SHIFT_BACKUP_TEXTFILE_DIR` for the node_exporter textfile collector.
`deployments/monitoring/alerts.yaml` pages on `ShiftBackupFailed` (last run
failed) and `ShiftBackupStale` (no successful drill in 48h). The solo Compose
stack schedules the database backup sidecar nightly, but it cannot safely invent
restore credentials or a second database. The repository includes an honest
systemd example (`deployments/systemd/shift-backup-drill.service`, `.timer`, and
`backup-drill.env.example`); install it only after provisioning isolated empty
restore state/database targets and the escrowed keyfile. A production run must use
`--keys-output` outside `--output` and set `SHIFT_BACKUP_TEXTFILE_DIR`.


## Claims boundary and release evidence

The drill does not claim that CRIU restore ran unless the real-CRIU integration
suite is also executed as root. The release evidence bundle must contain both
results, the age of the last successful drill, the backup location, and the
escrow locations of the keys archive and keyfile.
