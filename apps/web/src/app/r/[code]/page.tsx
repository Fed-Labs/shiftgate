import type { Metadata } from "next";
import Link from "next/link";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card } from "@/components/ui";

export const metadata: Metadata = {
  title: "You're invited to SHIFT",
  description:
    "Someone shared SHIFT with you. Sign up with the referral link and, when your team runs its first move, you both get Pro free for a month (up to 12).",
};

// The share link `shiftgate refer` prints (…/r/<code>). The code rides to
// signup as ?ref=; the signup flow stamps it on the new organization through
// POST /v1/organizations/{id}/referral/claim (self-referral is refused
// server-side). The reward terms mirror the CLI's own printout so the link
// and the terminal never disagree.
export default async function ReferralPage({
  params,
}: {
  params: Promise<{ code: string }>;
}) {
  const { code } = await params;
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
              Referral
            </p>
            <h1 className="text-3xl md:text-4xl font-semibold tracking-tight mb-6">
              You&apos;ve been invited to move machines like files
            </h1>
            <p className="text-text-secondary text-lg leading-relaxed mb-10">
              SHIFT moves running computation between Linux machines — and a
              failed move never destroys the source. Create an account through
              this referral link and, when your team runs its first move, you
              and the person who invited you both get Pro free for a month (up
              to 12).
            </p>
            <div className="flex flex-wrap gap-3">
              <Link
                href={`/signup?ref=${encodeURIComponent(code)}`}
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
                The offer, in full
              </p>
              <p className="text-sm text-text-secondary leading-relaxed">
                Give a month, get a month: when a team you invite runs its first
                move, you both get Pro free for a month (up to 12). The free
                tier is there meanwhile — 2 machines, 10 GB, cold migration —
                and{" "}
                <Link href="/pricing" className="text-accent hover:underline">
                  the pricing page
                </Link>{" "}
                lays out what Pro adds.
              </p>
            </Card>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
