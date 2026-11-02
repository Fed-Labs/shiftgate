# SHIFT Support Policy

**Effective date:** 2026-10-14

This policy defines what the plan tiers' support labels actually mean. It is
the scope reference for "Community support", "Priority support", and
"Dedicated support engineer" as shown in `GET /v1/plans`
(`internal/billing/plans.go`). Anything beyond this document is a contract
term, not a plan feature.

## Channels

| Channel | Who can use it |
|---|---|
| GitHub issues on [Fed-Labs/shiftgate](https://github.com/Fed-Labs/shiftgate) | everyone |
| GitHub Security Advisories | everyone (vulnerabilities only, see [SECURITY.md](../../SECURITY.md)) |
| Email support (address published at signup and in the billing portal) | Pro, Business, Enterprise |
| Private shared channel + named engineer | Enterprise, contract only |

There is no telephone support. There is no status-page SLA; the `/status`
page is informational.

## Tiers

### Community support (Free)

- Best-effort answers in public GitHub issues.
- No response-time target. Fixes are prioritized at maintainers' discretion
  alongside all other open-source work.
- "Free forever" means: the free plan (2 machines, 10 GB hosted storage, cold
  migration) remains available without payment for as long as the hosted
  service operates, subject to the fair-use and acceptable-use terms. It is
  not a perpetual license grant to the software (that is Apache-2.0) and it
  is not a promise of feature freeze — limits can change with notice in the
  release notes.

### Priority support (Pro, Business)

- Everything in Community.
- Email support with a target first response of **2 business days**
  (Mon–Fri, 09:00–17:00 UTC, excluding public holidays observed by the
  project).
- Bug reports affecting migration, checkpoint, or billing correctness are
  targeted for triage within 2 business days. Resolution times are not
  promised.
- **Migration-incident bridge:** Business includes a 2-hour first response
  for migration incidents (a SHIFT-mediated move in progress or recently
  failed). Pro with SourceGuard Assurance gets the same migration-incident
  response. This is a response target, not a resolution SLA — see the
  no-published-SLA rule below.

### Enterprise (contract only)

- A dedicated support engineer and response-time SLAs may be included **only
  if your enterprise agreement or order form says so**. The marketing phrase
  "SLA-backed response times" has no effect outside a signed agreement.
- The agreement may define: named contacts, response/resolution targets,
  escalation path, security-notification commitments, and credits.

## What support covers

- The SHIFTGATE hosted service and the open-source agents/CLI in this
  repository, on platforms certified in
  [docs/support-matrix.json](../support-matrix.json).
- Not covered: CRIU, kernel, or distro bugs; third-party storage or Stripe
  outages; workloads that themselves cannot be checkpointed (see
  [docs/limitations.md](../limitations.md)); custom development.

## Escalation

1. Support channel of your tier.
2. Maintainer escalation via the same channel (request explicitly).
3. Enterprise: contractual escalation path, if one is in your agreement.

## Security reports

Never route vulnerabilities through support channels — use the private
process in [SECURITY.md](../../SECURITY.md). Security reports are handled on
a separate, faster track regardless of tier.
