# SHIFT Architecture

SHIFT is split into a privileged Linux agent, an optional PostgreSQL-backed control plane,
and clients. The agent owns computational state; the control plane stores fleet metadata,
authorization state, migration intent, billing entitlements, and audit records.

## Agent

`shift-agent` runs on Linux x86_64 and exposes a Unix-socket API for the local user. A
separate HTTPS listener carries peer migration traffic. Local requests are authorized from
Unix peer credentials; peer requests require a machine certificate. Workloads are persisted
in encrypted local collections, and process identity includes `/proc` start ticks to avoid
signalling a reused PID.

Checkpoint creation combines CRIU process images with explicitly configured filesystem
roots. The chunk store compresses and encrypts each bounded-size chunk (zstd at its default
level; chunks written by earlier versions in gzip remain readable — the codec is identified
by the decompressed frame's magic, not trusted from the manifest), addresses it with a
keyed digest, and verifies both ciphertext and plaintext before restore. Manifests are
signed by the source machine identity and encrypted with the workload data key. Optional
object-store mirroring uploads that encrypted envelope and the encrypted chunks; a
destination decrypts and authenticates the envelope before importing any chunk or metadata.

A restore is a two-phase transaction — prepare stages the filesystem and the
process image set, switches the root, restores and validates the tree, then
commit reaps the superseded source and drops the staging state — and an
opt-in lazy variant (`restore --lazy`) starts the tree before its memory is
materialized: the agent runs `criu lazy-pages` as a foreground child over the
image set, CRIU's restore hands page serving to it through userfaultfd, and
the daemon exits once it has served every image page (or the process is
gone), at which point a watch removes the now-derived image set. The restore
record carries an honest `time_to_first_execution_ms` for both variants, and
the lazily restored process's own checkpoints remain correct — reading its
memory faults the remaining pages in through the daemon.

A clone set is the same restore transaction applied N times to one checkpoint
on one machine (`clone CHECKPOINT_ID --count N`): the chunk store's contents
are extracted once, every member's filesystem copy is a reflink clone where
the filesystem allows it, and each member restores through a private mount
namespace bind mounting its own root over the path the checkpointed images
record — the mechanism a fork uses, applied per member. Members restore in a
bounded parallel pool (default 4, cap 16); the set is all-or-nothing with a
persisted record that recovery can reverse after an agent restart mid-set.

## Filesystem

The `internal/filesystem` package decides what SHIFT can honestly do with a workload
root's storage. It probes the filesystem the root lives on, takes atomic copy-on-write
snapshots where the filesystem provides them (btrfs read-only subvolumes, which let the
workload resume while the archive is read), clones trees with reflinks where the kernel
allows and byte copies where it does not, tracks file-level changes between checkpoints
by stamping size/mtime/mode/inode, and discovers the files a command depends on by
reading ELF `DT_NEEDED` chains and script shebangs. Every checkpoint manifest records
which capture mechanism ran and which dependencies sit outside the root — a
destination can see exactly what will and will not travel with the process image.

Migration is a persisted state machine. Destination reservation, workload-key import,
manifest negotiation, missing-chunk upload, verification, restore validation, commit, and
source cleanup are separate stages. A failed or cancelled migration rolls back the
destination transaction when possible and keeps the source workload available.

## GPU and Device Compatibility

The `internal/device` package inventories what a machine can prove about its
accelerators: NVIDIA GPUs through `nvidia-smi` and the driver's procfs files,
AMD GPUs through the kernel's DRM topology with `rocminfo` refining model names
and gfx targets, plus the CUDA and ROCm environments installed on the machine.
Each advertised GPU carries its vendor, model, driver version, memory, compute
capability, and the device files a restored process would need. The
checkpoint/restore capability is an advertised fact derived from the driver
line (NVIDIA R550 and newer), never a guess — AMD devices report it as false
because ROCm has no process GPU state checkpoint API.

Required devices flow through the same decision everywhere: the compatibility
checker before a migration and the scheduler during placement both call
`MatchGPU`, which matches vendor, memory, and compute capability (compared
numerically for NVIDIA dotted levels, exactly for AMD gfx architectures) and
prefers an exact model match. The outcome is one of three tiers: incompatible
or restore-incapable is an error, a different-but-capable GPU model or a
missing/older runtime environment is a warning the operator sees, and a full
match is silent. The workload's device policy selects between rejecting an
unavailable GPU (the default) and starting without it under a warning — except
that a requirement for GPU checkpoint/restore is always fatal when the
destination cannot honor it, because a warning there would silently discard
state the checkpoint claims to carry.

## Network

The `internal/network` package owns everything about a workload's reachability. Each
workload gets a virtual address derived deterministically from its id in the RFC 6598
carrier-grade range — an identifier that follows the workload, never a configured route.
Declared ports are resolved into NAT mappings: a mapping whose host and container ports are
equal is bound by the restored process itself, and any other mapping is served by a TCP
forwarder the agent runs on the host port. A machine-local NAT table reserves host ports
before a restore starts, so two workloads can never silently take each other's ports, and
forwarders drain — stop accepting, let in-flight connections finish, then close — when a
workload rolls back, stops, is deleted, or migrates away under `drain` policy.

The migration plan is computed once, before anything moves, and stored on the migration
record: which sockets the checkpoint will carry, which listeners are re-established, which
connections are dropped. The destination follows the same plan when it publishes ports.
Nothing claims transparent socket migration — TCP state only travels when CRIU TCP repair is
requested and the destination accepts it — and every migrated, restored, or forked workload
receives a `.shift/migration-status.json` document in its root stating what actually
happened to its sockets and listeners, because the environment of a CRIU-restored process
cannot be rewritten.

## Control Plane

`shift-control` is intentionally not a state store. It records organizations, memberships,
machines, workload metadata, migration jobs, entitlements, sessions, and audit events in
PostgreSQL. Access tokens are opaque, short-lived bearer values whose HMAC digests are
stored; refresh tokens rotate transactionally. Passwords use Argon2id with a deployment
pepper. Organization routes enforce viewer/operator/admin/owner roles server-side.

Stripe subscription webhooks are signature-checked, timestamp-bounded, and idempotent by
event ID. Entitlement checks occur in the database transaction that registers machines.
The plan catalog lives in the control plane (`internal/billing`); it defines each tier's
price and limits, serves the public `/v1/plans` endpoint the pricing page renders, and is
the authority a webhook consults — a catalog plan's limits always come from the catalog,
never from subscription metadata. Checkout and billing-portal sessions are opened by the
control plane's outbound Stripe client; a negative limit (`-1`) means the unlimited tier
and is honored as such by the machine and storage entitlement checks.

An agent may optionally send machine presence to `shift-control` with a scoped API key.
That reporter updates registration metadata such as status, inventory, and advertised peer
address. It is one-way presence reporting; it does not expose workload keys, checkpoint
chunks, or dashboard command dispatch.

## Periodic checkpoint policies

A workload's spec can carry a checkpoint policy (interval, optional retention count). A
scheduler goroutine in the agent reconsiders policies every 5 seconds: a running workload
whose interval has elapsed — measured from its newest checkpoint, or its creation when
none exists — gets a full `leave_running` checkpoint, after which retention deletes the
oldest snapshots beyond the count, never an ancestor a kept checkpoint still needs. The
schedule is derived from persisted facts only, so it survives agent restarts; workloads
that are not running or have a migration in flight are skipped. Policies are per-agent
and per-workload — there is no cross-node coordination, because each agent is the
authority for the workloads it owns.

## Warm-standby failover

A workload's spec can additionally carry a failover policy (a standby's peer URL, its
expected machine id, a retention count), which is only valid paired with a checkpoint
policy — the schedule is what produces replicas, and removing the schedule while a
standby waits would strand it at the checkpoint it holds, so the pair is enforced in both
directions. The source's replicator runs alongside the scheduler: each time the scheduler
makes a root checkpoint, the newest one — process image, filesystem root, and the
workload data key — is pushed to the standby over the same mutually authenticated peer
channel a migration uses. Incrementals are not replicated; the standby always holds
self-contained roots, so any one of them restores on its own.

