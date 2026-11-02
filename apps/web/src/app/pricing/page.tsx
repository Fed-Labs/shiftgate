import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card, Badge, Divider, EmptyState } from "@/components/ui";
import type { PlanCatalogEntry } from "@/lib/types";
import { formatCurrency } from "@/lib/utils";
import { Check } from "lucide-react";

export const metadata: Metadata = {
  title: "Pricing",
  description:
    "Price the fleet, not the bytes. Free is 2 machines and cold-only; paid plans add live migration (peer-to-peer included), teams, audit, and failover. A failed move never destroys the source.",
};

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "https://shiftgate.dev";

// Plans come from the control plane's catalog (GET /v1/plans), which is the
// same source the server enforces entitlements from. The page renders it; it
// never defines limits or prices of its own. Add-on copy below (SourceGuard
// Assurance, HA Pack, trials, overage) is packaging text — the enforced
// entitlements still come from the catalog and the server.
async function loadPlans(): Promise<PlanCatalogEntry[] | null> {
  // A transient failure at build/deploy must not ship a pricing page without
  // prices: retry before reporting unavailable. On persistent failure the
  // page reports it — it never replaces the catalog with guessed pricing.
  for (let attempt = 0; attempt < 3; attempt++) {
    try {
      const response = await fetch(`${API_BASE}/v1/plans`, {
        next: { revalidate: 300 },
      });
      if (response.ok) {
        const plans = (await response.json()) as PlanCatalogEntry[];
        if (Array.isArray(plans) && plans.length > 0) return plans;
      }
    } catch {
      // An unreachable control plane is reported below, never guessed around.
    }
  }
  return null;
}

function formatPrice(plan: PlanCatalogEntry): string {
  if (plan.price_cents === 0) return "$0";
  return formatCurrency(plan.price_cents);
}

function priceLabel(plan: PlanCatalogEntry): string {
  if (plan.price_cents === 0) return "free tier";
  if (plan.price_cents < 0) return "";
  return plan.per_seat ? "/seat/mo" : "/mo";
}

const TIER_PRESENTATION: Record<string, { highlight?: boolean; cta: string }> = {
  free: { cta: "Get started" },
  pro: { highlight: true, cta: "Get started" },
  business: { cta: "Get started" },
  enterprise: { cta: "Contact us" },
};

const GATES: Array<{ gate: string; free: string; pro: string; business: string; enterprise: string }> = [
  { gate: "Live migration (peer-to-peer included)", free: "cold only", pro: "yes", business: "yes", enterprise: "yes" },
  { gate: "Cold migration, dry-run/preflight, checkpoints, restore, same-machine clone", free: "yes", pro: "yes", business: "yes", enterprise: "yes" },
  { gate: "Periodic checkpoint policies (keep_last schedules)", free: "—", pro: "yes", business: "yes", enterprise: "yes" },
  { gate: "Warm-standby failover", free: "—", pro: "HA Pack", business: "yes", enterprise: "yes" },
  { gate: "SourceGuard Assurance (credit-back, monthly attestation)", free: "invariant only", pro: "add-on", business: "included", enterprise: "custom terms" },
  { gate: "Storage reclamation", free: "—", pro: "yes", business: "yes", enterprise: "yes" },
  { gate: "Teams, roles & permissions", free: "—", pro: "—", business: "yes", enterprise: "yes" },
  { gate: "Audit log (365 days)", free: "—", pro: "—", business: "yes", enterprise: "yes + export" },
  { gate: "SSO (OIDC) + SCIM", free: "—", pro: "—", business: "yes", enterprise: "yes" },
  { gate: "Metered storage/transfer overage with spend cap", free: "never billed", pro: "yes", business: "yes", enterprise: "contract" },
];

