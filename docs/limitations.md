# Current Limitations

- Linux x86_64 is the supported platform. Other operating systems and architectures are
  rejected by the agent configuration rather than treated as compatible.
- CRIU and its kernel capability check are required for real checkpoint/restore. Test
  engines used in unit tests do not provide production checkpoint semantics.
- A privileged agent runs every workload the way a container runtime does: as the init
  of its own PID namespace, leader of its own session, born into its own cgroup, and
  behind a seccomp filter that fails the three io_uring syscalls with EPERM. Each part
  is a checkpointability requirement, not hardening for its own sake: CRIU refuses to
  dump a process whose session leader lives outside its PID namespace, records two
  cgroups for one tree when a workload forks before an attach lands, and cannot dump
  the `anon_inode:[io_uring]` mappings a process holds — libuv (Node) creates those
  for synchronous file operations and ignores its `UV_USE_IO_URING` switch, so denial
  is the only reliable fix; runtimes fall back to plain syscalls on the EPERM. The
  filter also sets no-new-privileges, so a workload cannot exec a setuid binary to
  root. Two consequences follow: a workload that creates a nested PID namespace
  inside its own cannot be checkpointed (CRIU 4.2), and a workload that installs no
  SIGTERM handler ignores `stop`'s graceful signal the way any PID-1 process does —
  it is killed when the stop timeout expires. Unprivileged agents run workloads
  directly in the host namespaces without the filter; they cannot checkpoint
  anything, so there is nothing to protect.
- Workload resource limits (memory, pids, cpu) are enforced through cgroup v2 control
  files, which the kernel creates in a workload's cgroup only for controllers enabled
  in the SHIFT root's subtree_control. The agent enables memory, pids, and cpu under
  its root when the kernel offers them; a machine that does not offer a controller
  fails a workload declaring that limit with the controller named, rather than
  starting it silently unconfined. A workload the kernel kills for exceeding a
  limit — an OOM kill inside its cgroup — is reported as failed with the
  kernel's reason, never as a clean stop; only a stop the operator or the
  agent itself ordered reads as stopped. A restore parks the checkpoint's
  frozen source tree in a private cgroup for its duration — otherwise the
  recreated
  tree and its rollback copy would share one cgroup, and the workload's own
  pids and memory limits would count both, failing the restore halfway
  through on a budget it was supposed to fit in. A commit reaps the parked
  copy; a rollback moves it back and resumes it.
- Incremental checkpoints are supported through retained CRIU parent images. The child
  archive carries the reconstructed image set and records its parent for deduplication and
  lineage. Restoring a chain validates and materializes ancestors oldest-first.
- Live migration uses CRIU pre-copy (`pre-dump`) before the final checkpoint. The final
  checkpoint remains authoritative and the workload is stopped for the final transfer;
  transparent dirty-page convergence and filesystem copy-on-write are not assumed.
- Filesystem capture is always consistent, never torn, by one of two mechanisms, and the
  checkpoint manifest records which one ran. On btrfs, a workload root that is a
  subvolume is snapshotted atomically (`btrfs subvolume snapshot -r`, which requires
  btrfs-progs) and the archive is read from the snapshot, letting the workload resume
  before the bytes are read. On every other filesystem — or a btrfs directory that is
  not a subvolume — the archive is read straight from the root while the process is
  frozen; SHIFT cannot make ext4 or xfs snapshot, and does not pretend otherwise.
- Changed-file tracking between checkpoints is descriptive, based on size, mtime, mode,
  and inode stamps, and is reported as counts in the manifest and CLI output. The
  integrity mechanism remains the chunk store's keyed digests; the tracking never
  decides what is transferred, only what is reported.
- Dependency discovery reads ELF headers (`DT_NEEDED`, the interpreter) and script
  shebangs to list the files a command needs, marking which live inside the workload
  root and travel with the checkpoint. Libraries a process loads with `dlopen` after
  startup cannot be discovered statically and are absent from that list.
- Restoring or forking onto a different filesystem than the agent's staging directory
  cannot use rename; the root is cloned instead — copy-on-write where the filesystem
  supports reflinks, a byte copy otherwise. Device nodes in a cloned tree are only
  reproduced when the agent runs as root.
- Transparent TCP/socket migration is only attempted when CRIU TCP repair is explicitly
  requested and the compatibility report permits it. Otherwise workloads must reconnect or
  drain according to their network policy. Everything else a workload's network needs is
  re-established at the SHIFT layer, and the workload is told what happened:
  - Every workload has a virtual address derived from its id in the RFC 6598 range
    (100.64.0.0/10). It is an identifier that follows the workload across machines, not a
    routable address; nothing is assigned to an interface.
  - Declared ports are reserved before a restore begins and published after the restored
    process passes its health check. Mappings with distinct host and container ports are
    served by a SHIFT TCP forwarder; equal ports are bound by the workload itself. Only TCP
    is forwarded — a UDP or Unix-socket declaration fails validation instead of silently
    not coming back.
  - Two workloads restored on one machine can never silently take each other's host ports;
    the second reservation fails the migration at validation, not mid-restore.
  - Under `drain` policy the source's forwarded connections are drained (listeners stop
    accepting, in-flight connections get a grace period) before the checkpoint. SHIFT
    cannot drain connections a workload accepted on a directly bound listener; checkpointing
    such a workload without TCP state fails with the real CRIU error rather than pretending
    the connections traveled.
  - After every migration, restore, or fork, a `.shift/migration-status.json` document in
    the workload's root states the true disposition: whether socket state was carried, which
    connections were dropped, and which ports are served by forwarders. Applications read
    that file — the process image restored by CRIU cannot have its environment rewritten.
