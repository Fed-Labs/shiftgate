import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Lazy Restore Explained: Start Before Your Memory Arrives",
  description:
    "How CRIU lazy restore works: the lazy-pages daemon, time-to-first-execution vs full restore wall, when laziness wins, and when it does not (with the measured numbers).",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="CRIU deep dive"
      title="Lazy Restore Explained: Start Before Your Memory Arrives"
      lede="Lazy restore starts the process before its memory is fully materialized — a lazy-pages daemon serves faults on demand and streams the rest in the background. The measured story is more interesting than the pitch: on fast local disk, laziness can even lose."
      sections={[
        {
          heading: "The mechanism",
          blocks: [
            { kind: "code", body: "shiftgate restore CHECKPOINT_ID --lazy\n# time to first execution: 142 ms (lazy: memory pages are still streaming in on demand)" },
            { kind: "bullets", items: [
              "The restore maps the memory image but defers materializing pages.",
              "A lazy-pages daemon (CRIU's) serves page faults on demand over a local socket and streams the bulk behind the running process.",
              "First execution wins: the process exists and begins working while its memory catches up.",
            ] },
          ],
        },
        {
          heading: "The measured numbers (bench box)",
          blocks: [
            { kind: "table", head: ["Measure", "Lazy", "Eager"], rows: [
              ["Time to first execution", "142 – 179 ms", "96 – 131 ms"],
              ["Restore wall", "2.23 – 2.51 s", "2.10 – 2.36 s"],
              ["Checkpoint after restore", "1.104 – 1.209 s", "914 – 973 ms"],
            ] },
            { kind: "p", text: "The honest headline from docs/benchmarks.md: lazy restored slower to first execution than eager on this workload. Local disk already delivered the image in ~100 ms, so there was nothing for laziness to hide — while the daemon's startup and socket handshake sat on the critical path. Where lazy wins is exactly where bulk transfer would otherwise block startup: slow storage, cross-machine restore, huge heaps." },
          ],
        },
        {
          heading: "When to reach for it",
          blocks: [
            { kind: "bullets", items: [
              "Huge memory images on slow or remote backing: first execution begins while pages stream.",
              "Failover restores where perceived availability matters more than steady-state throughput.",
              "Not for: small states on fast local disk (measure first), or workloads that touch most of their heap immediately (the faults become the critical path anyway).",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "Lazy pages rely on the lazy-pages daemon; the checkpoint after a lazy restore is measurably slower (1.10–1.21 s vs 914–973 ms on the bench).",
        "The benchmark's lazy first-execution was slower than eager — we publish that rather than hide it.",
        "restore --lazy is free on every tier; it is a restore mode, not a plan gate.",
        "Linux x86_64, privileged agent, CRIU-capable kernel.",
      ]}
      faqs={[
        { q: "Is lazy restore always faster?", a: "No. On fast local disk it can be slower to first execution — the daemon handshake costs more than the eager load. Measure both on your state." },
        { q: "Does lazy change the freeze during a migrate?", a: "It changes the restore side of the move. The freeze (downtime) definition is unchanged: freeze → health-validated restore." },
        { q: "Can I checkpoint immediately after a lazy restore?", a: "Yes — and on the bench it takes 1.10–1.21 s vs 914–973 ms normally, because pages are still streaming." },
      ]}
      cta={{ heading: "Time both paths", body: "Restore the same checkpoint eager and lazy on your hardware; the numbers decide." }}
      related={[
        { href: "/guides/criu-checkpoint-restore-tutorial", label: "CRIU checkpoint/restore tutorial" },
        { href: "/guides/criu-pre-dump", label: "Pre-dump passes: buying down the frozen window" },
        { href: "/docs/benchmarks", label: "The benchmark harness" },
      ]}
    />
  );
}