const faqs = [
  {
    q: "What is the promise I am actually buying?",
    a: "A failed move never destroys the source. That invariant is product behavior at every tier — including the free tier — and it is never gated. What paid plans add is the fleet around it: live migration (peer-to-peer included), history and lineage, policies, failover, teams, audit, and — with SourceGuard Assurance — a credit-backed contract around the invariant.",
  },
  {
    q: "What does SourceGuard Assurance include?",
    a: "The engineering invariant is free for everyone. SourceGuard Assurance sells the contract around it: a credit-back guarantee (3× the monthly fee on Pro, 12× on Business) if a SHIFT-mediated move ever destroys or corrupts the source through a product defect, a monthly signed attestation of your migration ledger (every move: mode, downtime, outcome, rollback events), and 2-hour migration-incident response. $19/mo on Pro, included on Business.",
  },
  {
    q: "What is the HA Pack?",
    a: "Warm-standby failover: checkpoints replicate to a standby agent and the newest copy can restore when the source is provably dead — or on your command. Automatic restore additionally requires a fencing provider, which is disabled by default (fail-closed), so the always-available path is the manual trigger. $39/mo on Pro, included on Business and Enterprise. Be honest with yourself about the current edge: a manual trigger on a source that is partitioned rather than dead can leave two live copies (docs/limitations.md) — name the human who decides death.",
  },
  {
    q: "Is there a trial?",
    a: "Yes: 14 days of Business entitlements, no card, once per organization — start it from the dashboard (Billing → Start free trial). Trials exist to let you prove the move on your own hardware; they are not a discount mechanism (trial cycling is against the terms).",
  },
  {
    q: "How does overage work? Will I get a surprise bill?",
    a: "No. Included storage and transfer are per plan (the tier cards are rendered from the enforced catalog); beyond them, storage is $0.05/GB-mo and transfer is $0.04/GB on both Pro and Business — the same values the catalog enforces and the tier cards print. Overages run against a customer-settable spend cap (default $50/mo Pro, $200/mo Business) — at the cap, new checkpoint storage is refused and transfer throttled, never billed above your cap. The cap is chosen at checkout; adjust it later from the billing portal or by contacting us. The free tier is never billed: it hard-stops at quota with an upgrade prompt.",
  },
  {
    q: "How does per-seat billing work?",
    a: "Business includes 5 seats and adds seats at $19/seat/mo beyond that. Seats are not metered against dashboard logins — adjust the quantity in the billing portal. API keys and automated agents do not need seats.",
  },
  {
    q: "Do I need the platform to move between my own machines?",
    a: "No: peer-to-peer moves work agent-to-agent. The fleet features (history, policy, failover, audit) are what the paid platform adds beyond live mode.",
  },
  {
    q: "Can I switch plans at any time?",
    a: "Yes. Plan changes run through the Stripe customer portal (Billing → Manage). Upgrades, downgrades, and any proration are handled by Stripe according to your subscription — this platform does not compute proration itself. Annual plans are 2 months free ($290 Pro, $990 Business) and cancel at end of period.",
  },
  {
    q: "What happens to my checkpoints if I downgrade?",
    a: "Your existing checkpoints are kept. The new plan's storage quota is enforced when new checkpoints are created. Paid plans can reclaim mirrored chunks explicitly; on the free tier, hosted-storage cleanup is an operator-side step today — contact us if you need stored bytes reduced. Data is never deleted or frozen on downgrade.",
  },
  {
    q: "What is the refund policy?",
    a: "14-day money-back on the first charge of any subscription, no questions (one per organization). After that, cancel in the portal — effective end of period, no proration.",
  },
  {
    q: "What does support include?",
    a: "The free tier is community support: GitHub issues and community channels, with no guaranteed response time. Pro and Business get a 2-business-day first response; Business adds a migration-incident bridge (2-hour migration-incident response, also available on Pro with SourceGuard Assurance). There is no published SLA today — contractual response terms, if any, are defined per agreement.",
  },
  {
    q: "Do you offer discounts for startups or open-source projects?",
    a: "We do. Maintainers of open-source projects with a public repository can apply from the contact page (3 months of Business, free), and founding Business partnerships are open at 50% off year one with case-study rights — reach out from the contact page with a brief description of your project.",
  },
  {
    q: "Is live migration really a paid feature?",
    a: "Yes — on every path, including peer-to-peer. Live migration is a paid plan feature, including peer-to-peer moves. The free tier is cold-only (2 machines, 10 GB). We say this everywhere rather than at the last click: the gate is enforced in the agent and CLI, so the shipped product never disagrees with this page.",
  },
];

const faqJsonLd = {
  "@context": "https://schema.org",
  "@type": "FAQPage",
  mainEntity: faqs.map(({ q, a }) => ({
    "@type": "Question",
    name: q,
    acceptedAnswer: { "@type": "Answer", text: a },
  })),
};

