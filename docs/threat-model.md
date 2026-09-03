# SHIFT Threat Model

This document walks the ten threat scenarios the security model was designed against and
states, for each one, what the implementation does and what remains exposed. It is a map of
the code as it is, not as it might become; where a mitigation depends on operator actions,
that is stated. The mechanisms referenced here live in `internal/identity` (machine
identity), `internal/securestore` (keys), `internal/chunkstore` (encrypted chunks),
`internal/checkpoint` (signed manifests, restore validation), `internal/transfer`
(peer protocol), `internal/agent` (local API), `internal/controlplane` (authn/authz,
audit), and `internal/update` (signed releases).

Cross-cutting properties:

- Machine identity is an Ed25519 keypair generated on the agent host; its public key is
  self-certifying (the machine id is derived from the public key), and it signs every
  checkpoint manifest.
- Checkpoint chunks are AES-256-GCM ciphertext with authenticated associated data; digests
  are keyed, so plaintext content hashes never appear in metadata or the control plane.
- Peer traffic is TLS 1.3 with mutual certificate verification; the authenticated peer
  identity (certificate-derived machine id) is bound into every transfer session.
- The control plane stores metadata only: workload records, checkpoint manifests (already
  public-safe), entitlements, audit events. It never receives workload keys or chunk
  plaintext.

## 1. Malicious destination

A machine that receives a migration must, by design, be able to restore the workload — so
it obtains the workload state it restores. What it cannot do:

- **Forge the source.** Every chunk upload is authenticated with the source's certificate;
  the session records the authenticated machine id, and a request whose declared source
  does not match the certificate fails (`SOURCE_IDENTITY_MISMATCH`). The destination cannot
  impersonate a different source to a third machine, because it holds no other machine's
  key.
- **Inject or substitute state.** The manifest is verified (Ed25519 signature over the
  canonical digest, signer identity matching the embedded public key) before the transfer
  proceeds, and every chunk must be referenced by that manifest
  (`CHUNK_NOT_AUTHORIZED`). A destination cannot smuggle extra chunks into the session or
  swap the workload image for its own, because restore validates each asset digest again
  before the process is created.
- **Exceed its reservation.** Sessions carry an estimated size enforced with a tolerance,
  request bodies are size-limited per chunk, and sessions expire.
- **Persist access.** The destination receives a workload key for the workloads migrated
  to it; it does not receive the source's master keys or other workloads' keys. Key
  versions are per-workload and rotate (`RotateWorkloadKey`).

Exposed: a malicious destination learns the plaintext of anything it restored. Treat
destination selection as a trust decision — the compatibility report and device policies
help evaluate fitness, but confidentiality against the destination is not achievable in
any migration system that restores there.

## 2. Compromised source

The source agent holds the plaintext of its own workloads and their keys — a compromised
source can read and exfiltrate what it already hosts. What it cannot do:

- **Move workloads somewhere unnoticed.** The destination authenticates the source's
  certificate and binds the session to it; manifests it sends are signed with its identity,
  so every checkpoint it produces is attributable to the machine id. A destination
  verifying manifests will reject images signed by a key that does not match the claimed
  source.
- **Corrupt a destination.** Chunk digests are checked on import and re-validated before
  restore; tampered ciphertext fails the GCM tag or the digest comparison, and the transfer
  aborts with `CHECKPOINT_CORRUPT` rather than restoring.
- **Masquerade as another machine to the control plane.** Presence reporting requires a
  scoped API key bound to the machine's organization.

Exposed: a compromised source can destroy or leak its own workloads' state, and can sign
new checkpoints as itself. Recovery is restoring from object-storage mirrors or backups —
mirrored manifests are verified before publication, so a mirror written by a healthy
period remains restorable.

## 3. Stolen credentials

- **Control-plane tokens.** Only HMAC digests of access and refresh tokens are stored;
  access tokens live 15 minutes, refresh tokens rotate transactionally on use (a replayed
  refresh token invalidates the session chain), and logout revokes the session. A stolen
  access token grants at most its TTL; a stolen refresh token is detected at rotation.
- **API keys.** Scoped (broad `read`/`operate`/`admin` or per-resource), bound to one
  organization, stored only as digests, revocable. A leaked key's blast radius is its scope.
- **Agent certificates.** Peer certificates live under the state directory (0600, state
  dir 0700). A stolen certificate plus key is machine identity theft: it can authenticate
  as that machine to peers. Mitigation is operational — issue certificates from a private
  CA, keep the CA offline, and re-issue the machine's identity when theft is suspected.
- **Stripe webhook secret.** Webhook signatures are HMAC-SHA256 with a five-minute
  timestamp tolerance; replayed events are deduplicated by event id (`stripe_events`), so
  a captured webhook cannot be replayed after the tolerance window or processed twice.
- **Object-storage credentials.** The backend only ever receives ciphertext chunks and
  encrypted manifest envelopes; stolen backend credentials expose ciphertext, not state.

## 4. Malicious checkpoint

A checkpoint presented to an agent (from a peer transfer, an object-store mirror, or an
operator-supplied archive) is verified before anything restores from it:

- Format and version are checked, refusing unknown formats.
- The manifest signature must verify against the embedded source public key, and that key
  must hash to the claimed machine id — a manifest cannot claim an identity its signer
  does not control.
- The manifest digest is recomputed over the canonical form, so any field tampering breaks
  verification.
- Every asset is digest-checked against the chunk store before restore; missing or
  substituted chunks abort.

