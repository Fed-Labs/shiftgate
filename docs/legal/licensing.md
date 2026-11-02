# SHIFT licensing map

> Everything that runs on your machines is Apache-2.0. Everything that
> coordinates a fleet is source-available (Elastic License 2.0). The hosted
> service is the commercial product.

This page is the authoritative map. Where a per-directory license file exists,
it governs that directory; where one does not, the root
[LICENSE](../../LICENSE) (Apache-2.0) and [NOTICE](../../NOTICE) govern, except
for the directories listed under the Elastic License 2.0 below.

## A. Apache-2.0, forever (the distribution core)

| Component | Path | Why open |
|---|---|---|
| `shiftgate` CLI (local + fleet client) | `cmd/shift/` | the typed surface; must be in every package manager and every quick start |
| `shift-agent` daemon | `cmd/shift-agent/` and its `internal/*` runtime packages (agent, checkpoint, chunkstore, transfer, migration, filesystem, device, network, runtime, persistence, securestore, gc, compatibility, config, endpoint, fencing, platform, observability, termio, update, model, supportmatrix, controlclient, objectstore) | the trust anchor; operators must audit a root daemon before install |
| `shift-gc` / `shift-verify` | `cmd/shift-gc/`, `cmd/shift-verify/` | maintenance + contract-check tools; zero marginal ARR, pure trust |
| K8s & Slurm integrations | `integrations/` | package ecosystems accept only OSI-licensed operators; these are pure distribution |
| Desktop client | `apps/desktop/` | client-side; same argument as the CLI |
| Installers & agent deployments | `install.sh`, `deployments/dist/`, `deployments/systemd/` | one-command install is the top of every funnel |
| API contract + docs | `api/openapi/shift.yaml`, `docs/` | interop is fine; docs are the product surface |
| Tests, release tooling | `tests/`, `cmd/shift-release/` | credibility |

## B. Elastic License 2.0, source-available (the fleet plane)

| Component | Path | Why not permissive |
|---|---|---|
| `shift-control` (fleet control plane) | `cmd/shift-control/`, `internal/controlplane/`, `internal/database/`, `internal/billing/`, `internal/identity/`, `internal/scheduler/` | this is what a competitor would need to run a rival fleet service; ELv2 forbids offering it as a managed service |
| `shift-relay` (authenticated outbound relay) | `cmd/shift-relay/`, `internal/relay/` | NAT-traversal mesh is fleet infrastructure, not machine-level |
| Web dashboard (fleet views) | `apps/web/` | the control plane's face; ships with the ELv2 stack |
| Control-plane deployments | `deployments/docker/Dockerfile.control`, `deployments/docker/Dockerfile.relay`, control-plane values in `deployments/helm/` | distributed alongside the ELv2 binaries |

Elastic License 2.0 in one line: use it, modify it, run it internally — but do
not offer it as a managed service to third parties, and do not circumvent
license-key functionality. The full text lives in `LICENSE-ELv2` at the
repository root. Contributions to a directory under ELv2 are accepted under
ELv2 (see CONTRIBUTING.md / DCO); everything else is Apache-2.0.

## C. Closed / hosted-operated (source never published)

The operated service itself: hosted control plane, hosted checkpoint
object-store credential broker, release-signing operations, compliance evidence
tooling, and future fleet analytics / policy packs / managed relay regions
add-ons. Nothing in today's repository moves to C — the line exists so future
commercial work lands in the right bucket instead of leaking into the Apache
tree.

## Entitlements are not licensing

Live migration (`--mode live`, peer-to-peer included) is a **plan feature inside
the Apache agent/CLI** (`LIVE_MIGRATION_PLAN_REQUIRED`), not a license change.
The free tier is cold-only on every path a customer's hands touch; the shipped
product never disagrees with the pricing table. A determined root user can
patch a fork — accepted and documented; the buyers we care about are
organizations that need the license to be clean.

## FAQ

**Can I self-host the control plane?**
Yes, under an Enterprise agreement — ELv2 permits internal use, not offering it
as a managed service. "Self-hosted control plane" is a licensed conversation,
not a free-tier feature.

**Can I embed the agent in my product?**
Yes — the agent and CLI are Apache-2.0, including in commercial products,
subject to the Apache terms (NOTICE, patent grant included).

**Which license do my contributions land under?**
The license of the directory you contribute to. DCO sign-off applies either
way; the map above is the table of record.

**What about the hosted service?**
It is the commercial product: the same fleet plane, operated by us, under the
subscription terms of service.

**Is this open source?**
The distribution core is OSI open source (Apache-2.0). The fleet plane is
source-available (ELv2) — honest wording matters to this audience, so we use
"open core" precisely: Apache core, source-available fleet plane, commercial
hosted service.
