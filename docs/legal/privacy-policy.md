# SHIFT Privacy Policy

**Effective date:** 2026-10-14 · **Applies to:** the SHIFTGATE hosted control
plane (shift.dev service), the marketing site, and the accounts created at
signup. Self-hosted deployments are operated by their own organization, which
becomes the data controller for its own users.

This policy is written against the actual data the product stores. Where the
code and this document disagree, the code wins and this document is a bug —
file it.

## Who we are

Fed-Labs ("we") operates the SHIFTGATE hosted service. Contact for privacy
requests: the `security` contact on the
[Fed-Labs organization profile](https://github.com/Fed-Labs) — request a
private channel for anything sensitive; privacy requests may also be filed as
a private security advisory if you need an authenticated channel.

## What we collect

### At signup and login (`POST /v1/auth/register`, `/v1/auth/login`)

| Data | Where it is stored | Why |
|---|---|---|
| Email address | `users.email` (control-plane database) | account identity, login, notifications |
| Password | Argon2id hash + pepper (`users.password_hash`) — never plaintext | authentication |
| Display name | `users.display_name` | attribution in the dashboard and audit trail |
| Organization name | `organizations.name` | creates your workspace |

### Automatically collected while you use the service

| Data | Where it is stored | Why |
|---|---|---|
| Session metadata: user agent string and client IP address | `sessions.user_agent`, `sessions.remote_addr` | abuse detection, session revocation, security forensics |
| Audit trail: who did what to which resource, request id, and client IP | `audit_events` (including `remote_addr`) | security accountability, admin audit log |
| Billing identifiers | `entitlements.stripe_customer_id`, `stripe_subscription_id`, `stripe_events` | subscription management (payment details are held by Stripe, not by us) |
| Machine and workload inventory you register (hostname, hardware capabilities, workload names, paths you configure) | control-plane database | fleet management — this is the product's purpose |
| Usage measurements (checkpoint storage bytes; transfer/relay bandwidth when metering is enabled) | `usage_records` | quota enforcement, billing summaries |

### What we do **not** collect

- Payment card numbers, bank details, or government ids (Stripe collects
  these directly; we never see them).
- The contents of your workloads or checkpoint plaintext. Checkpoint chunks
  are encrypted end-to-end before they leave the source agent
  ([docs/security.md](../security.md)); the hosted storage layer stores
  ciphertext only and cannot decrypt it.
- Location data, contacts, or telemetry from your machines beyond the
  inventory you configure.

## Legal basis

- **Contract** (GDPR Art. 6(1)(b)): account data, machine/workload metadata,
  and usage metering are processed to deliver the service you signed up for.
- **Legitimate interests** (Art. 6(1)(f)): security logging (audit events,
  session IP/user-agent), abuse prevention, and fraud prevention for billing.
- **Consent** (Art. 6(1)(a)): optional product communications, where offered;
  withdrawable at any time. We do not run third-party advertising trackers.

## Sub-processors

The canonical, maintained list is [subprocessors.md](subprocessors.md)
(currently: Stripe for billing, Cloudflare for DNS/TLS/email routing, and the
self-hosted control-plane infrastructure we operate; customer-configured
object storage for mirrored checkpoint ciphertext). Checkpoint plaintext is
never available to any sub-processor — agents upload ciphertext and encrypted
manifests only.

Sub-processor changes will be reflected on that page. Enterprise customers
may sign a DPA listing the same sub-processors.

## Retention

- Account and organization records: while the account exists, plus the
  retention window below for derived logs.
- Audit events: per organization-configurable retention policy
  (`audit_retention_days`, default 365 days, 0 = keep forever at the
  organization's discretion — available on every plan tier). The platform
  does not silently shorten or extend an organization's chosen window; a
  window above the plan's cap (365 days on Free/Pro, see
  [operations.md](../operations.md#retention)) is refused outright rather
  than reduced.
- Sessions and their IP/user-agent metadata: revoked sessions are removed by
  the retention sweep; active sessions expire on their own lifetimes
  (15-minute access / 30-day refresh tokens).
- Billing events: retained for accounting obligations (typically 7–10 years
  depending on jurisdiction) — Stripe holds the underlying payment records.

## Your rights (GDPR / UK GDPR / CCPA)

- **Access & portability:** organization owners can export organization
  metadata, member lists, machine/workload inventory, and audit history
  through the API (`GET /v1/organizations/...`). Account-level data can be
  requested from the contact above.
- **Erasure:** account deletion anonymizes the personal data we hold about
  you — email and display name are replaced with irreversible placeholders,
  session identifiers and connection metadata (IP addresses, user agents) are
  destroyed, and audit rows are pseudonymized rather than deleted so the
  security trail stays intact without pointing at a natural person
  (`internal/database/erasure.go`). The anonymization is designed so it
  cannot be reversed (the original email is not hashed-and-kept; it is
  overwritten).
- **SCIM-deprovisioned users:** when your identity provider deletes a user
  through SCIM, the account is deactivated immediately and its history is
  preserved for audit. The email address and connection metadata are
  anonymized by the erasure sweep described above (the deactivation record
  itself keeps no more than a pseudonymous id), matching the behavior
  [docs/limitations.md](../limitations.md) documents.
- **Rectification & objection:** update display name/email through the API or
  contact us; object to legitimate-interest processing (security logging) by
  contacting us — note that we cannot run a multi-tenant service without
  security logs and will explain that balance on request.

Requests are answered within 30 days. You may lodge a complaint with your
supervisory authority at any time.

## Data residency

The hosted service runs in a single region; hosted checkpoint ciphertext lands
in the bucket the platform operator configures. We make no residency promise
for the hosted service. Self-hosted deployments keep every byte — including
checkpoint ciphertext — inside your own network and storage.

## Changes

Material changes to this policy will be published here with a new effective
date and announced in the release notes of the service.

## Related documents

- [Terms of Service](terms.md)
- [Support Policy](support-policy.md)
- [Security Policy](../../SECURITY.md) and [docs/security.md](../security.md)
- [docs/limitations.md](../limitations.md) — the product's honest list of what
  it does not do
