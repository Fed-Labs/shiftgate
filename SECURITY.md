# Security Policy

SHIFT moves live process state between machines. Security reports are treated
as production incidents: this document is the disclosure contract the project
holds itself to.

## Reporting a vulnerability

Report suspected vulnerabilities **privately** through GitHub Security
Advisories for [Fed-Labs/shiftgate](https://github.com/Fed-Labs/shiftgate/security/advisories/new).
Do not open a public issue for a suspected vulnerability, and do not include
exploit details, keys, or workload plaintext in any public channel.

If GitHub private reporting is unavailable to you, contact the maintainers
through the repository's `security` contact on the organization profile and
request a private channel; do not send the report body until one is confirmed.

Include, when possible:

- the affected component (`shift-agent`, `shift-control`, control plane,
  checkpoint format, `internal/securestore`, …) and version or commit;
- a reproduction or proof of concept;
- the impact you believe it has (confidentiality of checkpoints/keys,
  integrity of migrated state, availability of the control plane, …).

## Response expectations

| Step | Target |
|---|---|
| Acknowledgement of a report | within 3 business days |
| Initial severity assessment | within 7 days of acknowledgement |
| Fix or mitigation for critical issues | within 30 days of confirmation |
| Fix or mitigation for high issues | within 90 days of confirmation |
| Public advisory + credit | coordinated with the fix release |

These are targets for the open-source project, not a paid support SLA. Paid
support response times are defined in [docs/legal/support-policy.md](docs/legal/support-policy.md)
and in enterprise contracts.

## Scope

In scope: the Go services and CLI in this repository, the checkpoint/chunk
cryptography in `internal/securestore` and `internal/checkpoint`, the control
plane's authentication and authorization paths, the Stripe webhook handling,
and the release-signing tooling (`cmd/shift-release`).

Out of scope (report anyway, but expect a referral): vulnerabilities in CRIU,
the Linux kernel, PostgreSQL, MinIO/S3, Stripe, Node.js, or other upstream
dependencies; social engineering; physical attacks; and issues that require a
privileged local attacker on a machine that already runs `shift-agent` (the
agent is privileged by design — see docs/threat-model.md).

## Security-relevant design facts

- The full threat model is [docs/threat-model.md](docs/threat-model.md) and the
  cryptography inventory is [docs/security.md](docs/security.md). The summary
  version: checkpoint chunks are encrypted with AES-256-GCM under per-workload
  keys derived with HKDF-SHA256 from an agent-held master key; keys travel to
  a destination only inside the mutually-authenticated TLS channel; session
  tokens are opaque, digest-persisted, and revocable. Customer-managed keys
  and an external KMS are **not** implemented.
- Supported platform and certified CRIU/kernel combinations are published in
  [docs/support-matrix.json](docs/support-matrix.json). Combinations the
  matrix does not certify are blocked from automatic operations.

## Security updates

Security fixes ship in tagged releases (see [docs/release.md](docs/release.md),
signed with `shift-release` keys) and are called out in the release notes of
the advisory. There is no separate security mailing list; advisories are the
channel.

## Safe harbor

Good-faith research conducted within this policy will not result in legal
action from the project. Do not access, exfiltrate, or destroy data that is
not yours; stop testing and report as soon as you have demonstrated impact.