The standby side is a duty list. Each duty records the workload id, the authenticated
source identity, the checkpoint it is armed with, retention, and its failover history,
and the standby's tick loop (10s) runs retention on armed duties and then judges death.
The judgment is two-signal by construction: the standby first reads the control plane's
presence record for the source — looked up by the machine id it authenticated, which is
why the source's configured `control_plane.machine_id` must match its agent identity —
and a record that is missing or fresh means no action. Only when the record says the
source is offline, or its last heartbeat is older than 90 seconds, does the standby
probe the source's advertised peer listener itself; a probe that fails on a
misconfiguration (no URL, unreachable) never fails over on its own — silence alone is
not death. When both signals agree, the standby runs the restore path (prepare, commit,
rollback on failure), marks the duty failed-over with the reason, and clears the
workload's failover policy while leaving its checkpoint schedule — the restored workload
is live but unprotected until re-armed. A failed attempt is recorded on the duty and
retried a minute later. The operator path, `standby trigger`, runs the same restore but
starts by probing the source and says so when the source answers. There is no fencing
mechanism — nothing revokes the source's claim to the workload — so a failover during a
live source runs an independent copy; the probe warning and the staleness bound are the
documented defenses, not a guarantee.

## Observability

The agent emits structured JSON logs (slog), serves Prometheus text metrics on its local
socket, and propagates W3C Trace Context (`traceparent`) end to end.

