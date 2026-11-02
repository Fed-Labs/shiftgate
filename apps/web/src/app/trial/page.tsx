import type { Metadata } from "next";
import Link from "next/link";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card } from "@/components/ui";

export const metadata: Metadata = {
  title: "Start your free trial",
  description:
    "14 days of Business, no card, once per organization — live migration, machine headroom, teams, and audit. Prove the move on your own hardware before paying for anything.",
};

// The no-card trial door every plan refusal carries (trialURL in the control
// plane). Trial start itself is a dashboard action (Billing → Start free
// trial); this page routes people there through an account.
export default function TrialPage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
              Trial
            </p>
            <h1 className="text-3xl md:text-4xl font-semibold tracking-tight mb-6">
              14 days of Business, no card
            </h1>
            <p className="text-text-secondary text-lg leading-relaxed mb-4">
              Once per organization. Live migration, machine headroom, teams,
              and audit are enabled for the window — prove the move on your own
              hardware before paying for anything. Trial data is preserved when
              it ends; entitlements revert to the free tier (cold-only, 2
              machines, 10 GB) unless you subscribe.
            </p>
            <p className="text-text-secondary text-lg leading-relaxed mb-10">
              Starting it takes one click in the dashboard: Billing → Start free
              trial. Create an account first — or sign in if you already have
              one.
            </p>
            <div className="flex flex-wrap gap-3">
              <Link
                href="/signup"
                className="inline-flex items-center justify-center font-medium transition-colors duration-150 rounded-md px-5 py-2.5 text-sm bg-accent text-bg hover:bg-accent-dim"
              >
                Create an account
              </Link>
              <Link
                href="/login"
                className="inline-flex items-center justify-center font-medium transition-colors duration-150 rounded-md px-5 py-2.5 text-sm bg-bg-surface text-text border border-border hover:bg-bg-hover"
              >
                Sign in
              </Link>
            </div>
          </Container>
        </Section>

        <Section className="pt-0">
          <Container>
            <Card>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                After the trial
              </p>
              <p className="text-sm text-text-secondary leading-relaxed">
                Checkpoints and history are preserved when the trial ends —
                compare the plans on the{" "}
                <Link href="/pricing" className="text-accent hover:underline">
                  pricing page
                </Link>
                ; the free tier is cold-only (2 machines, 10 GB) and paid plans
                add live migration (peer-to-peer included), teams, audit, and
                failover.
              </p>
            </Card>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
