# CLI

The `shiftgate` client talks to the local agent and emits either tables or JSON:

```sh
shiftgate doctor
shiftgate machines
shiftgate workload create demo --path "$PWD" -- /usr/bin/python3 -m http.server 8080
shiftgate workload start demo
shiftgate workload policy demo --every 10m --keep 5
shiftgate checkpoint create demo --leave-running
shiftgate restore CHECKPOINT_ID
shiftgate restore CHECKPOINT_ID --lazy
shiftgate checkpoint mirror CHECKPOINT_ID
shiftgate fork demo --name demo-experiment
shiftgate fork demo --activate --root /srv/demo-b
shiftgate fork list
shiftgate clone CHECKPOINT_ID --count 50 --prefix worker
shiftgate clone list
shiftgate clone inspect CLONE_ID
shiftgate clone rollback CLONE_ID
shiftgate migrate demo --to https://workstation.example:8443 --machine-id DESTINATION_ID
shiftgate migrate demo --dry-run --to https://workstation.example:8443
shiftgate migrate demo --mode live --pre-copy-passes 4 --to https://workstation.example:8443 --machine-id DESTINATION_ID
shiftgate workload failover demo --to https://standby.example:8443 --machine-id STANDBY_ID --keep 3
shiftgate workload failover demo --off
shiftgate failover status
shiftgate standby list
shiftgate standby trigger WORKLOAD_ID
shiftgate standby trigger WORKLOAD_ID --lazy
shiftgate status
shiftgate update status
shiftgate update check
shiftgate update apply --version 1.4.2
shiftgate update rollback
shiftgate update block 1.4.3 --reason "restarted the workload under load"
shiftgate update unblock 1.4.3
```

Use `--agent unix:///path/to/agent.sock`, `--json`, and `--timeout` to override defaults.
Exit codes distinguish usage errors (2), failed prerequisites or transport (3), missing
resources (4), failed/rolled-back migrations (5), and deferred updates (6).
`shiftgate completion bash|zsh|fish` generates shell completion snippets.