`GET /metrics` renders HTTP series (uptime, requests, errors, active requests, response
statuses, duration) plus the migration diagnostics: outcome counts and success ratio,
duration and downtime (last and cumulative), checkpoint and restore outcome counts, transfer
bytes per direction, and transfer time. A sampler goroutine publishes resource gauges every
30 seconds — memory total/available, load averages, free space on the state filesystem,
tracked workload count, a sample timestamp — and GPU utilization/memory every 5 minutes,
measured through `nvidia-smi` or the amdgpu sysfs counters. A GPU whose tooling is absent
produces no gauge rather than a fabricated zero; the sample timestamp going stale is the
agent-health signal. Diagnostics are process-local and reset on restart; durable history
lives in the encrypted migration, checkpoint, and restore records.

A migration is one trace. The CLI mints the root trace when it creates the migration, the
agent's middleware carries it into the orchestrator's background run, and every peer call —
reserve, key and manifest import, each chunk upload, verify, restore, commit — propagates it
to the destination agent, so source and destination land in one trace view. Metrics scrapes
and health probes are exempt. Spans go to the structured log at debug level and, when
`tracing.otlp_endpoint` (or `SHIFT_OTLP_ENDPOINT`) is configured, to an OTLP/HTTP JSON
collector over stdlib `net/http`. Telemetry never blocks or fails a migration: the exporter
buffers with a bounded queue, drops with counters when full, and retries on its own
goroutine.

Logs, spans, and metric labels carry operational facts only — ids, stages, byte counts,
durations, machine identities. Secrets, workload keys, and computational contents are never
logged.

## Boundaries and limitations

The current live-migration mode interleaves CRIU `pre-dump` with transfer, the way
spec §10 draws the flow: an initial checkpoint is transferred while the workload runs,
changes are tracked, and each further pass's delta transfers the moment the pass
completes — each pass parenting the previous one so unchanged memory travels early —
until a pass's delta converges or a pass cap is reached. Each pass's images are
packaged as their own content-addressed asset, so chunks stream to the destination in
`KEY_READY` state (authenticated by machine identity and content digests, before any
manifest exists), and the destination session is reserved and measured from pass 1's
actual packaged size rather than a guess. The short freeze at the end pays only the
final delta. A migration record's `downtime`
is the freeze-to-resumed window: from the pause before the final dump (recorded in the
checkpoint manifest as `freeze_started_at`) to the destination's restore call returning
with the process resumed and health-validated. It is never the migration's wall clock —
pre-copy passes run while the workload is live, and the commit and source-cleanup stages
run after it is already executing on the destination — and a live migration that fails
before its freeze reports zero downtime, because the workload ran the whole time; its
metrics separate `pre_copy_transferred_bytes` from the frozen window's transfer. It does not assume a copy-on-write
filesystem adapter. Network identity is
preserved as a SHIFT-layer virtual address with published port mappings, never claimed as a
routable address that follows the process; socket state itself only travels under CRIU TCP
repair. GPU state and external services are represented in compatibility reports and state
inventories rather than silently claimed portable. The control plane does not receive raw
checkpoint chunks or workload keys.