export default async function PricingPage() {
  const plans = await loadPlans();

  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        {/* Header */}
        <Section>
          <Container narrow>
            <div className="text-center space-y-4">
              <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                Price the fleet, not the bytes
              </h1>
              <p className="text-text-secondary text-lg max-w-xl mx-auto">
                Start free — 2 machines, 10 GB, cold migration. Upgrade when the fleet
                needs live moves, burst capacity, teams, and audit. Every move on every
                tier keeps the same promise: a failed move never destroys the source.
              </p>
              <p className="text-sm text-text-muted max-w-xl mx-auto">
                Live migration is a paid plan feature, including peer-to-peer moves.
                Free tier is cold-only. Annual plans are 2 months free.
              </p>
            </div>
          </Container>
        </Section>

        {/* Tier cards */}
        <Section className="pt-0">
          <Container>
            {plans === null ? (
              <EmptyState
                title="Pricing is unavailable"
                description="The plan catalog could not be loaded from the control plane. Please try again in a moment."
              />
            ) : (
              <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-5">
                {plans.map((plan) => {
                  const presentation =
                    TIER_PRESENTATION[plan.key] ?? { cta: "Get started" };
                  const highlight = presentation.highlight ?? false;
                  return (
                    <Card
                      key={plan.key}
                      className={`flex flex-col ${
                        highlight ? "border-accent/40 ring-1 ring-accent/20" : ""
                      }`}
                    >
                      {highlight && (
                        <div className="mb-4">
                          <Badge variant="accent">Most popular</Badge>
                        </div>
                      )}

                      <h2 className="text-lg font-medium text-text">
                        {plan.name}
                      </h2>

                      <div className="mt-3 flex items-baseline gap-1">
                        {plan.price_cents < 0 ? (
                          <span className="text-2xl font-semibold">Custom</span>
                        ) : (
                          <>
                            <span className="text-3xl font-semibold tabular-nums">
                              {formatPrice(plan)}
                            </span>
                            <span className="text-text-muted text-sm">
                              {priceLabel(plan)}
                            </span>
                          </>
                        )}
                      </div>

                      <p className="mt-3 text-sm text-text-secondary">
                        {plan.description}
                      </p>

                      <Divider className="my-5" />

                      <ul className="space-y-2.5 flex-1">
                        {plan.features.map((feature) => (
                          <li
                            key={feature}
                            className="flex items-start gap-2.5 text-sm text-text-secondary"
                          >
                            <Check className="w-4 h-4 text-accent mt-0.5 shrink-0" />
                            <span>{feature}</span>
                          </li>
                        ))}
                      </ul>

                      <div className="mt-6">
                        <a
                          href={
                            plan.key === "enterprise" ? "/contact" : "/signup"
                          }
                          className={`inline-flex items-center justify-center w-full font-medium transition-colors duration-150 rounded-md px-4 py-2 text-sm ${
                            highlight
                              ? "bg-accent text-bg hover:bg-accent-dim"
                              : "bg-bg-surface text-text border border-border hover:bg-bg-hover"
                          }`}
                        >
                          {presentation.cta}
                        </a>
                      </div>
                    </Card>
                  );
                })}
              </div>
            )}
          </Container>
        </Section>

        {/* Feature clarity row */}
        <Section className="pt-0">
          <Container narrow>
            <div className="rounded-lg border border-border bg-bg-elevated p-5 text-sm text-text-secondary">
              <strong className="text-text">Live migration (peer-to-peer included) —
              Pro and up. Free tier is cold-only.</strong>{" "}
              Cold migration, dry-run/preflight, checkpoints, restores, and
              same-machine clones are free on every tier, forever — gating safety
              would convert nobody, so we never gate the source-preserve invariant
              itself.
            </div>
          </Container>
        </Section>

        {/* SourceGuard guarantee band */}
        <Section className="pt-0">
          <Container>
            <div className="rounded-lg border border-accent/40 bg-bg-elevated p-6">
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                SourceGuard Assurance
              </p>
              <h2 className="text-xl font-semibold mb-3">
                A failed move never destroys the source — proven in e2e rollback
                tests, credit-backed on paid plans.
              </h2>
              <p className="text-sm text-text-secondary leading-relaxed">
                The invariant ships in the product at every tier. SourceGuard
                Assurance is the contract around it: credit-back if a SHIFT-mediated
                move ever destroys or corrupts the source through a product defect
                (3× the monthly fee on Pro, 12× on Business), a monthly signed
                attestation of your migration ledger — every move with its mode,
                measured downtime, outcome, and rollback events — and 2-hour
                migration-incident response. $19/mo on Pro, included on Business.
              </p>
            </div>
          </Container>
        </Section>

        {/* Add-ons + trial + founding partner */}
        <Section className="pt-0">
          <Container>
            <div className="grid grid-cols-1 md:grid-cols-3 gap-5">
              <Card>
                <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                  HA Pack
                </p>
                <h3 className="text-base font-medium mb-2">Warm-standby failover</h3>
                <p className="text-sm text-text-secondary leading-relaxed">
                  Checkpoints replicate to a standby agent; the newest copy can
                  restore when the source is provably dead — unreachable and silent —
                  or on your command. Automatic restore requires a fencing provider
                  (disabled by default — fail-closed); the manual trigger is the
                  always-available path. $39/mo on Pro, included on Business and
                  Enterprise. The honest edge: a manual trigger on a source that is
                  partitioned rather than dead can leave two live copies — documented
                  in limitations.
                </p>
              </Card>
              <Card>
                <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                  Trial
                </p>
                <h3 className="text-base font-medium mb-2">14 days of Business, no card</h3>
                <p className="text-sm text-text-secondary leading-relaxed">
                  Once per organization. Start it from the dashboard (Billing) and
                  prove the move on your own hardware — live migration, machine
                  headroom, teams, audit. Trial data is preserved when it ends;
                  entitlements revert to the free tier (cold-only, 2 machines,
                  10 GB) unless you subscribe.
                </p>
              </Card>
              <Card>
                <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                  Founding partners
                </p>
                <h3 className="text-base font-medium mb-2">50% off year one</h3>
                <p className="text-sm text-text-secondary leading-relaxed">
                  Two founding Business partnerships: 50% off year one for the right
                  to publish a case study — you approve every sentence. Ask for the
                  FOUNDING-TEAM coupon from the{" "}
                  <a href="/contact" className="text-text hover:text-accent transition-colors">
                    contact page
                  </a>
                  .
                </p>
              </Card>
            </div>
          </Container>
        </Section>

        {/* Comparison gate matrix */}
        <Section className="pt-0">
          <Container>
            <h2 className="text-xl font-semibold mb-6">What each tier unlocks</h2>
            <div className="overflow-x-auto rounded-lg border border-border">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border bg-bg-elevated text-left">
                    <th className="px-4 py-3 font-medium text-text">Gate</th>
                    <th className="px-4 py-3 font-medium text-text">Free</th>
                    <th className="px-4 py-3 font-medium text-text">Pro</th>
                    <th className="px-4 py-3 font-medium text-text">Business</th>
                    <th className="px-4 py-3 font-medium text-text">Enterprise</th>
                  </tr>
                </thead>
                <tbody>
                  {GATES.map((row) => (
                    <tr key={row.gate} className="border-b border-border-subtle last:border-0">
                      <td className="px-4 py-3 text-text-secondary">{row.gate}</td>
                      <td className="px-4 py-3 text-text-secondary">{row.free}</td>
                      <td className="px-4 py-3 text-text-secondary">{row.pro}</td>
                      <td className="px-4 py-3 text-text-secondary">{row.business}</td>
                      <td className="px-4 py-3 text-text-secondary">{row.enterprise}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="text-xs text-text-muted mt-3">
              Numeric limits (machines, storage, transfer) are rendered above from
              the same catalog the server enforces — the page and the product cannot
              drift apart. Enterprise is custom —{" "}
              <a href="/contact" className="text-text hover:text-accent transition-colors">
                contact us
              </a>
              .
            </p>
          </Container>
        </Section>

        {/* FAQ */}
        <Section className="pt-0">
          <Container narrow>
            <h2 className="text-xl font-semibold mb-8">
              Frequently asked questions
            </h2>
            <div className="space-y-8">
              {faqs.map(({ q, a }) => (
                <div key={q}>
                  <h3 className="text-sm font-medium text-text mb-2">{q}</h3>
                  <p className="text-sm text-text-secondary leading-relaxed">
                    {a}
                  </p>
                </div>
              ))}
            </div>
          </Container>
        </Section>

        <script
          type="application/ld+json"
          dangerouslySetInnerHTML={{ __html: JSON.stringify(faqJsonLd) }}
        />
      </main>
      <SiteFooter />
    </>
  );
}
