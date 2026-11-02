# SHIFT Terms of Service

**Effective date:** 2026-10-15 · **Applies to:** the SHIFTGATE hosted service
and marketing site. The software in this repository is licensed under
Apache-2.0 and the Elastic License 2.0 per the license map
([licensing](licensing.md), [LICENSE](../../LICENSE),
[LICENSE-ELv2](../../LICENSE-ELv2)) — these terms cover the
hosted service, not the software licenses.

## 1. The service

SHIFTGATE moves process state between Linux machines you control, using
checkpoint/restore (CRIU), and offers an optional hosted control plane:
account and organization management, fleet dashboards, migration
orchestration, encrypted hosted checkpoint storage, and billing.

What the service is **not** — the honest list in
[docs/limitations.md](../limitations.md) is incorporated here:

- "Live migration" is CRIU pre-copy with a final freeze; it is not
  zero-downtime VM live migration.
- Peer-to-peer migrations between your own agents never touch the hosted
  control plane and are outside the platform's enforcement and metering.
- The hosted service is single-region; no data-residency, SIEM export, or
  per-customer identity provider is offered on the hosted service (those are
  self-hosted capabilities).
- SLA-backed response times, custom SLAs, and dedicated support engineers are
  **contract terms only**: they apply solely if your order form or enterprise
  agreement says so. No SLA applies to free or self-serve paid tiers.

## 2. Accounts

You must provide a real email address and keep credentials secret. You are
responsible for activity under your organization, including the machines you
register and the workloads you migrate. One person or legal entity may not
maintain multiple free accounts to evade plan limits.

## 3. Plans, seats, and billing

- Plans, prices, and limits are defined by the API (`GET /v1/plans`) and your
  checkout; the enforced limits are the server's and cannot be changed from
  the client.
- Paid subscriptions are billed by Stripe. Plan changes, payment methods,
  invoices, and cancellations are managed in the Stripe billing portal. **No
  proration, local invoicing, or dunning logic runs in the product** —
  whatever the billing portal reflects is what we charge.
- Per-seat plans count each human member of an organization as one seat.
  API-only integrations are provisioned by members and do not create
  additional seats. Seat counts are enforced against the seats purchased; if
  members exceed seats, member management is refused until you purchase seats
  or remove members.
- **Entitlements follow subscription state.** A cancelled, expired, unpaid,
  or incomplete subscription reverts to free-tier limits (2 machines,
  10 GB hosted storage, cold migration only) while your data is preserved
  for the grace period in section 5. Past-due subscriptions keep their limits
  only while the payment provider is still retrying collection.
- **Refunds.** We refund the first charge of any subscription within 14 days
  of that charge, at our discretion and without a lengthy process — one refund
  per organization. After 14 days, cancellation takes effect at the end of the
  paid period with no proration (section 6).
- **Trials.** Where a trial is offered, it is 14 days of Business-tier
  entitlements, no card required, once per organization. Trial entitlements
  end exactly like a cancelled subscription: limits revert, data is preserved.
  Trial cycling (repeatedly creating organizations to obtain trials) is
  prohibited under section 4.
- **Annual prepay.** Annual subscriptions are billed once per year at
  2 months free relative to monthly (the checkout shows both numbers).
  Cancellation of an annual subscription takes effect at the end of the annual
  period, with no proration.
- **Overage and spend caps.** Where metered overage is enabled for your plan,
  storage and transfer above the plan's included amounts are billed at the
  per-unit rates shown on the pricing page, against a customer-settable spend
  cap (a plan default applies). We never invoice above your cap: at the cap,
  new checkpoint storage issuance is refused and transfer is throttled. The
  free tier is never billed — it stops at quota with an upgrade prompt
  (section 5). Overages are visible before they bill via usage summaries,
  which carry a warn flag at 80% of included amounts (usage forecasts and
  100% / 120% threshold notifications are not offered yet).

## 3.1 SourceGuard Assurance (where purchased or included)

SourceGuard Assurance packages the product's source-preserve invariant ("a
failed move never destroys the source") as a contract. The invariant itself is
product behavior at every tier and is never gated; this section covers the
commercial add-on only.

- **Scope.** SHIFT-mediated operations on registered agents (CLI, agent, and
  dashboard migrations/checkpoints/restores).
