# Versioning and Stability Promise

Current release: **`v0.2.0` · pre-1.0**. This document is the promise that label
stands behind: what is stable today, what may change before 1.0, how on-disk state
stays compatible across versions, and how long a release is supported. It is
binding on the project's behavior, not marketing copy.

## What `v0.2.0 · pre-1.0` means

- The product is past prototype but before its 1.0 stability freeze. It is
  usable and tested (see [benchmarks](benchmarks.md) and the real-CRIU
  end-to-end suite in [development](development.md)), but interfaces may still
  change.
- Releases are `vMAJOR.MINOR.PATCH` (see [release](release.md)); agent, CLI,
  control plane, and desktop client in one repository release together and
  report one version.

## Semver policy before 1.0

- **PATCH** (`v0.2.0` → `v0.2.1`): compatible fixes only. Safe to upgrade.
- **MINOR** (`v0.2.x` → `v0.3.0`): may contain breaking changes to the HTTP API
  contract, the CLI surface, and configuration schemas. Breaking changes are
  called out in the release notes; nothing is removed silently.
- **MAJOR** at 1.0: the API contract ([OpenAPI](../api/openapi/shift.yaml)) and
  CLI become a stable surface; breaking changes then require a major bump and
  follow the deprecation window below.
- A pre-1.0 MINOR bump is the *declared* breaking boundary. Within a MINOR line
  nothing breaks; across a MINOR line, read the release notes before upgrading
  automation.

## State-format compatibility

On-disk checkpoint state carries an explicit format and version
(`state_format` in each manifest; the current value is `shift-state` `1.0.0`).
**Today that version is frozen at exactly `1.0.0`**: the checkpoint loader
requires a manifest whose `state_format` matches the writing build's constant
exactly, and refuses anything else with `unsupported checkpoint state format`
— on every load, every save re-verify, and every peer/mirror import. A release
that bumps the format version is therefore a **declared hard break**: state
written before the bump is refused (a clean refusal that preserves the source,
never a partial or corrupt restore), and the release notes will say so. Within
the freeze — every `v0.2.x` build — all checkpoints are mutually restorable.

A range-based compatibility engine exists (`internal/compatibility`) and
defines the precise `STATE_FORMAT_UPGRADE_REQUIRED` / `STATE_FORMAT_UNSUPPORTED`
refusals for the `1.x` line (and drives migration compatibility reports
today); the checkpoint loader does not yet consult it — it enforces the exact
match above. When the loader moves to the range rule, this page will describe
the full window it opens; until then the binding promise is the freeze.

Chunk codecs on the line are self-identifying (zstd now, gzip for older
chunks, selected from the frame's magic bytes) — see
[limitations](limitations.md). Because a newer source can write state an older
destination cannot read, **upgrade destinations before sources**.

## Agent release compatibility and the upgrade window

The release-level promise is **N-1** ([release](release.md)): a release restores
state written by the immediately previous release (and its own) — true for as
long as the state-format freeze above holds, which the release checklist
enforces (a format bump is a breaking MINOR, with an explicit note). A release
train is not a compatibility matrix — bring a fleet to one version before
cutting the next, and run `shiftgate doctor` on every machine between trains.

Concretely, for a fleet on `vN`:

- `vN` agents interoperate with `vN-1` agents; `vN-2` is out of the window and
  must be upgraded before mixed operation is attempted.
- The update system ([release](release.md)) stages, verifies, and swaps binaries
  at safe points, keeps three backups, and supports `shiftgate update rollback`
  plus a block list — an upgrade window is a rollback window too.

## Deprecation policy

- Anything documented as deprecated keeps working for **at least the N-1
  window** (one release) before removal, and its replacement ships in the same
  release that deprecates it.
- Deprecations and removals are announced in the release notes of the release
  that starts them. Pre-1.0, a breaking change may land in a MINOR bump
  (above), but it is still announced, never silent.
- The on-disk state format is held to a stricter bar than the API: state
  compatibility across the `1.x` line is guaranteed as described above, so a
  CLI or API deprecation can never brick existing checkpoints.

## What is not promised yet

- API/CLI stability before 1.0 (that is what "pre-1.0" is for).
- Cross-major state-format compatibility (2.x restoring 1.x) — undecided until
  there is a 2.x; the range rule within a major line is the promise.
- Support response times — see [support policy](legal/support-policy.md);
  there is no published SLA outside a signed agreement.
