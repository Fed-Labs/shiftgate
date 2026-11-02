import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Designing a Fleet Checkpoint Policy",
  description:
    "Checkpoint policy for fleets: interval and keep_last trade-offs, RPO thinking, retention and lineage, storage math, and a checklist for rolling policies out across dozens of machines.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Designing a Fleet Checkpoint Policy"
      lede="One workload with a checkpoint is a demo. A fleet with a policy is an operation. This guide covers the two knobs that matter (interval and keep_last), how to reason about RPO and storage, and the rollout checklist."
      sections={[
        {
          heading: "The two knobs",
          blocks: [
            { kind: "code", body: "shiftgate workload policy runner --every 5m --keep 5\nshiftgate workload policy runner --off" },
            { kind: "bullets", items: [
              "--every: how often the agent checkpoints the workload automatically (floor: 10 seconds; measured from the newest checkpoint, so it survives agent restarts).",
              "--keep: how many checkpoints are retained — the rolling window of restore points.",
              "The workload keeps running: periodic checkpoints are capture-and-resume, not maintenance windows.",
            ] },
          ],
        },
        {
          heading: "RPO is the policy; RTO is the restore",
          blocks: [
            { kind: "p", text: "Your worst-case data loss in an incident is the interval since the last checkpoint. Your recovery time is the restore (bench: 2.27–2.77 s wall including resume and health validation — plus whatever your service needs to be ready). Set the interval from the loss you can tolerate, not from what feels aggressive; a 5-minute policy on a build worker and a 5-minute policy on a stateful queue are different decisions." },
            { kind: "table", head: ["Workload class", "Interval", "keep_last", "Why"], rows: [
              ["CI runner / worker", "30m", "3–5", "Warm seed freshness; cheap storage."],
              ["Stateful service", "5m", "5–10", "RPO minutes; restore-tested monthly."],
              ["Long batch job", "15m", "4–8", "Protect the accumulated work, not the second."],
            ] },
          ],
        },
        {
          heading: "Storage and lineage",
          blocks: [
            { kind: "p", text: "Checkpoints are immutable and content-addressed: incremental checkpoints (--parent) move only what changed, and lineage records the family so you can reason about restore chains. keep_last prunes the oldest beyond the count. Hosted storage is quota'd fail-closed per plan (free tier 10 GB) — deleting a checkpoint removes its manifest; mirrored chunk cleanup on paid plans is an explicit reclaim operation." },
          ],
        },
        {
          heading: "Fleet rollout checklist",
          blocks: [
            { kind: "bullets", items: [
              "Default policy per workload class (the table above is a starting point, not a law).",
              "One restore drill per class per month: restore the newest checkpoint on a scratch host and time it.",
              "Alert on checkpoint age: a workload whose newest checkpoint is older than 2× its interval is unprotected.",
              "Pin your version expectations — pre-1.0 state formats follow the versioning promise; policy semantics ride along.",
              "Keep policy changes auditable: who changed what, when (the platform's audit log covers this on Business).",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "Periodic policies capture and resume; the capture freeze is brief but real (bench checkpoint: 914–973 ms).",
        "Pruning is keep_last only — there is no time-based retention in the agent policy today.",
        "Deleting checkpoints does not instantly free mirrored bytes on free-tier hosted storage; paid plans reclaim explicitly.",
        "Policy is per-workload; there is no fleet-wide policy template in the product yet — script it via the API/CLI.",
      ]}
      faqs={[
        { q: "What happens when the agent restarts?", a: "The schedule is measured from the newest checkpoint and survives agent restarts — the next capture happens one interval after the last one, not on a wall clock that drifted." },
        { q: "Can a policy restore automatically?", a: "Policies checkpoint; restore is a deliberate act (or the failover path, which restores when the source is provably dead — with the fencing caveat). Do not confuse backup with recovery." },
        { q: "How do I see what a checkpoint contains?", a: "checkpoint inspect shows the manifest: kind, lineage, sizes, what was captured — including out-of-root dependencies that did not travel." },
      ]}
      cta={{ heading: "Set your first policy", body: "One workload, one interval, one restore drill — then scale the habit, not the hope." }}
      related={[
        { href: "/guides/clone-checkpoint-into-n-workers", label: "Clone one checkpoint into N workers" },
        { href: "/guides/warm-standby-linux-workloads", label: "Warm standby for Linux workloads" },
        { href: "/use-cases/failover-and-dr", label: "Use case: failover and DR" },
      ]}
    />
  );
}