- **Credit-back guarantee.** If a SHIFT-mediated move destroys or corrupts the
  source workload through a product defect, we credit 3× the monthly fee (Pro
  add-on) or 12× the monthly fee (Business, where included) on report, and
  provide hands-on recovery assistance. Claim window: 30 days from the
  affected operation.
- **Monthly signed attestation.** A signed export of your organization's
  migration ledger (mode, measured downtime, outcome, rollback events), one
  per calendar month.
- **Migration-incident response.** 2-hour first response for migration
  incidents (Business includes the bridge; SourceGuard on Pro adds it for
  migration incidents).
- **Exclusions.** Self-hosted CRIU misuse outside the documented interfaces,
  operator root actions on the agent host, and hardware/kernel failures with
  no product defect are outside the guarantee — the same licensing-boundary
  honesty as [docs/limitations.md](../limitations.md).

## 4. Acceptable use

You may not: migrate state you have no right to control; use the service to
circumvent plan limits (including repeatedly churning trials or abusing the
peer-to-peer bypass to resell platform orchestration); probe or attack the
service or other customers; resell hosted storage as generic object storage;
or run workloads whose checkpoint/restore would violate law or third-party
rights. The relay exists to carry your migration bytes; abusive bandwidth
use is subject to the fair-use limits below.

## 5. Storage, metering, and fair use

- Hosted storage is quota'd fail-closed per organization from the metered
  bytes your organization occupies in the object store. Checkpoint deletion
  removes manifests locally; mirrored cloud chunks may persist until garbage
  collection — the operational facts are documented in
  [docs/limitations.md](../limitations.md) and
  [docs/deployment.md](../deployment.md). Where storage reclamation is
  available on your plan, it is an explicit, operator-triggered operation.
- Transfer metering: relay bandwidth and egress/restore bytes are metered per
  organization (`usage_records`, kind `transfer_bytes`). The free tier's
  allowance is a hard stop: at the monthly allotment, new relay/transfer
  issuance is refused with an upgrade prompt. Paid
  plan allotments and overage rates are the numbers on the pricing page;
  sustained use beyond your plan's allotment **is** throttled at your spend
  cap (section 3), never billed above it. The former "2× quota" fair-use
  guidance is replaced by these plan allotments.
- Free-tier hosted storage beyond quota is refused at creation time (403
  `STORAGE_QUOTA_EXCEEDED`); we do not bill overages implicitly.

## 6. Cancellation and data

- You may cancel at any time via the billing portal; cancellation takes
  effect per the portal's terms and the entitlements revert as in section 3.
- Where a pause is offered, pausing a subscription moves the organization to
  free-tier limits while preserving all data; payment resumes with the
  subscription. The save offer states this explicitly before you accept.
- Checkpoint data and organization data you delete are subject to the
  retention and erasure behavior in the
  [Privacy Policy](privacy-policy.md). On account erasure, personal data is
  anonymized irreversibly; audit rows are pseudonymized, not removed.
- We may suspend an organization for abuse, non-payment beyond the provider's
  retry window, or legal risk, with notice where legally possible.

## 7. Your data

You own your workloads, checkpoint contents, and organization data. You grant
us only the rights needed to run the service (store and transfer your
encrypted chunks, display your metadata in dashboards, bill you). Checkpoint
plaintext is never available to us; keys stay with your agents.

## 8. Warranty and liability

The hosted service is provided **"as is"** without warranties of
uninterrupted or error-free operation. To the maximum extent permitted by
law, our aggregate liability is limited to the fees you paid in the 12 months
before the claim, and neither party is liable for indirect or consequential
damages. Nothing here limits liability that cannot be limited by law. The
open-source software carries the Apache-2.0 warranty disclaimer in
[LICENSE](../../LICENSE) §7.

## 9. Changes

Material changes will be published with a new effective date; continued use
after the effective date accepts the change. If you do not accept it, stop
using the service and cancel. Grandfathering: when a quota or price changes,
existing subscriptions keep their existing terms until renewal, affected
organizations get at least 30 days' notice (release notes plus email), and
existing data is never deleted to enforce a new limit — new limits apply to
new storage and new subscriptions.

## 10. Governing terms

Enterprise agreements, order forms, and DPAs, where signed, supersede these
terms for that customer. Support entitlements are defined solely by the
[Support Policy](support-policy.md) and any signed agreement.