A live migration (`--mode live`) runs CRIU pre-dump passes while the workload keeps
running and stops the process only for the final transfer. `--pre-copy-passes N`
(0–16, default 0) caps the passes; each pass parents the previous one, and the loop
ends early once a pass's delta converges, so 0 means a single pass and 4 does not
promise four — a write-heavy workload honestly runs to the cap and pays the longer
freeze. Each pass's images transfer as the pass completes, so the freeze pays only
the final delta; the migration record reports the split (`pre_copy_transferred_bytes`
against the frozen window's transfer). A live migration that fails before its freeze
reports zero downtime — the workload ran the whole time and stays running on the
source — and the destination refuses the whole migration up front when its free
storage cannot hold the reservation the source measured from pass 1.

Migrations can also be started from the web dashboard: the Workloads page (or a
workload's detail page) has a MOVE button that opens a destination-and-mode picker and
dispatches the same `migrate` agent command through the control plane — it needs the
source machine's `agent_url` registered and is refused for machines without a reachable
peer listener, so it never queues a move the agent can't execute. Progress lands on the
dashboard's Migrations page, which shows the same stage events the CLI does.

A clone set derives many independent running workloads from one checkpoint:
`shiftgate clone CHECKPOINT_ID --count N --prefix worker` restores the
checkpointed process N times on this machine, each clone with its own workload
identity, its own root directory, and its own process — all of them live
continuations of the same state. Members materialize in parallel
(`--parallel`, default 4, capped at 16) and share the cheap parts: the
checkpoint's chunks are read from the store once and every filesystem copy is
clone-on-write (`FICLONE`) where the filesystem supports it, so on btrfs or
xfs a set of fifty costs fifty restores plus metadata, not fifty full copies.
`--count` is capped at 128 per set. A clone of a workload that declares TCP
ports is a single clone — every member would rebind the same host port — and
larger sets of such workloads are refused up front rather than half-started.
The set is all-or-nothing: any member failure rolls back every member, and
the record (`clone inspect`) names the member that failed and why. A
committed set is a fleet of ordinary workloads — teardown is the workload
API, never `clone rollback`, which is refused after commit and exists for
sets interrupted before it.

A workload can carry a periodic checkpoint policy: `shiftgate workload create …
--checkpoint-every 10m --checkpoint-keep 5` arms one from the start, and
`shiftgate workload policy demo --every 10m --keep 5` applies or changes it on a
running workload (`workload policy demo --off` removes it). While the workload
runs, the agent snapshots it every interval without stopping it; `--keep N`
deletes the oldest snapshots beyond N. Each periodic snapshot is a full
checkpoint — independently restorable, and it also arms CRIU's memory tracker so
operator-made incremental checkpoints still work alongside. Retention never
deletes an ancestor a kept checkpoint still depends on. The interval floor is
10 seconds, and the schedule is measured from the newest checkpoint — manual or
periodic — so it survives agent restarts and an operator's manual snapshots
push the next automatic one out.

`shiftgate restore CHECKPOINT_ID` brings a checkpointed workload back on this
machine, and `--lazy` changes when it comes back: the process starts executing
before its memory image is fully loaded, and a `criu lazy-pages` daemon serves
page faults on demand while streaming the rest in the background. It needs a
kernel and CRIU build with userfaultfd (`criu check --feature uffd`) and is
refused up front with the reason when either is missing — never silently
downgraded to an eager restore. The restore record reports
`time_to_first_execution_ms` for every restore, lazy and eager, so the two
compare honestly; on the reference benchmark (a 171 MB workload restored from
local disk) lazy measured 144 ms to first execution against 123 ms eager —
comparable, because materializing 171 MB from local disk is already fast. Lazy
restore is for workloads whose memory takes meaningful time to materialize
(very large heaps, slow storage); on fast local storage it buys nothing. The
daemon keeps the checkpoint's image set on the machine while it serves and
exits when it has finished — every page transferred, or the process gone —
at which point the agent removes the image set; that window is typically
seconds. `shiftgate standby trigger WORKLOAD_ID --lazy` is the same trade-off
on the failover path; automatic failover and migration restores are always
eager — nobody opted into the lazy trade-off there, and a migration's
downtime metric must mean the process is fully there.

Warm-standby failover replicates a workload's checkpoints to a second machine
so it can be brought back there when the first dies. `shiftgate workload
failover demo --to https://standby.example:8443 --machine-id STANDBY_ID
--keep 3` designates the standby (`--keep 0`, the default, retains every
replica; `--off` withdraws the designation and leaves already-replicated
checkpoints behind on the standby). A designation requires a checkpoint
policy — a standby without a schedule would wait forever — and removing the
schedule while a standby waits is refused until the designation goes first.
Each time the scheduler makes a root checkpoint the newest one is pushed to
the standby over the same mutually authenticated peer channel a migration
uses; incrementals are not replicated, so the recovery point is the last root
checkpoint — at most one policy interval of work. `shiftgate failover status`
shows the source's replication ledger; `shiftgate standby list` on the standby
machine shows the duties it holds, the checkpoint each is armed with, its
failover history, and whether automatic failover is armed.

Automatic failover is a two-signal judgment on the standby, never a guess
from one silence: the standby restores a duty only when the source's peer
listener is unreachable and the control plane's presence record for the
source is stale (no heartbeat for 90 seconds) or marked offline. A source
that is merely paused, partitioned, or rebooting keeps its standby armed.
Both machines must report presence to a control plane for the automatic path
— without one, the standby holds its replicas and waits for an operator.
`shiftgate standby trigger WORKLOAD_ID` is that operator path (duties are
keyed by workload id, not name — names are not unique across sources): it
probes the source first and prints a warning when the source answers, then
restores from the newest held checkpoint. The restored workload keeps its
checkpoint schedule but drops its failover designation — it must be pointed
at a standby again to be protected. There is no fencing: a standby that
fails over while the source is actually alive runs an independent copy; the
trigger's probe warning is the operator's check, and the automatic path leans
on the control plane's staleness bound instead.

`migrate --dry-run` answers "would this migration work" before anything moves. The agent
reaches the destination over the same peer channel a real migration uses, runs the same
compatibility check on the same inputs, and prints the two machines, the network plan the
migration would apply, and every warning and rejection with its remedy — an incompatible
destination lists exactly which requirement failed. Nothing is frozen, transferred, or
recorded: no migration record is created and the workload keeps running. Exit 0 means the
migration would be admitted (warnings allowed); exit 3 means it would be rejected. An
unreachable destination, a machine id that does not match the destination's certificate,
and a destination that is the source itself fail with the same codes a real migration
would fail with. `--json` prints the full report machine-readably.

## Control-plane commands

The same binary manages a control-plane session. These commands talk to the
platform's control plane by default — its address ships with the product
(`config.DefaultControlPlaneURL`), the way a hosted service's SDK carries its
endpoint. Pass `--control-url` (or set `SHIFT_CONTROL_URL`) only when pointing
at a private control plane; `--token-store` overrides
where the session is stored (default `~/.config/shift/cli-session.json`, 0600
in a 0700 directory). The password is prompted on the terminal with echo
disabled and is never accepted as a command-line flag, so it cannot land in
shell history or process listings; piping stdin also works for scripts.

```sh
shiftgate login --email operator@example.com    # the platform's control plane
shiftgate --control-url https://control.example.com login --email operator@example.com
shiftgate whoami
shiftgate plans
shiftgate logout
```

When the control plane has an OIDC issuer configured, `login --sso` authenticates through
it instead of a password: the CLI starts a loopback listener, asks the control plane for
the issuer's authorization URL, opens the browser (or prints the URL on a headless
machine), and redeems the one-time authorization code the listener receives. The PKCE
verifier never leaves the process and the code travels only over the loopback interface.
A password is never read, so `login --sso` also works for domains whose organizations
enforce single sign-on — password login for those domains is refused outright.

```sh
shiftgate login --sso --email operator@company.example.com
```

`shiftgate machines` and `shiftgate workloads` show the local machine by
default — even when logged in to the platform — and switch to the fleet view
when `--control-url` or `SHIFT_CONTROL_URL` names a control plane (the
platform's own address counts); `shiftgate fleet` always shows the platform
fleet. The organization defaults to the
first one the account belongs to; pass `--org ID` before a subcommand to
choose another.

```sh
shiftgate machines                          # fleet view when --control-url is set
shiftgate machines register --machine-id workstation --name Workstation --agent-url https://workstation:8443
shiftgate machines capability cuda_version --kind number --minimum 12
shiftgate fleet workloads
shiftgate fleet migrations                  # list jobs
shiftgate fleet migrations MIGRATION_ID     # one job with its event stream
shiftgate fleet checkpoints [WORKLOAD]
shiftgate fleet audit
shiftgate fleet api-keys
shiftgate fleet api-keys create --name ci --scopes machines,workloads
shiftgate fleet api-keys revoke KEY_ID
shiftgate fleet retention                   # show retention windows
shiftgate fleet retention --audit-days 90 --checkpoint-days 30
shiftgate fleet sso                         # show SSO enforcement state
shiftgate fleet sso --enforce example.com   # claim a domain for single sign-on
shiftgate fleet sso --disable               # release the claim
shiftgate fleet entitlement
shiftgate fleet usage --from 2026-08-01 --to 2026-08-28
shiftgate storage                           # hosted storage status + agent setup lines
shiftgate storage --org ORG_ID              # same, for another organization
```

`shiftgate machines capability NAME` is a fleet query through the capability
projection the control plane maintains alongside each machine's JSONB
document: `--kind number|text|boolean` selects the comparison (numbers also
take `--minimum`), and only scalar capabilities are queryable — objects and
arrays stay in the document. Retention windows are in days; an omitted flag
leaves its window unchanged, and `0` is the deliberate keep-forever policy.
The control plane enforces the windows on a background sweep (configurable
via `retention_sweep_interval`, default hourly): audit rows past their window
are deleted, checkpoints past theirs are marked deleted while their lineage
rows survive, and storage objects already soft-deleted are purged after their
grace period.

`shiftgate fleet sso` manages single sign-on enforcement (admin role; the control
plane must have an OIDC issuer configured). Enforcing SSO claims an email
domain: the domain's accounts then authenticate with `shiftgate login --sso`,
password login and self-service registration are refused for them, and a
federated login automatically joins the organization as a viewer. One
organization can claim a domain at a time, and disabling releases the claim
rather than parking it.

`shiftgate storage` shows the organization's platform-hosted checkpoint
storage: reconciled usage against the plan limit, the bucket location, and —
when hosting is enabled — the exact environment lines to put on an agent for
hosted mirroring (`SHIFT_OBJECTSTORE_BACKEND=control-plane` and the
control-plane reporter settings; the agent fetches its own short-lived
credentials, so no storage secret is printed or needed). When the control
plane does not host storage it says so and points at the agent-side S3
mirroring configuration instead.

## Marketplace

Compute offers, placements, and reservations:

```sh
shiftgate marketplace inventory                     # everything schedulable now
shiftgate marketplace offers                        # this organization's offers
shiftgate marketplace publish --machine workstation --cpu 8 --memory 32GiB \
    --region eu-central --country DE --visibility organization
shiftgate marketplace withdraw --offer OFFER_ID
shiftgate marketplace place --workload demo --cpu 4 --memory 8GiB --checkpoint CHECKPOINT_ID
shiftgate marketplace reserve --offer OFFER_ID --workload demo --cpu 4 --memory 8GiB
shiftgate marketplace reservations
shiftgate marketplace commit --reservation RESERVATION_ID    # migration landed
shiftgate marketplace release --reservation RESERVATION_ID   # capacity returned
shiftgate marketplace fail --reservation RESERVATION_ID --reason "restore failed"
```

A placement ranks destinations and explains every rejection without holding
anything; a reservation runs the real scheduler server-side inside the store's
locked transaction, so a hold can never be granted on terms a placement would
refuse, and it expires (or is released) rather than pinning capacity forever.

The `shiftgate update` subcommands manage this machine's agent updates through the local API:
`status` shows the running and pending versions, available and refused releases, preserved
backups, blocked versions, and history; `check` consults the release feed immediately;
`apply` installs the current selection or one named version; `rollback` reinstalls a
preserved binary and blocks the version it undid; `block` and `unblock` manage the local
block list. A release is only installed after its signatures verify against the configured
key ring and the staged binary passes the `--version` self-check, and an install that
would interrupt a migration, restore, fork, or incoming transfer exits 6 to be retried
later.
