import type { Metadata } from "next";
import Link from "next/link";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card } from "@/components/ui";

export const metadata: Metadata = {
  title: "Upgrade your plan",
  description:
    "You reached a plan limit. Paid plans add live migration (peer-to-peer included), machine and storage headroom, teams, audit, and failover — prices and limits come from the catalog the server enforces.",
};

// The hosted upgrade path: every plan refusal (CLI, API, dashboard) points
// here (?feature=&org=&src= ride along for attribution). The page routes to
// the live plan catalog rather than restating prices it cannot enforce.
export default function UpgradePage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
              Upgrade
            </p>
            <h1 className="text-3xl md:text-4xl font-semibold tracking-tight mb-6">
              You reached a limit on your current plan
            </h1>
            <p className="text-text-secondary text-lg leading-relaxed mb-4">
              The free tier stays yours: 2 machines, 10 GB, cold migration — and
              the promise never moves behind a paywall: a failed move never
              destroys the source. Paid plans add what a growing fleet needs —
              live migration (peer-to-peer included), machine and storage
              headroom, checkpoint history and policies, teams, audit, and
              failover.
            </p>
            <p className="text-text-secondary text-lg leading-relaxed mb-10">
              Every price and limit on the pricing page is rendered from the
              same catalog the server enforces — the page and the product cannot
              drift apart.
            </p>
            <div className="flex flex-wrap gap-3">
              <Link
                href="/pricing"
                className="inline-flex items-center justify-center font-medium transition-colors duration-150 rounded-md px-5 py-2.5 text-sm bg-accent text-bg hover:bg-accent-dim"
              >
                Compare all plans
              </Link>
              <Link
                href="/signup"
                className="inline-flex items-center justify-center font-medium transition-colors duration-150 rounded-md px-5 py-2.5 text-sm bg-bg-surface text-text border border-border hover:bg-bg-hover"
              >
                Get started
              </Link>
            </div>
          </Container>
        </Section>

        <Section className="pt-0">
          <Container>
            <Card>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                Not ready to pay?
              </p>
              <p className="text-sm text-text-secondary leading-relaxed">
                Prove the move on your own hardware first: the 14-day Business
                trial needs no card, once per organization —{" "}
                <Link href="/trial" className="text-accent hover:underline">
                  see the trial
                </Link>
                . Or stay on the free tier and move cold; upgrade whenever the
                fleet outgrows it.
              </p>
            </Card>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
