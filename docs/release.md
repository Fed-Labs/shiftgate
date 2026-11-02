# Release Process

How a SHIFT release is cut, signed, published, and rolled out — and how the
update system verifies what it installs. The agent's update machinery is the
consumer of everything this document produces; the pipeline that runs it is
`.github/workflows/release.yml`, and every step below is a step that workflow
performs (the manual commands are shown so a release can also be cut from a
clean workstation).

## Versioning

Releases are `vMAJOR.MINOR.PATCH`. The agent, CLI, control plane, and
desktop client in one repository release together; the agent reports its
version to peers and the control plane, and the update system refuses a
release whose staged binary answers `--version` with anything other than the
version the release document signed.

The on-disk state format is versioned (`state_format` in each manifest) and
frozen at exactly `1.0.0` today ([versioning](versioning.md#state-format-compatibility)):
the checkpoint loader requires an exact match and refuses any other format
version with a clean "unsupported checkpoint state format" error — never a
guess, never a corrupt restore. A release that bumps the state-format version
is a declared breaking release: it must be labeled as such in the release
notes and cannot land on a PATCH/MINOR-preserving train. Within the freeze,
restore compatibility across releases follows the N-1 rule below. Either way,
mixed-version fleets remain a deliberate upgrade-ordering problem: **upgrade
destinations before sources** — an upgraded source can write state (for
example newer compression) that a previous-release destination cannot
restore, which fails as a clean refusal, never a corrupt restore. The agent-level compatibility promise is N-1: a
release restores state written by the immediately previous release (and its
own), but a release train is not a matrix — upgrade a fleet to one version
before cutting the next, and run `shiftgate doctor` on every machine between
trains.

## 1. Cut the release

```bash
git checkout main && git pull
git tag v1.2.3
git push origin v1.2.3
```

CI (`.github/workflows/ci.yml`) must be green on the tag, including the
end-to-end and stress lanes. A release cut from a red build is not a
release; the e2e lane failing because "nothing ran" is a red build. Pushing
the tag starts two tag-triggered workflows: `release` (below) and `images`
(container images to GHCR for the Helm chart) — and nothing else
(`ci.yml` runs on branches and pull requests only).

## 2. Build and generate the release document

```bash
make dist   # binaries, dist/shiftgate-linux-amd64.tar.gz + .sha256 + .txt
go run ./cmd/shift-release document \
  --version 1.2.3 \
  --channel stable \
  --base-url "https://github.com/Fed-Labs/shiftgate/releases/download/v1.2.3" \
  --artifact agent=bin/shift-agent \
  --artifact cli=bin/shiftgate \
  --artifact control=bin/shift-control \
  --artifact gc=bin/shift-gc \
  --artifact verify=bin/shift-verify \
  --artifact relay=bin/shift-relay \
  --out release-document.json
```

The release document is **generated from the build outputs**, never written
by hand: `document` records each binary's SHA-256 and size (the digests it
prints are the digests it computed from the bytes on disk), pins the
protocol/config/state schema versions this build speaks, and points each
artifact at the URL it will be published under. Optional rollout controls
(`--minimum-from`, `--mandatory`, rollout stages) are part of the signed
document too.

The release document is not committed to the repository; the pipeline
generates it from the tagged build every time, so a checked-in document can
never drift from the binaries it describes.

## 3. Sign

Release signing keys are Ed25519. A signing key never leaves the machine
that generated it. `keygen` writes the pair; `trusted-key` prints the entry
— a one-element JSON **array** — that a trusted-keys file carries for the
public key.

```bash
go run ./cmd/shift-release keygen --private-key release.key --public-key release.pub
go run ./cmd/shift-release trusted-key --public-key release.pub --comment "release-2026-10"
go run ./cmd/shift-release sign --key release.key --release release-document.json --out signed-release.json
```

A fleet can require more than one signature (the `signature_threshold`
agent setting): run `sign` once per key and merge the signatures into the
signed release. Threshold enforcement happens on every agent that installs
it.

### Key ceremony

The signing key is generated once, offline, and split between two places:

1. `shift-release keygen` on a trusted workstation (never a CI runner).
2. The **private key** goes into the GitHub `release` environment as the
   `SHIFT_RELEASE_SIGNING_KEY` secret, with required reviewers on the
   environment — the humans who approve a deployment to `release` are the
   humans who authorize the key's use. It exists nowhere else: not in the
   repository, not in a workflow artifact, not in a log.
3. The **public key** replaces the placeholder entry in
   `deployments/release-keys.json` — a JSON array of `trusted-key` entries —
   as a deliberate, reviewed commit. That file is the pipeline's pinned
   trust anchor: `shift-release keys-status` reports `configured` once it
   holds a real entry (any comment containing `PLACEHOLDER` marks ceremony
   scaffolding and is never a trust anchor).
4. Each fleet pins the same public key in its agents' `trusted_keys_file`
   (`/etc/shift/release-keys.json` on the machines) — the fleet's copy is
   what installers trust; the repository copy is what the release pipeline
   verifies against. Keep them in step.

Rotation is the same ceremony with a new key: add the new entry first (a
ring may hold several keys), ship a release signed by it, then revoke the old
entry (`"revoked": true`) in a follow-up commit. Never re-key per release.

While `deployments/release-keys.json` still holds only placeholders, the
pipeline signs and verifies with an ephemeral dev key and says so loudly —
that keeps the sign→verify path exercisable before the ceremony, and no
fleet that pins real keys will accept such a release. Once the ceremony has
run, the verify gate is authoritative and fails closed on any mismatch.

**Launch note (as of v0.2.0):** the ceremony has not run —
`deployments/release-keys.json` still holds PLACEHOLDER entries. Until the
real public key lands, agent-driven updates (`shiftgate update`,
`updates.enabled` with the checked-in key ring) fail closed for everyone:
that is deliberate, and the feature is effectively inert. Either run the
ceremony before enabling update messaging, or treat `update` as a
preview-only surface for the launch build.

## 4. Assemble, verify, and bind the digests

```bash
go run ./cmd/shift-release feed --channel stable --out feed.json signed-release.json
go run ./cmd/shift-release verify --feed feed.json --keys deployments/release-keys.json \
  --artifacts bin --tarball dist/shiftgate-linux-amd64.tar.gz
```

`verify` checks the feed against the trusted-key JSON array exactly the way
an installing agent will — and then **binds the signed digests to the bytes
that ship**: every artifact digest (all six shipped binaries) is re-derived
from the build outputs (`--artifacts`) and from inside the release tarball
(`--tarball`), and the tarball itself is checked against its published
`.sha256` sidecar. A green verify therefore means the signed digests match
the tarball installers will download, not merely that some signature is
mathematically valid. The pipeline additionally asserts the binaries
self-report exactly the tagged version (a mismatch makes every auto-update
fail closed) and that the tarball contains exactly those six members. Run it as
a separate step so its exit code is impossible to miss; in the pipeline it
is the mandatory gate between signing and publishing.

## 5. Publish

The pipeline's `publish` job (gated on `verify`) creates the GitHub Release
for the tag and uploads exactly what the install paths fetch:

| Asset | Consumed by |
|---|---|
| `shiftgate-linux-amd64.tar.gz` + `.tar.gz.sha256` | `deployments/dist/install.sh` (`curl \| sh`) |
| `install.sh` + `install.sh.sha256` | the one-command installer and the sidecar its bootstrap verifies |
| `shiftgate-linux-amd64.txt` | humans / release notes |
| `shiftgate`, `shift-agent`, `shift-control`, `shift-gc`, `shift-verify`, `shift-relay` | the signed artifact URLs (each recorded and digest-bound in the release document) |
| `sbom.json` | the software bill of materials |
| `release-document.json`, `signed-release.json`, `feed.json` | update feeds / audit |

For a self-hosted feed: upload the feed and binaries to the HTTPS location
the fleet's `updates.feed_url` points at. Everything the agents fetch — feed
and binaries — must be served over TLS from the configured origin. The
update system does not trust the transport for integrity (it verifies
signatures and digests itself), but the feed location is fixed in
configuration, so serving it from the origin you configured is part of the
contract.

## 6. Rollout

Updates follow each machine's policy:

- `mandatory`: agents install within their check interval.
- `staged` with rollout controls: each machine checks whether it is inside
  the released percentage and its local time is past the earliest start.
- A machine running a migration, restore, fork, or incoming transfer defers
  the swap until the operation finishes.

Watch rollout through the control plane's machine inventory (agent version
per machine) and the agents' `/v1/updates` status.

