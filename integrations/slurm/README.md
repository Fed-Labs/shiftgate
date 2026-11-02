# SHIFT Slurm adapter

This adapter is intentionally a thin sidecar: Slurm owns scheduling and SHIFT
owns checkpoint/restore/migration state. The adapter calls the control-plane API
only; it never dials an agent peer listener.

## Trust model

The adapter authenticates to the control plane with an organization bearer
token over `https://` only (`SHIFT_CONTROL_URL must use https://`; the Slurm
client rejects anything else). The control-plane-to-agent hop that a
checkpoint command triggers is a separate leg the adapter never crosses:
that hop is authenticated by placement (the agent's Unix socket is 0660 /
group-restricted, or the control plane presents TLS client certificates),
not by application-level bearer credentials. Keep agents on a private
interface and the control plane's agent channel on a mutually-authenticated
transport; the adapter's security guarantees end at the control-plane API.

## Contract

Configure `SHIFT_CONTROL_URL`, `SHIFT_CONTROL_TOKEN`, and `SHIFT_ORGANIZATION_ID`.
For a Slurm job, store the SHIFT workload id in the job environment as
`SHIFT_WORKLOAD_ID`. Use a stable Slurm job id as the idempotency-key suffix.

```bash
# checkpoint before preemption or maintenance
slurm/shift-slurm.sh checkpoint "$SLURM_JOB_ID" "$SHIFT_WORKLOAD_ID" "$SHIFT_MACHINE_ID"

# migrate to a selected destination machine
slurm/shift-slurm.sh migrate "$SLURM_JOB_ID" "$SHIFT_WORKLOAD_ID" \
  "$SHIFT_SOURCE_MACHINE_ID" "$SHIFT_DESTINATION_MACHINE_ID" cold

# job-end cleanup (Slurm epilog): cancel the job's in-flight migration
slurm/shift-slurm.sh cancel "$SLURM_JOB_ID"
```

The same operations are available from the Go CLI (`shift-slurm`), which also
provides `poll MIGRATION_ID` for `sacct`/operator state tracking.

### Checkpoint budget

A checkpoint command runs a synchronous CRIU dump that takes minutes for real
HPC workloads. The adapters therefore send `timeout_seconds: 600` (the control
plane's cap is ten minutes) and give the request its own deadline
(`--checkpoint-timeout`, default 15m for the CLI; `SHIFT_CHECKPOINT_TIMEOUT_SECONDS`
for the shell hook) instead of the 30-second quick-operation timeout. Run the
checkpoint hook from Slurm preemption and drain hooks.

### Failure semantics

The commands return non-zero on an API failure, print the error to stderr, and
print the control-plane response (the registered checkpoint id for
`checkpoint`, the migration record including its id for `migrate`) on stdout.
A failed checkpoint is never silent: the hook reports it and exits non-zero so
your hook framework can alert. If your framework treats a non-zero hook as
fatal to the job, wrap the call accordingly.

### Idempotency

Duplicate `migrate` callbacks reuse the same idempotency key
(`shift-slurm-migrate-$job_id`), which the migrations endpoint honours, so
they cannot create duplicate migration jobs. The checkpoint commands endpoint
does not yet durably replay idempotency keys: the adapter forwards the key
(`shift-slurm-checkpoint-$job_id`) for forward compatibility and never
auto-retries a checkpoint POST, but a Slurm re-run of the hook can still
produce a second checkpoint. Treat checkpoint retries as operator-visible
events, not as free no-ops.

### Cleanup on job end

`migrate` records its migration id under `${SHIFT_SLURM_STATE_DIR:-/tmp/shift-slurm}/$job_id.env`.
Register `shift-slurm.sh cancel "$SLURM_JOB_ID"` as a Slurm epilog (or a
`SIGTERM`/`SIGKILL` trap in the job script) so a job that dies mid-migration
cancels its in-flight migration: an orphaned active migration locks the
workload (one active migration per workload) and holds the destination
machine's capacity. `cancel` is idempotent — an already-settled migration
counts as cleaned up.

A production deployment should invoke checkpoint on Slurm preemption and drain
hooks, and use the Go CLI's `poll` (or the migration event endpoint) to update
`sacct`/operator state. `poll` degrades gracefully: if the events endpoint is
unavailable the migration record is still reported, with `events_error` set.
