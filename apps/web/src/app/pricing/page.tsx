import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card, Badge, Divider, EmptyState } from "@/components/ui";
import type { PlanCatalogEntry } from "@/lib/types";
import { Check } from "lucide-react";

export const metadata: Metadata = {
  title: "Pricing",
  description: "SHIFTGATE plans for every scale — from a single machine to custom infrastructure.",
};

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "http://127.0.0.1:8090";

// Plans come from the control plane's catalog (GET /v1/plans), which is the
// same source the server enforces entitlements from. The page renders it; it
// never defines limits or prices of its own.
async function loadPlans(): Promise<PlanCatalogEntry[] | null> {
  try {
    const response = await fetch(`${API_BASE}/v1/plans`, {
      next: { revalidate: 300 },
    });
    if (!response.ok) return null;
    const plans = (await response.json()) as PlanCatalogEntry[];
    if (!Array.isArray(plans) || plans.length === 0) return null;
    return plans;
  } catch {
    // An unreachable control plane is reported, never replaced with guessed
    // pricing.
    return null;
  }
}

function formatPrice(plan: PlanCatalogEntry): string {
  if (plan.price_cents === 0) return "$0";
  return `$${Math.round(plan.price_cents / 100)}`;
}

function priceLabel(plan: PlanCatalogEntry): string {
  if (plan.price_cents === 0) return "Free forever";
  if (plan.price_cents < 0) return "";
  return plan.per_seat ? "/seat/mo" : "/mo";
}

const TIER_PRESENTATION: Record<string, { highlight?: boolean; cta: string }> = {
  free: { cta: "Get started" },
  pro: { highlight: true, cta: "Get started" },
  business: { cta: "Get started" },
  enterprise: { cta: "Contact us" },
};

const faqs = [
  {
    q: "Can I switch plans at any time?",
    a: "Yes. Upgrades take effect immediately and you are billed the prorated difference. Downgrades apply at the end of your current billing period.",
  },
  {
    q: "What happens to my checkpoints if I downgrade?",
    a: "Your existing checkpoints are preserved for 30 days. If they exceed the storage limit of your new plan, you will need to remove older checkpoints before the grace period ends.",
  },
  {
    q: "Do you offer discounts for startups or open-source projects?",
    a: "We do. Reach out to us at billing@shiftgate.dev with a brief description of your project and we will figure something out.",
  },
  {
    q: "How does per-seat billing work?",
    a: "Each team member who accesses your organization's SHIFTGATE dashboard counts as one seat. Read-only API keys and automated agents do not count toward the seat total.",
  },
];

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
                Pricing
              </h1>
              <p className="text-text-secondary text-lg max-w-xl mx-auto">
                Start free. Upgrade when your infrastructure demands it.
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
                            plan.key === "enterprise" ? "/contact" : "/login"
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
      </main>
      <SiteFooter />
    </>
  );
}
