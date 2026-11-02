import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Failover and DR: Prove Death, Then Restore",
  description:
    "Use case: failover and disaster recovery. Warm standby with replicated checkpoints, manual trigger drills, honest RPO/RTO math — and the fencing limitation stated up front.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Use case"
      title="Failover and DR: Prove Death, Then Restore"
      lede="The problem: a service dies with its host at the worst time, and the recovery plan is a restore you have never timed. The pattern: keep a warm standby fed with replicated checkpoints, restore on your command — or automatically when death is proven and a fencing provider is configured (disabled by default) — and drill it monthly."
      sections={[
        {
          heading: "The three steps",
          blocks: [
            { kind: "code", body: "shiftgate workload failover app --to https://standby:8443 --keep 3\nshiftgate workload policy app --every 5m --keep 5\nshiftgate standby trigger app    # the drill, and the real thing" },
            { kind: "p", text: "Every scheduled checkpoint replicates to the standby agent. Automatic restore fires only when the source is provably dead — unreachable and silent on the control plane — and a fencing provider positively confirms it is fenced (the default provider is disabled, so the automatic path is fail-closed). Manual trigger covers everything else." },
          ],
        },
        {
          heading: "The honest DR math",
          blocks: [
            { kind: "bullets", items: [
              "RPO = the replication interval (your policy: bench-scale checkpoints every 5 minutes is routine).",
              "RTO = restore wall (bench: 2.27–2.77 s including resume and health validation) + your service's readiness.",
              "Drill both numbers monthly; an unmeasured RTO is a hope.",
            ] },
          ],
        },
        {
          heading: "Limitations (the ones that matter at 3 a.m.)",
          blocks: [
            { kind: "bullets", items: [
              "Automatic restore requires a fencing provider, which is disabled by default (fail-closed). The manual trigger is explicitly unsafe: a partitioned source can return and leave two live copies. Name the human who decides death.",
              "Failover/standby is a paid capability (HA Pack / Business); checkpoints and cold migration are free.",
              "A restore is a measured operation — not instant recovery.",
              "This is operational recovery, not a substitute for off-site backups.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "The split-brain window of the manual trigger is real until fencing is configured — automatic restore stays fail-closed without it. We would rather you design for it than discover it.",
        "A failed move never destroys the source, but failover assumes the source is already gone — different failure model, same honesty bar.",
        "Linux x86_64, privileged agents, CRIU kernels on both ends.",
      ]}
      faqs={[
        { q: "Is this backup?", a: "No. It is operational recovery with a bounded RPO. Keep off-site backups and a restore drill as a separate discipline." },
        { q: "What triggers automatic failover?", a: "The source being unreachable and silent on the control plane, with a fencing provider confirming the source is fenced — the default fencing provider is disabled, so without configuration the path is manual: standby trigger. Ambiguity goes to a human." },
        { q: "How do I rehearse?", a: "Failover drill: trigger against the standby, measure the restore, fail back deliberately. Do it monthly — the runbook is the product." },
      ]}
      cta={{ heading: "Run the failover drill", body: "Two machines, one policy, one trigger — and a stopwatch." }}
      related={[
        { href: "/guides/warm-standby-linux-workloads", label: "Warm standby for Linux workloads" },
        { href: "/guides/fleet-checkpoint-policy", label: "Designing a fleet checkpoint policy" },
        { href: "/use-cases/edge-fleets", label: "Use case: edge fleets" },
      ]}
    />
  );
}
