# Positioning

Internal positioning pack: who SHIFT is for, how it is framed against the
alternatives, the one promise everything else hangs on, and the open commercial
decisions that are not yet made. This is a recommendation record, not published
marketing — where a decision is open, it says so. Everything here is checkable
against the implementation; the claims bar is the same as the rest of the docs
(never claim behavior the code does not have).

## Ideal customer profile (ICP)

Primary: **Linux fleet operators** and **ML & CI teams** who move long-running,
stateful workloads between machines they already own — rebalancing a GPU node,
evacuating a host for maintenance, cloning a warmed environment for a burst of
CI workers, standing a workload up on spare hardware.

What they have in common:

- The workload is stateful enough that "just restart it elsewhere" costs real
  time or work: large heaps, warm caches, long build/test cycles, model state.
- They own both the source and the destination — the destination is not a
  cloud they are migrating *to*, it is another machine in their estate.
- They are Linux-first (SHIFT's only supported platform), comfortable with
  root/privileged agents, and willing to run `criu check` as part of provisioning.

Explicitly not the ICP (today): Windows/macOS estates, unprivileged multi-tenant
clouds, live VM evacuation with shared storage, anyone needing transparent
socket migration without application cooperation (see [limitations](limitations.md)).

## Competitive frame

| What they do today | What it costs | SHIFT's frame |
|---|---|---|
| Checkpoint scripts + `scp`/rsync (hand-rolled CRIU or app dumps) | Fragile ordering, no integrity story, source kept alive by luck; every app reimplements freeze/copy/restore | One state machine with persisted stages, content-addressed encrypted chunks (dedup transfer), and a commit gate: the move is only committed after destination health validation |
| Restart-and-rebuild (redeploy, re-warm, re-attach) | Minutes to hours of warm-up; lost in-memory state; re-run of expensive work | State moves as-is: a warm checkpoint restores in seconds (see [benchmarks](benchmarks.md)), and `clone` turns one warm checkpoint into many |
| VM migration (live migration / evacuation) | Moves the whole machine — OS, cruft, licenses; needs shared storage or hypervisor cooperation; heavyweight per move | Moves the *workload*: explicitly-scoped filesystem state plus the process image, agent-to-agent over TLS, no shared storage, no hypervisor |

Honest edges against those alternatives: SHIFT is Linux x86_64 only, pre-1.0,
requires a privileged agent and a CRIU-capable kernel, freezes the workload for
the final delta of a live move (pre-dump passes reduce, never eliminate, the
frozen window), and does not transparently carry TCP socket state without CRIU
TCP repair. VM migration keeps the whole machine identity; SHIFT reconstructs
the workload on a destination that may be a different distro/kernel (subject to
the compatibility report).

## Core promise

> **State moves, and a failed move never destroys the source.**

Every mechanism serves that sentence: staged, persisted migration state; source
termination only legal after destination restore and health validation; failure
at any stage rolls back and preserves the source (running, in live mode, until
the freeze). Marketing must not outrun it — a live move is not "zero
downtime", it is a freeze measured and reported as `downtime`.

## Brand (recommendation)

- Product: **SHIFT**. CLI: **`shiftgate`** (the bare name `shift` is a POSIX
  shell builtin, so a binary named `shift` can never be typed — the CLI is
  named after the gate it opens).
- Recommendation: use "SHIFT" for the product and "shiftgate" for the CLI
  everywhere; the repo/org slug `Fed-Labs/shiftgate` is historical and stays.
- Alternatives considered: "SHIFTGATE" as the product name (rejected: the
  product is the platform, the gate is the client); renaming the CLI to
  `shift` via an alias (rejected: the shell-builtin collision is why the name
  exists).
- Surface chrome drift ("SHIFTGATE" strings in apps) is being aligned to this
  recommendation by the owning agents; `internal/billing/plans.go` is aligned,
  and this doc records the target state.

## Pricing principle

**Price the fleet, not the bytes.** The differentiators worth charging for are
fleet capabilities — search and inventory, checkpoint policy, audit history,
SSO/SCIM, warm-standby failover, managed migrations, hosted storage — not
per-GB transfer. Rationale: transfer bytes are the part customers can (and
should) route themselves; the fleet is what keeps an operator on the platform.
The current catalog follows this in spirit (hosted-storage quotas, live-mode
tiering) while metering is storage-only today; bandwidth metering exists in the
usage tables but is not priced.

## Churn risks (honest)

- **Self-sufficiency churn** (04-business m-7): the core value — peer-to-peer
  `shiftgate migrate` — needs no platform after setup. An operator who only
  moves workloads between two machines they own can churn and keep most of the
  value. Mitigation direction: make fleet features (policy, history, failover,
  audit) the retention surface, not the transfer itself.
- **Pre-1.0 churn**: breaking MINOR bumps before 1.0 ([versioning](versioning.md))
  can break automation. Mitigation: announce every breaking change; keep the
  N-1 window and rollback path honest.
- **Support-expectation churn**: no published SLA; enterprise response times
  exist only under a signed agreement ([support policy](legal/support-policy.md)).
  Overselling support here converts directly to churn.
- **Platform-scope churn**: Linux x86_64 only. A customer whose estate grows a
  second platform hits the wall early.

## Decision: P2P `--mode live` is entitled (resolved)

The paid differentiator "live migration" must not be free on the free tier on
any path a customer's hands touch. The platform-mediated gate (a live-mode
migration job or dashboard dispatch is refused on a cold-only plan with
`LIVE_MIGRATION_PLAN_REQUIRED`) was always real; the open question was the
peer-to-peer `shiftgate migrate --mode live`, which never touches the control
plane. **Decision: enforce the entitlement on the agent/CLI side too.** The
CLI checks the organization's entitlement before a live move is admitted, and
the agent's local API refuses a live-mode create its control-plane identity is
not entitled to — one shared rule (`billing.LiveMigrationAllowed`) behind every
admission point. The three options considered:

1. **Keep P2P live free.** Trust-building, honest, zero license complexity in
   the agent; the paid surface is strictly the fleet. Rejected: the headline
   differentiator would be free for anyone with two agents, undercutting the
   upgrade story.
2. **License-gate live mode in the agent** (entitlement enforced at the
   admission points). Chosen, in its soft form: the enforcement is a licensing
   boundary at the product surface, not DRM. Offline checks are either soft
   (bypassable by an operator who edits an open-source agent) or hostile
   (revocation, clock tricks); we chose soft and said so, because the goal is
   that the shipped product never disagrees with the pricing table — not that
   a determined root user be physically incapable of running CRIU.
3. **Middle path**: keep P2P transfer free and bind only the orchestration of
   live moves to paid tiers. Rejected: a bypass that is merely "less valuable"
   is still a gap a buyer finds by comparing the pricing table to the CLI.

Semantics in full (including the detached-agent and dry-run cases) are
documented in [limitations.md](limitations.md); the pricing surface must say
"live migration is a paid plan feature, including peer-to-peer moves" wherever
it lists the feature.
