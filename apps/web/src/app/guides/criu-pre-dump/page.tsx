import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Pre-Dump Passes: Buying Down the Frozen Window",
  description:
    "How CRIU pre-dump passes shrink live-migration downtime: iterative dirty-page transfer, choosing --pre-copy-passes, convergence, and the measured diminishing returns.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="CRIU deep dive"
      title="Pre-Dump Passes: Buying Down the Frozen Window"
      lede="Live migration's whole trick is arithmetic: move most of the memory while the workload runs, then freeze only long enough to carry the delta. Pre-dump passes are how the bulk gets moved outside the freeze."
      sections={[
        {
          heading: "How pre-copy converges",
          blocks: [
            { kind: "bullets", items: [
              "Pass 1: dump the full memory image while the workload keeps running; transfer it.",
              "Pass N: dump only pages dirtied since the previous pass; transfer the smaller delta.",
              "Final dump under freeze: whatever changed since the last pass — this is the downtime payload.",
            ] },
            { kind: "code", body: "shiftgate migrate app --to dst --mode live --pre-copy-passes 4" },
            { kind: "p", text: "Each pass trades wall time for frozen time. Convergence depends on the workload's dirty-page rate: a workload rewriting its whole heap every pass never converges and the cap (--pre-copy-passes 0–16) bounds the loop." },
          ],
        },
        {
          heading: "What the bench shows",
          blocks: [
            { kind: "table", head: ["State", "Moved before freeze (live)", "Moved inside freeze (live)", "Downtime live vs cold"], rows: [
              ["~17 MB", "~719 KB", "~16.8 MB", "3.20 – 3.29 s vs 3.75 – 3.91 s"],
              ["~118 MB", "~101 MB", "~16.8 MB", "3.39 – 3.52 s vs 4.61 – 5.50 s"],
            ] },
            { kind: "p", text: "The shape to notice (docs/benchmarks.md): the first pass moved the bulk outside the freeze for the large state, so the frozen payload stayed small — but downtime is dominated by the final dump plus the ~2.3–2.8 s restore wall. Pre-copy buys down the transfer term; it cannot buy down the restore term." },
          ],
        },
        {
          heading: "Choosing the pass count",
          blocks: [
            { kind: "bullets", items: [
              "Small states: 0–1 passes. The freeze is already short; extra passes are overhead.",
              "Large heaps with modest write rates: 2–6 passes. The deltas shrink each round.",
              "Write-heavy workloads: watch for non-convergence; the cap stops the loop and the final delta carries whatever churned.",
              "Always measure: the migration record reports the actual downtime for your workload.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "A live move is a freeze measured and reported as downtime — pre-copy minimizes it, it does not eliminate it.",
        "Loopback numbers overstate the win: across a real network the transfer term grows with state size.",
        "Live mode is a paid plan feature on every path, including peer-to-peer.",
        "Linux x86_64, privileged agent, CRIU-capable kernels on both ends.",
      ]}
      faqs={[
        { q: "What is a dirty page?", a: "A memory page written since the last dump. Pre-copy passes transfer only the dirtied set, so each round moves less — if the workload cooperates." },
        { q: "Why cap the passes?", a: "A pathological write rate would otherwise loop forever chasing convergence. The cap bounds the wall time; the final freeze carries the remainder." },
        { q: "Does pre-copy affect the source?", a: "The workload keeps running during pre-dump passes; it freezes only for the final dump. CPU overhead during passes is real — benchmark on production-shaped load." },
      ]}
      cta={{ heading: "Measure your convergence", body: "Migrate a real workload with 0, 2, and 4 passes; the migration record tells you what each bought." }}
      related={[
        { href: "/guides/live-migration-without-shared-storage", label: "Live migration without shared storage" },
        { href: "/guides/criu-lazy-restore", label: "Lazy restore explained" },
        { href: "/docs/benchmarks", label: "The benchmark harness" },
      ]}
    />
  );
}
