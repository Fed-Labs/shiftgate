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

Control-plane password hashes use Argon2id and a separate pepper. Access and refresh token
digests, never bearer values, are persisted. Rotate peppers by deploying a migration that
re-hashes credentials and invalidates sessions; do not place peppers in source control.
The OIDC client secret is used only at the issuer's token endpoint as HTTP Basic
authentication, never in a form body or a redirect, and is never logged.

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