A malicious checkpoint therefore fails closed — it is never restored. Exposed: a
well-formed, correctly signed checkpoint from a *compromised* machine is accepted (see
threat 2); signatures establish provenance, not intent.

## 5. Checkpoint tampering

- Chunks are AEAD-encrypted; any byte flipped in storage or transit fails the GCM tag on
  decrypt.
- Manifests are signed and digest-pinned; edited manifests fail signature verification.
- The transfer `verify` step re-validates every asset of the session before the
  destination commits, and restore re-validates again before the process is created —
  tampering between verification and restore is still caught at the last gate.
- Mirrors publish only verified manifests, and mirrored objects retain the same ciphertext
  integrity properties.

Exposed: nothing specific to SHIFT — tampering is detected, and the failure is the real
error. What SHIFT cannot do is prove a chunk was not *deleted* from object storage; keep
backend versioning or backups if deletion resistance matters.

## 6. Man-in-the-middle

- Peer migration traffic is TLS 1.3 with required and verified client certificates on both
  sides; the destination's certificate identifies the machine the source believes it is
  talking to, and the source's certificate is what the destination authenticates. A MITM
  without both certificates fails the handshake.
- Chunk ciphertext is additionally authenticated by the manifest digests, so even a
  successful transport-layer substitution is caught at verification.
- The control plane API is expected behind TLS terminated at a reverse proxy; tokens are
  bearer credentials, so the deployment must not expose plain HTTP.
- `insecure_development` mode relaxes peer verification on purpose, for development
  machines only; it is refused as a default and clearly flagged in configuration.

## 7. Malicious workload

A workload is a process tree the agent checkpoints, restores, and migrates. The boundary
is its configured root:

- A workload root must be explicitly configured and cannot be `/` or an unrestricted home
  directory; capture, restore, and fork operate within it.
- Restored workloads get reserved, conflict-checked ports; a malicious workload cannot
  pre-empt another workload's ports — the second reservation fails validation.
- Device policy decides GPU availability (`reject_incompatible` by default); device nodes
  in cloned roots reproduce only under root, and never blindly.
- Checkpointing runs with the privileges CRIU needs; a malicious workload cannot elevate
  through SHIFT beyond what its own process already had — but it *does* run with its own
  privileges, and the agent process itself is privileged. The residual risk is a workload
  exploiting the kernel or CRIU during dump/restore; that trust is inherent to any
  checkpoint/restore system and is stated rather than hidden.

## 8. Privilege escalation

- The control plane enforces viewer/operator/admin/owner roles per route, server-side, on
  every organization-scoped request; API keys additionally carry scopes that are checked
  against the route's requirement. A viewer session cannot mutate; a machine-scoped key
  cannot read audit logs.
- The agent's update endpoints (which replace the agent binary — equivalent to root on the
  host) are restricted to root and the agent's own user; releases must verify against the
  configured Ed25519 key ring and pass a `--version` self-check before the swap, and an
  apply is deferred while any migration, restore, fork, or transfer is in flight.
- Entitlements are enforced inside the database transaction that registers machines and
  checkpoints, so no client-supplied value can bypass a limit.

Exposed: the local agent API accepts any caller that can reach its Unix socket and passes
`SO_PEERCRED` (socket mode 0660 — the socket's group is the effective gate for workload
operations). Membership in that group is deliberately the administration boundary;
workload operations are powerful (they can stop and delete workloads) and operators must
treat socket-group membership accordingly.

## 9. Compromised control plane

The control plane is metadata-only. A fully compromised control plane:

- **Cannot** read workload state — it never holds workload keys or chunk plaintext, and
  checkpoints reach it as already-safe manifests.
- **Cannot** mint machine identities or forge manifests — those keys never leave agents.
- **Can** read and edit metadata: organizations, memberships, workload records,
  entitlements, audit trails. It can create sessions and API keys and thus act as any
  user.
- **Can** dispatch commands to enrolled agents (`/machines/{id}/commands`) when agents are
  reachable from it. In production the agent's TCP listener demands a client certificate
  the control plane does not hold, and the local listener is a Unix socket, so dispatch
  requires either co-location or an explicitly configured insecure deployment — both are
  operator decisions that should be made deliberately.

Mitigations: keep PostgreSQL and the control plane on a private network, monitor audit
logs for anomalous sessions and key creation, and scope what the control plane can reach
at the network layer.

## 10. Malicious local user

- Agent state (master keys, workload keys, identity) is 0600 inside a 0700 state directory;
  another user cannot read keys, checkpoints, or manifests.
- The local API requires Unix-socket peer credentials; in production a TCP listener
  without TLS is refused. Socket-group membership (0660) is the boundary for workload
  operations, and root or the agent's own user for update operations (see threat 8).
- The control plane CLI stores tokens in the user's runtime state with restrictive
  permissions; a malicious user with the victim's UID is, as always, the victim.

## Reading this model alongside the code

Every claim above traces to a mechanism: signature verification
(`internal/checkpoint/repository.go`), AEAD chunk storage (`internal/chunkstore`),
peer authentication and session binding (`internal/transfer`), peer credentials
(`internal/agent/credentials.go`), token hashing and rotation (`internal/controlplane`),
update trust (`internal/update/trust.go`). Where this document says something is exposed,
no code claims otherwise — the implementation reports real errors instead of simulating
success, and the limitations document (`limitations.md`) carries the same boundaries.
