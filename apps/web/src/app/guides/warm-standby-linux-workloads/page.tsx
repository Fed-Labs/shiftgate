import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Warm Standby for Linux Workloads: Prove Death, Then Restore",
  description:
    "Warm standby for self-hosted Linux services: replicate checkpoints to a standby agent, restore when the source is provably dead or on command — and the fencing honesty you need before you trust it.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Warm Standby for Linux Workloads: Prove Death, Then Restore"
      lede="A warm standby is a running conversation, not a backup folder: scheduled checkpoints replicate to a standby agent, and the newest copy restores when the source is provably dead — unreachable and silent — or when you pull the trigger yourself."
      sections={[
        {
          heading: "The pattern",
          blocks: [
            { kind: "code", body: "shiftgate workload failover demo --to https://standby:8443 --keep 3\nshiftgate workload policy demo --every 5m --keep 5\nshiftgate standby list\nshiftgate standby trigger demo            # manual failover\nshiftgate standby trigger demo --lazy    # restore with lazy pages" },
            { kind: "p", text: "The failover policy replicates every scheduled checkpoint to the standby agent. Automatic restore fires when the source is provably dead — unreachable and silent on the control plane — and manual trigger is always available for drills." },
          ],
        },
        {
          heading: "Prove death before you restore",
          blocks: [
            { kind: "p", text: "The dangerous failure in any failover story is the partitioned source: the source is alive but unreachable, the standby restores, and now two copies of one workload exist. The current honest position: detection is reachability + control-plane silence, and automatic restore additionally requires a fencing provider (disabled by default — fail-closed), so the always-available path is the manual trigger — which is explicitly unsafe on a source that is merely partitioned. Treat failover as a human-confirmed recovery tool, and configure fencing before relying on the automatic path." },
            { kind: "bullets", items: [
              "Drill monthly: trigger failover on purpose, measure the restore, fail back deliberately.",
              "Keep the RPO explicit: your worst-case loss is the interval between replicated checkpoints.",
              "Name the split-brain owner: who decides a source is dead when the network lies?",
            ] },
          ],
        },
        {
          heading: "What a restore costs",
          blocks: [
            { kind: "p", text: "Bench restore wall is 2.27–2.77 s including resume and health validation (eager); lazy restore starts execution in 142–179 ms and streams memory in behind it. RPO is your checkpoint interval (policy floor: every 10s, practical: minutes); RTO is restore plus your service's own readiness." },
          ],
        },
        {
          heading: "Failover vs migration",
          blocks: [
            { kind: "table", head: ["", "Migration", "Failover / standby"], rows: [
              ["Trigger", "Operator, planned", "Source death or operator"],
              ["Source state", "Retired after commit (failed move → rollback)", "Assumed dead (automatic restore needs fencing)"],
              ["Free tier", "Cold yes; live paid", "HA Pack / Business (paid)"],
              ["Drill", "preflight + migrate", "standby trigger"],
            ] },
          ],
        },
      ]}
      honestEdges={[
        "No fencing yet: a partitioned source can return while a standby copy is live. Documented, not marketed away.",
        "RPO = checkpoint replication interval; there is no synchronous replication.",
        "Warm-standby failover is a paid capability (HA Pack on Pro, included on Business/Enterprise); checkpoints and cold migration are free.",
        "Linux x86_64, privileged agents, CRIU kernels on both ends.",
        "A restore is a measured operation (bench: 2.27–2.77 s wall) — not instant, and not zero-recovery-time.",
      ]}
      faqs={[
        { q: "What triggers automatic failover?", a: "The source being provably dead — unreachable and silent on the control plane — with a fencing provider confirming the source is fenced (disabled by default, so the automatic path is fail-closed). Anything ambiguous is a human decision via standby trigger." },
        { q: "How much data can I lose?", a: "At most the interval since the last replicated checkpoint (your policy's keep/interval). It is a warm standby, not synchronous mirroring." },
        { q: "Can I fail back?", a: "Yes — treat fail-back as another migration once the original host is healthy, and drill it before you need it." },
        { q: "Is failover free?", a: "The failover/standby capability is a paid plan feature (HA Pack on Pro). The underlying checkpoint, restore, and cold migration paths are free." },
      ]}
      cta={{ heading: "Run a failover drill", body: "Two machines, a policy, one deliberate trigger — measure your RTO before an incident does." }}
      related={[
        { href: "/use-cases/failover-and-dr", label: "Use case: failover and DR" },
        { href: "/guides/fleet-checkpoint-policy", label: "Designing a fleet checkpoint policy" },
        { href: "/guides/criu-lazy-restore", label: "Lazy restore explained" },
      ]}
    />
  );
}
