# SHIFT sub-processors

This page lists the third parties that process data to run the SHIFT hosted
service. It is part of the [Privacy Policy](privacy-policy.md) and exists to
unblock DPA conversations without a lawyer in the loop. Enterprise agreements
can attach a signed DPA referencing this list.

| Sub-processor | Role | Data processed | Location |
|---|---|---|---|
| Stripe, Inc. | Payments and subscription billing | Email, billing identity, subscription state; card data is Stripe's (we never see PAN) | Stripe's infrastructure (US; global) |
| Cloudflare | DNS, TLS termination, email routing for support mail | Request metadata; support email content (routed to the founder inbox) | Cloudflare's edge network |
| Self-hosted / own hardware | Application + PostgreSQL hosting for the control plane | All service data as described in the privacy policy | Operated by us; single-region |

Notes:

- Checkpoint plaintext is never available to any sub-processor: agents upload
  chunk ciphertext and encrypted manifests; keys stay with your agents
  ([docs/security.md](../security.md)).
- No advertising networks, no third-party analytics SDKs, no session-replay.
  Analytics are cookie-less / self-hosted and aggregate-only.
- Support mail is handled by a human founder inbox via Cloudflare Email
  Routing; free-tier support is GitHub issues (public by nature — do not post
  secrets; the issue templates ask for `doctor --json` output).
- Changes to this list are published here before they take effect; material
  changes follow the notice rules in the [Terms](terms.md) §9.

Contact for DPA requests and data questions: the support address in the
[Support Policy](support-policy.md).
