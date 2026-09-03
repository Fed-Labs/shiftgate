# Release Process

How a SHIFT release is cut, signed, published, and rolled out — and how the
update system verifies what it installs. The agent's update machinery is the
consumer of everything this document produces.

## Versioning

Releases are `vMAJOR.MINOR.PATCH`. The agent, CLI, control plane, and
desktop client in one repository release together; the agent reports its
version to peers and the control plane, and the update system refuses a
release whose staged binary answers `--version` with anything other than the
version the release document signed.

## 1. Cut the release

```bash
git checkout main && git pull
git tag v1.2.3
git push origin v1.2.3
```

CI (`.github/workflows/ci.yml`) must be green on the tag, including the
end-to-end and stress lanes. A release cut from a red build is not a
release; the e2e lane failing because "nothing ran" is a red build.

## 2. Build the release documents

For each target (linux-amd64 is the supported one):

```bash
go build -trimpath -o shift-agent-v1.2.3 ./cmd/shift-agent
go build -trimpath -o shift-v1.2.3 ./cmd/shift
go build -trimpath -o shift-control-v1.2.3 ./cmd/shift-control
```

The release document for the agent carries its SHA-256 digest, size, target
OS/architecture, a channel (stable or beta), and optional rollout controls
the update manager honors (percentage, earliest-start time).

## 3. Sign

Release signing keys are Ed25519. A signing key never leaves the machine
that generated it. The key pair is written by `keygen`; `trusted-key` prints
the entry an agent's trusted-keys file carries for the public key.

```bash
./bin/shift-release keygen --private-key release.key --public-key release.pub
./bin/shift-release trusted-key --public-key release.pub --comment "release-2026-08"
./bin/shift-release sign --key release.key --release shift-agent-v1.2.3.json --out signed/shift-agent-v1.2.3.signed.json
```

A fleet can require more than one signature (the `signature_threshold`
agent setting): run `sign` once per key and merge the signatures into the
release document. Threshold enforcement happens on every agent that
installs it.

## 4. Assemble and verify the feed

```bash
./bin/shift-release feed --out feed.json signed/shift-agent-v1.2.3.signed.json
./bin/shift-release verify --feed feed.json --keys trusted-keys.json
```

`verify` checks the feed against the trusted-key JSON array exactly the way
an installing agent will — this is the last gate before publication. Run it
as a separate step so its exit code is impossible to miss.

## 5. Publish

Upload the feed and binaries to the HTTPS location the fleet's
`updates.feed_url` points at. Everything the agents fetch — feed and
binaries — must be served over TLS from the configured origin. The update
system does not trust the transport for integrity (it verifies signatures
and digests itself), but the feed location is fixed in configuration, so
serving it from the origin you configured is part of the contract.

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

`shift update rollback` reinstalls the preserved previous binary (three
backups are kept by default) with no network access, and the version that
was undone is blocked from reinstalling. `shift update unblock VERSION`
re-arms it deliberately. A release whose install failed is blocked the same
way — the block list is how the fleet remembers a bad release.

For a release that must be withdrawn entirely: remove it from the feed and
push a new signed feed. Machines that already installed it need
`shift update rollback` or a fixed release on top; a withdrawn release does
not uninstall itself.

## What we never do

- Ship an unsigned or below-threshold release and raise the threshold after
  the fact — agents verify at install time.
- Re-sign the same version with different content: digests are pinned in
  the release document, and a mismatch refuses the install.
- Publish a feed that has not passed `shift-release verify` with the same
  public keys the fleet configures.