## 7. If something goes wrong

`shiftgate update rollback` reinstalls the preserved previous binary (three
backups are kept by default) with no network access, and the version that
was undone is blocked from reinstalling. `shiftgate update unblock VERSION`
re-arms it deliberately. A release whose install failed is blocked the same
way — the block list is how the fleet remembers a bad release.

For a release that must be withdrawn entirely: remove it from the feed and
push a new signed feed. Machines that already installed it need
`shiftgate update rollback` or a fixed release on top; a withdrawn release does
not uninstall itself.

## Provenance: what is signed and what is only integrity-checked

- The update feed is signed: release documents are Ed25519-signed, each
  binary's SHA-256 is pinned inside the signed document, and every installing
  agent verifies both against its configured trusted-keys file (optionally to a
  `signature_threshold`). The release pipeline additionally binds those
  digests to the built bytes and the release tarball before publishing
  (`verify --artifacts/--tarball`). A fleet pins trust by holding its own
  `trusted-keys.json` across releases and running `shift-release verify` with
  those keys before publication — the keys are the pin; rotate them
  deliberately, not per release.
- The `curl | bash` release installer verifies the downloaded artifact against
  its published SHA-256 over TLS. That is a build-output integrity check
  against the published digest — not signature-based provenance and not source
  verification.
- A from-source install builds the checkout it is given and verifies only its
  own build output; checkout provenance (commit or tag pinning) is the
  operator's practice, not something the installer enforces.
- No claim beyond that: no build attestations, no reproducible-build
  guarantee, no third-party signing. Channels that require those must obtain
  them outside this repository.

## What we never do

- Ship an unsigned or below-threshold release and raise the threshold after
  the fact — agents verify at install time.
- Re-sign the same version with different content: digests are pinned in
  the release document, and a mismatch refuses the install.
- Publish a feed that has not passed `shift-release verify` with the same
  public keys the fleet configures.
- Hand-write a release document: it is generated from the build outputs, and
  the verify gate re-derives its digests from the shipped bytes.
