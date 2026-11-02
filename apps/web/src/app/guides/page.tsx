import Link from "next/link";
import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Guides: Linux workload mobility, CRIU, CI burst, failover",
  description:
    "Practical guides for moving running Linux state: CRIU checkpoint/restore tutorials, CI burst from warm checkpoints, maintenance evacuation, warm standby, security, and the honest limitations.",
};

const GUIDES: Array<{ href: string; title: string; blurb: string; group: string }> = [
  { href: "/guides/linux-workload-mobility", group: "Pillar", title: "Linux Workload Mobility: Moving Running State Between Machines", blurb: "The 2027 guide: what moves, the commit gate, cold vs live, and the four workflows that pay for it." },
  { href: "/guides/criu-checkpoint-restore-tutorial", group: "CRIU fundamentals", title: "CRIU Checkpoint/Restore: A 15-Minute Working Example", blurb: "What the kernel needs, a runnable example with measured times, and what raw CRIU leaves to you." },
  { href: "/guides/criu-limitations-explained", group: "CRIU fundamentals", title: "CRIU Limitations Explained (TCP Repair, GPU State, Kernel Skew)", blurb: "The honest boundaries list — reject or surface, never fake." },
  { href: "/guides/criu-lazy-restore", group: "CRIU fundamentals", title: "Lazy Restore Explained: Start Before Your Memory Arrives", blurb: "How lazy pages work — and the measured case where they lose." },
  { href: "/guides/criu-tcp-repair", group: "CRIU fundamentals", title: "TCP Repair or It Didn't Happen: Honest Socket Migration", blurb: "When socket state ports, when it is rejected, and why to design for reconnect." },
  { href: "/guides/criu-pre-dump", group: "CRIU fundamentals", title: "Pre-Dump Passes: Buying Down the Frozen Window", blurb: "Iterative dirty-page transfer and its measured diminishing returns." },
  { href: "/guides/criu-process-tree", group: "CRIU fundamentals", title: "Multi-Process Trees: Checkpointing the Whole Workload, Not One PID", blurb: "Trees as a unit, out-of-root files, and what breaks at 1,500 processes." },
  { href: "/guides/migrate-running-process-between-linux-servers", group: "Moving workloads", title: "How to Migrate a Running Process Between Linux Servers", blurb: "The end-to-end recipe, including the failure path scripts get wrong." },
  { href: "/guides/live-migration-without-shared-storage", group: "Moving workloads", title: "Live Migration Without Shared Storage: A Practical Pattern", blurb: "Pre-copy, measured freeze, commit gate — no NAS, no hypervisor." },
  { href: "/guides/gpu-node-drain-without-requeue", group: "Moving workloads", title: "Draining a GPU Node Without Requeueing the Job", blurb: "Evacuate a GPU box mid-run; keep the warm state." },
  { href: "/guides/linux-maintenance-window-without-downtime-cost", group: "Moving workloads", title: "The Maintenance Window Playbook", blurb: "Evacuate, patch, return — with the rollback drill before the window." },
  { href: "/guides/air-gapped-workload-migration", group: "Moving workloads", title: "Workload Migration in Air-Gapped Environments", blurb: "Local-only operation, offline install, peer-only data paths." },
  { href: "/guides/burst-ci-workers-from-checkpoint", group: "CI and fleets", title: "Burst CI Capacity from a Warm Checkpoint", blurb: "The CI wedge: warm once, run many, on hardware you own." },
  { href: "/guides/clone-checkpoint-into-n-workers", group: "CI and fleets", title: "Clone One Checkpoint Into N Workers", blurb: "Identity, isolation, rolling seeds, and where clone ends." },
  { href: "/guides/fleet-checkpoint-policy", group: "CI and fleets", title: "Designing a Fleet Checkpoint Policy", blurb: "Interval, keep_last, RPO math, and the rollout checklist." },
  { href: "/guides/warm-standby-linux-workloads", group: "CI and fleets", title: "Warm Standby for Linux Workloads: Prove Death, Then Restore", blurb: "Replicated checkpoints, deliberate trigger, fencing honesty." },
  { href: "/guides/checkpoint-security-and-encryption", group: "CI and fleets", title: "Checkpoint Security: Encryption, Trust Anchors, and Threat Model", blurb: "AES-256-GCM chunks, signed manifests, agent-held keys, boundaries." },
];

const COMPARES: Array<{ href: string; title: string }> = [
  { href: "/compare/criu-scripts-vs-shift", title: "Hand-Rolled CRIU Scripts vs SHIFT" },
  { href: "/compare/vm-live-migration-vs-shift", title: "VM Live Migration vs Process-Level Migration" },
  { href: "/compare/restart-and-rewarm-vs-checkpoint", title: "Restart-and-Rewarm vs Checkpoint/Restore" },
  { href: "/compare/rsync-and-scatter-vs-shift", title: "rsync + Homemade Scripts vs a Migration State Machine" },
  { href: "/compare/shift-vs-manual-checkpointing", title: "Manual Checkpointing vs SHIFT" },
];

export default function GuidesIndexPage() {
  const groups = Array.from(new Set(GUIDES.map((g) => g.group)));
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container>
            <div className="mb-12 max-w-3xl">
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
                Guides
              </p>
              <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Move running state. Keep the source.
              </h1>
              <p className="text-text-secondary text-lg leading-relaxed">
                Practical guides for checkpoint/restore and workload mobility on Linux —
                with measured numbers, real commands, and an honest-edges section on every
                page. New here? Start with the{" "}
                <Link href="/guides/linux-workload-mobility" className="text-text hover:text-accent transition-colors">
                  pillar guide
                </Link>{" "}
                or run the{" "}
                <Link href="/docs/ci-burst" className="text-text hover:text-accent transition-colors">
                  CI-burst proof
                </Link>
                .
              </p>
            </div>

            {groups.map((group) => (
              <div key={group} className="mb-12">
                <h2 className="text-sm font-mono uppercase tracking-widest text-text-muted mb-5">
                  {group}
                </h2>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                  {GUIDES.filter((g) => g.group === group).map((guide) => (
                    <Link
                      key={guide.href}
                      href={guide.href}
                      className="block rounded-lg border border-border bg-bg-elevated p-5 hover:border-accent/40 transition-colors"
                    >
                      <h3 className="text-base font-medium text-text mb-2">{guide.title}</h3>
                      <p className="text-sm text-text-secondary">{guide.blurb}</p>
                    </Link>
                  ))}
                </div>
              </div>
            ))}

            <div>
              <h2 className="text-sm font-mono uppercase tracking-widest text-text-muted mb-5">
                Comparisons
              </h2>
              <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                {COMPARES.map((compare) => (
                  <Link
                    key={compare.href}
                    href={compare.href}
                    className="block rounded-lg border border-border bg-bg-elevated p-5 hover:border-accent/40 transition-colors"
                  >
                    <h3 className="text-base font-medium text-text">{compare.title}</h3>
                  </Link>
                ))}
              </div>
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
