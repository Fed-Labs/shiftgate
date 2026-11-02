import type { Metadata } from "next";
import Link from "next/link";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card, EmptyState } from "@/components/ui";

export const metadata: Metadata = {
  title: "Public numbers",
  description:
    "The scoreboard, in public: registered orgs, machines, checkpoints, migration outcomes, restore medians from our own harness, and banded revenue and cost. Measured and published — not estimated.",
};

const API_BASE = process.env.NEXT_PUBLIC_API_URL || "https://shiftgate.dev";

// Contract-true to the PublicStats schema in api/openapi/shift.yaml — the
// control plane is the only source of these counters; the page never invents
// fields the endpoint does not publish.
type PublicStats = {
  organizations?: number;
  machines_registered?: number;
  checkpoints_created?: number;
  migrations_total?: number;
  migrations_succeeded?: number;
  restore_p50_ms?: number;
  mrr_band_usd?: string;
  cogs_band_usd?: string;
  generated_at?: string;
};

async function loadStats(): Promise<PublicStats | null> {
  try {
    const response = await fetch(`${API_BASE}/v1/public/stats`, {
      next: { revalidate: 3600 },
    });
    if (!response.ok) return null;
    return (await response.json()) as PublicStats;
  } catch {
    return null;
  }
}

export default async function NumbersPage() {
  const stats = await loadStats();

  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <div className="mb-12">
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
                Transparency
              </p>
              <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                The numbers, as measured
              </h1>
              <p className="text-text-secondary text-lg leading-relaxed">
                Technical buyers deserve a scoreboard, not a vibe. This page publishes what
                we can measure honestly: workload moves and their outcomes, restore medians
                from our own harness, and the revenue and cost bands of a $100-budget year.
                Rollbacks are a feature here — every one is a source that survived a failed
                move.
              </p>
            </div>
          </Container>
        </Section>

        <Section className="pt-0">
          <Container>
            {stats === null ? (
              <EmptyState
                title="Live counters are not published yet"
                description="The public stats feed (control-plane read-only counters) is not wired yet. Until it is, this page carries the reference numbers below — each links to the harness or document it comes from."
              />
            ) : (
              <div className="grid grid-cols-2 md:grid-cols-3 gap-5">
                {[
                  ["Orgs", stats.organizations],
                  ["Machines", stats.machines_registered],
                  ["Checkpoints", stats.checkpoints_created],
                  ["Moves attempted", stats.migrations_total],
                  ["Moves committed", stats.migrations_succeeded],
                  ["Restore median (ms)", stats.restore_p50_ms],
                ].map(([label, value]) => (
                  <Card key={String(label)}>
                    <p className="text-xs font-mono uppercase tracking-widest text-text-muted mb-2">
                      {label}
                    </p>
                    <p className="text-2xl font-semibold tabular-nums">
                      {value === undefined || value === null ? "—" : String(value)}
                    </p>
                  </Card>
                ))}
              </div>
            )}
            {stats?.generated_at && (
              <p className="text-xs text-text-muted mt-4">
                Counters generated {stats.generated_at}.
              </p>
            )}
          </Container>
        </Section>

        <Section className="pt-0">
          <Container>
            <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
              <Card>
                <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                  Reference numbers (harness)
                </p>
                <ul className="space-y-2 text-sm text-text-secondary">
                  <li>Full checkpoint: 914 – 973 ms (bench box, ~17 MB state).</li>
                  <li>Restore wall, eager: 2.27 – 2.77 s incl. resume + health validation.</li>
                  <li>Live-move downtime: 3.20 – 3.52 s (17 MB / 118 MB states, loopback).</li>
                  <li>Cold-move downtime: 3.75 – 5.50 s (same states).</li>
                  <li>Transfer inside the live freeze: 107 – 116 ms (the cold-mode transfer window is the larger 215 – 223 ms).</li>
                </ul>
                <p className="text-xs text-text-muted mt-3">
                  Source: the documented harness in docs/benchmarks.md — reproducible
                  commands, single-machine loopback, not SLAs.
                </p>
              </Card>
              <Card>
                <p className="text-xs font-mono uppercase tracking-widest text-accent mb-2">
                  Business numbers (band)
                </p>
                <ul className="space-y-2 text-sm text-text-secondary">
                  <li>
                    Revenue:{" "}
                    {stats?.mrr_band_usd
                      ? `${stats.mrr_band_usd} (run-rate band — exact revenue never leaves the server)`
                      : "run-rate bands publish here once the stats feed reports them"}
                    .
                  </li>
                  <li>
                    Cost:{" "}
                    {stats?.cogs_band_usd
                      ? `${stats.cogs_band_usd} external spend band`
                      : "external spend target ≤ $100 for the year (domain + contingency)"}
                    ; the ops ledger is audited quarterly.
                  </li>
                  <li>
                    Rollbacks: a failed move that leaves its source alive is the promise
                    kept — every move outcome sits in the counters above.
                  </li>
                </ul>
                <p className="text-xs text-text-muted mt-3">
                  Method notes travel with the numbers. If a number cannot be measured
                  honestly, it stays off this page.
                </p>
              </Card>
            </div>
          </Container>
        </Section>

        <Section className="pt-0">
          <Container narrow>
            <div className="rounded-lg border border-border bg-bg-elevated p-6">
              <h2 className="text-base font-semibold mb-2">What this page is not</h2>
              <p className="text-sm text-text-secondary leading-relaxed">
                Not a live status page (that is{" "}
                <Link href="/status" className="text-text hover:text-accent transition-colors">
                  /status
                </Link>
                ), not a benchmark of your workload ({" "}
                <Link href="/docs/benchmarks" className="text-text hover:text-accent transition-colors">
                  run the harness
                </Link>{" "}
                yourself), and never a claim we cannot source. The promise the numbers
                serve: state moves, and a failed move never destroys the source.
              </p>
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
