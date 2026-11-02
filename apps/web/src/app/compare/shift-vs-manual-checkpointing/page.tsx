import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Manual Checkpointing vs SHIFT",
  description:
    "Manual checkpointing (ad-hoc criu dumps, unowned restore points) vs checkpoint operations: policy, lineage, integrity, and who runs the restore when it matters.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Comparison"
      title="Manual Checkpointing vs SHIFT"
      lede="Many teams already checkpoint — manually: someone runs a dump before the risky thing, the image lands somewhere, nobody is sure it restores. This comparison is about the operational gap between having checkpoints and depending on them."
      sections={[
        {
          heading: "Manual vs operational, side by side",
          blocks: [
            { kind: "table", head: ["Today's approach", "What it costs", "SHIFT's frame"], rows: [
              ["Manual checkpointing (ad-hoc dumps, unowned restore points)", "Restore points nobody has tested; no policy, no lineage, no integrity story; the dump before the big migration is the only plan", "Policy-driven capture (interval + keep_last), immutable content-addressed checkpoints with lineage, authenticated restore, and a commit gate for every move"],
            ] },
          ],
        },
        {
          heading: "The four gaps in the manual habit",
          blocks: [
            { kind: "bullets", items: [
              "Freshness: the checkpoint is as old as the last time someone remembered. A policy makes it as old as the interval.",
              "Trust: an untested restore point is a rumor. Authenticated chunks + signed manifests + a drill schedule make it a fact.",
              "Provenance: which checkpoint descends from which, what changed, what did not travel (out-of-root dependencies are reported, not assumed).",
              "The move: checkpoints alone do not evacuate a host or burst a pool — the state machine does.",
            ] },
            { kind: "code", body: "shiftgate workload policy runner --every 5m --keep 5\nshiftgate checkpoint list --workload runner\nshiftgate checkpoint inspect CHECKPOINT_ID   # kind, lineage, sizes, dependencies" },
          ],
        },
        {
          heading: "What you keep by hand",
          blocks: [
            { kind: "p", text: "Judgment stays manual: which workloads deserve policy, when to trigger a failover, when the compatibility report says no. The tooling's job is to make those decisions reversible — a failed move rolls back with the source running, so the cost of trying is bounded." },
          ],
        },
      ]}
      honestEdges={[
        "Manual checkpoints are still real checkpoints — this is about operating them, not magic.",
        "Pruning is keep_last; deleting checkpoints does not instantly free mirrored bytes on free-tier hosted storage (paid plans reclaim explicitly).",
        "A policy captures and resumes; it does not replace backups or disaster-recovery drills.",
        "Linux x86_64, privileged agent, CRIU-capable kernel; pre-1.0.",
      ]}
      faqs={[
        { q: "Can I still take checkpoints by hand?", a: "Yes — checkpoint create is the manual trigger; policies are optional automation on top." },
        { q: "What does lineage give me?", a: "The family tree of incremental checkpoints: what each restore point descends from and what changed along the way — the difference between a restore point and a provenance chain." },
        { q: "How do I know a checkpoint will restore?", a: "Restore it. The honest answer is a drill — restore the newest checkpoint per workload class monthly and time it." },
      ]}
      cta={{ heading: "Turn the habit into a policy", body: "One interval, one keep count, one monthly drill." }}
      related={[
        { href: "/guides/fleet-checkpoint-policy", label: "Designing a fleet checkpoint policy" },
        { href: "/compare/criu-scripts-vs-shift", label: "Hand-rolled CRIU scripts vs SHIFT" },
        { href: "/docs/checkpoints", label: "The checkpoints docs page" },
      ]}
    />
  );
}
