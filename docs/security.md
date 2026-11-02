# SHIFT Security Model

The threat-by-threat analysis — what each of the ten modeled adversaries (malicious
destination, compromised source, stolen credentials, malicious checkpoint, checkpoint
tampering, man-in-the-middle, malicious workload, privilege escalation, compromised
control plane, malicious local user) can and cannot do against a SHIFT deployment —
lives in [threat-model.md](threat-model.md). This page holds the trust boundaries,
stored-secret handling, and operator requirements.

## Trust boundaries

- The local agent is privileged and must be installed as a dedicated system service.
- The local API is reachable only through its Unix socket in production.
- Peer migration traffic uses TLS 1.3 mutual authentication and binds the transfer to the
  authenticated source machine identity.
- The control plane is untrusted with respect to computational state; it receives metadata.
- The OIDC issuer is trusted for authentication only: its ID tokens are verified
  cryptographically (RS256/ES256 against its JWKS, with issuer, audience, expiry, and
  nonce checks), and a verified email claim is required before any account is linked.

## Stored secrets

Agent master keys and encrypted workload keys live under the agent state directory with
0600 permissions. Checkpoint chunks use AES-256-GCM with authenticated associated data;
content addresses are keyed so plaintext hashes are not exposed. Manifests are signed with
the machine Ed25519 identity and stored locally as authenticated encrypted envelopes. When
object storage is enabled, the remote backend receives only encrypted chunk ciphertext and
the encrypted manifest envelope (plus object-key and size metadata); workload keys and
plaintext environment values remain on agents.

**At-rest confidentiality of the key hierarchy depends on the master-key keyfile.**
Without one, the agent stores master keys as plaintext base64 in
`master-keyring.json`, protected only by the state directory's permissions — a
copied, backed-up, or leaked state directory then exposes every workload data
key. Production deployments must set `SHIFT_MASTER_KEY_FILE` to a keyfile
stored *outside* the agent state directory (a root-owned 0600 file on a
separate secret path); master keys are then sealed under a key derived from
that file, so a leaked state directory alone yields only ciphertext. The
keyfile is the key boundary: back it up separately from the state directory,
and losing it makes existing checkpoints undecryptable. There is no warning or
refusal at startup for an unprotected keyring today — checking
`SHIFT_MASTER_KEY_FILE` is part of the production configuration
checklist.

Control-plane password hashes use Argon2id and a separate pepper. Access and refresh
tokens and API keys are opaque random values — no JSON Web Tokens are issued or
accepted anywhere in the system — and only token digests (keyed HMAC-SHA256 with the
token pepper), never bearer values, are persisted. Do not place peppers in
source control; rotation is described under
[Pepper rotation](#pepper-rotation) below.

### Pepper rotation

Password hashes are `Argon2id(HMAC(pepper, password))` — one-way in the
pepper — so stored hashes cannot be re-hashed from SQL under a new pepper,
and no dual-pepper verify-and-rehash-on-login path exists today. Rotation is
therefore asymmetric, and there is no migration that re-hashes credentials:

- **Token pepper (`SHIFT_TOKEN_PEPPER`)**: rotate by deploying the new value
  and restarting the control plane. Every session and API-key digest becomes
  unverifiable — a fleet-wide re-login (and API-key reissue), nothing worse.
- **Password pepper (`SHIFT_PASSWORD_PEPPER`)**: rotating it makes every
  stored password unverifiable. The only honest procedure today is a forced
  password reset for every user (out-of-band, with the usual account-
  verification flow) deployed together with the new pepper. Do not rotate it
  believing hashes will follow — every login fails until passwords are
  reset.

Treat both peppers as long-lived, back them up like the keys they are, and
keep this procedure next to your secret manager's runbook.
The OIDC client secret is used only at the issuer's token endpoint as HTTP Basic
authentication, never in a form body or a redirect, and is never logged.

## Cryptography and key management

The primitives are standard and off-the-shelf: HKDF-SHA256 for key derivation,
AES-256-GCM for chunk and envelope encryption, Ed25519 for machine identity and
manifest signatures, and TLS 1.3 for transport. No custom cryptography is
implemented anywhere in the system. Keys are generated and held by the agent in
its state directory (0600); master keys at rest are plaintext there unless
`SHIFT_MASTER_KEY_FILE` seals them (see Stored secrets). SHIFT has no
customer-managed key or external KMS integration — organizations that need one
must treat the agent state directory, its keyfile, and their backups as the
key boundary.

## Encryption export posture

The cryptographic implementation uses only standard, publicly available algorithms and
is therefore intended to fall under standard encryption-export exceptions (for example
License Exception ENC/TSU for mass-market availability); no dedicated export
self-classification has been filed. The product, primitives, and key-management
statement suitable for compliance questionnaires are recorded in
[docs/legal/export-classification.md](legal/export-classification.md). Operators
shipping appliances across borders should confirm their own classification — that note
is not legal advice.

## Single sign-on and provisioning

- SSO login is authorization code with PKCE S256 and a loopback-only redirect URI
  (RFC 8252): authorization codes never travel to a remote host, and the client secret
  stays server-side on the control plane.
- The OAuth `state` is an HMAC-signed blob (keyed by the token pepper) carrying the email,
  nonce, and a ten-minute expiry — no server-side session state, and a forgery is as hard
  as producing a session token.
- Federated accounts carry an empty password hash, which no password can verify; a domain
  under SSO enforcement refuses password login and self-service registration before any
  credential is checked, so the answer does not reveal whether the account exists.
- SCIM provisioning requires an API key with the `scim` scope. User session tokens cannot
  provision — not even an administrator's — so a stolen browser session never reaches the
  directory. SCIM filter attribute and operator names come from fixed whitelists and
  values bind as query parameters only.
- SCIM delete deactivates rather than destroys: the account's sessions are revoked in the
  same transaction, and the row survives so the audit trail keeps pointing at a real
  account.

## Operational requirements

Use a private CA for agent certificates, restrict peer firewall rules to known machines,
keep PostgreSQL on a private network, and back up encrypted agent state and PostgreSQL
separately. Audit events intentionally omit secrets and checkpoint contents.