- GPU migration requires a matching vendor device and an explicitly advertised checkpoint/
  restore capability. Generic device files are never copied blindly. GPU detection reports
  only what the machine proves: NVIDIA inventory needs `nvidia-smi`, AMD inventory reads
  the kernel's DRM topology and only trusts `rocminfo` names when rocminfo saw exactly as
  many GPU agents as the kernel saw AMD cards. The NVIDIA checkpoint/restore capability is
  derived from the driver line (R550 and newer, which ship the CUDA checkpoint APIs); AMD
  devices never advertise it because ROCm has no process GPU state checkpoint API, so a
  GPU-dependent AMD workload cannot live-migrate. A different-but-capable destination GPU
  model, or a missing/older CUDA or ROCm environment, is a compatibility warning — the
  migration proceeds and the operator sees what differs — never a silent substitution. A
  workload's device policy (`--device-policy`, default `reject_incompatible`) decides what
  an unavailable GPU does: the default rejects the migration, `warn_incompatible` lets the
  workload start without the accelerator and says so. A workload that requires GPU state
  checkpoint/restore is rejected under both policies when the destination cannot restore
  that state, because proceeding would silently discard it.
- The control plane stores machine presence and migration intent. Agents optionally report
  presence with a scoped API key, but the dashboard cannot start, stop, checkpoint, restore,
  or dispatch migrations. CLI/agent operations are not automatically reconciled into
  control-plane workload, checkpoint, or migration records; agents perform actual transfers
  directly. Object-storage mirroring is implemented for local and
  S3-compatible backends but is not enabled by default; operators must provision backend
  credentials, lifecycle policies, quotas, and backups.
- Forking produces an independent workload with its own filesystem copy, its own key
  namespace, its own full checkpoint, and its own virtual network address; SHIFT
  deliberately implements no state-merge operation, and only records the lineage a future
  merge would need. `--activate` restores the forked process tree at the same time as its
  source by bind mounting the fork's root over the source's recorded root inside a private
  mount namespace, which requires the privileges CRIU restore already needs. A fork's host
  ports default to its container ports; forking a port-publishing workload on the machine
  where the original still holds those ports fails with an explicit port-conflict error
  before anything is copied, because two workloads cannot occupy one host port.
- A fork whose process tree is activated runs inside a private mount namespace, so
  checkpointing that fork again from outside the namespace is not supported. Fork state is
  captured before activation, so the fork's own checkpoint is always available; re-capture
  the fork by restoring that checkpoint into its own root instead.
- Observability is per-process. Structured logs, the Prometheus endpoint, traces, and
  diagnostics series (migration outcomes, durations, downtime, checkpoint and restore
  outcomes, transfer volume, resource gauges) reset when the agent restarts; durable history
  lives in the encrypted migration, checkpoint, and restore records and in whatever
  collector scrapes the endpoint. GPU utilization gauges exist only for GPUs whose vendor
  tool (`nvidia-smi`) or sysfs counters (`gpu_busy_percent`) are present — an unreadable
  sensor produces no gauge, never a zero. Trace export ships OTLP/HTTP JSON with no SDK; a
  slow or down collector drops spans (counted) after one retry rather than blocking a
  migration. Error reporting is structured logs plus metrics — no third-party crash
  reporter is embedded, so crash reports go only where the operator points stderr or a
  collector.
- Billing covers subscription plans through Stripe: a server-side catalog, hosted checkout
  and billing-portal sessions, signature-verified subscription webhooks, and
  database-enforced machine/storage entitlements. Metered usage beyond subscription limits
  is recorded (usage summaries) but not billed by unit, and no proration, invoicing, or
  dunning logic runs locally — Stripe's portal handles all payment operations.
- Single sign-on federates to exactly one OIDC issuer per control plane: all organizations
  share it, an organization cannot pick a different issuer, and only authorization-code
  flow with PKCE is supported (no implicit, hybrid, device, or CIBA flows). ID tokens must
  be RS256 or ES256; the control plane does not fetch keys over plain http, and an issuer
  whose JWKS rotates to an unknown `kid` works only after one automatic refresh.
  Enforcement is per email domain and one organization claims a domain at a time —
  sub-domains are separate domains, and an address with no domain is never enforced.
  SAML and WS-Federation are not supported.
- SCIM v2 implements the Users resource only. Groups, enterprise extension attributes,
  bulk operations, sorting, and ETags are not implemented, and the service provider
  configuration says so. Filters support `eq`, `co`, `sw`, `ew`, and `pr` joined by `and` —
  no `or`, `not`, grouping, or complex value paths — and unsupported shapes are rejected
  with a `400` rather than approximated. `PATCH` supports only the `replace` operation.
  SCIM delete deactivates rather than deletes, so an account disabled in the IdP keeps its
  history (and its unique email) forever unless an operator removes the row directly.
- A workload filesystem root must be explicitly configured and cannot be `/` or an
  unrestricted home directory.
