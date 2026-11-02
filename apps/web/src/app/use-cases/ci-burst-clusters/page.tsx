import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "CI Burst Clusters: Warm Runners Without the Warm-Up Bill",
  description:
    "Use case: CI burst clusters. Checkpoint one warmed runner state, clone it into N independent workers in seconds, and keep the seed rolling with a keep_last policy — burst CI on hardware you own.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Use case"
      title="CI Burst Clusters: Warm Runners Without the Warm-Up Bill"
      lede="The problem: your self-hosted runner fleet queues on busy days because every worker pays the warm-up — dependency fetch, cache rebuild, toolchain spin — before it compiles a line. The pattern: pay the warm-up once, checkpoint it, clone the state into a pool."
      sections={[
        {
          heading: "The three steps",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate workload create runner --path ~/ci --start -- ./warm-runner.sh\nshiftgate checkpoint create runner --leave-running\nshiftgate clone CHECKPOINT_ID --count 10 --prefix ci\nshiftgate clone list" },
            { kind: "p", text: "One warm checkpoint (bench: 914–973 ms to capture) becomes 10 independent workers — each with its own root and identity, reflink-copied where the filesystem allows. The full measured path: docs/ci-burst.md, driven unattended by scripts/demo-ci-burst.sh." },
          ],
        },
        {
          heading: "Keeping the pool fresh",
          blocks: [
            { kind: "bullets", items: [
              "Roll the seed: shiftgate workload policy runner --every 30m --keep 5 — a fresh prepared state without thinking about it.",
              "Rebuild the seed when the environment changes (new deps, new toolchain), then clone from the new checkpoint.",
              "Need the pool on a second host? Migrate the seed first, clone at the destination.",
            ] },
          ],
        },
        {
          heading: "Limitations (read before you plan)",
          blocks: [
            { kind: "bullets", items: [
              "Clone is same-machine; cross-machine is a migrate (cold free; live a paid plan feature).",
              "Disk and memory are per-worker: budget N roots plus the checkpoint store.",
              "A move is a measured freeze reported as downtime — never a zero-downtime claim.",
              "Linux x86_64, privileged agent, CRIU-capable kernel; pre-1.0 — pin versions in CI automation.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "Clone is same-machine.",
        "A failed move never destroys the source — the seed survives even a botched migration.",
        "Live migration is a paid plan feature on every path, including peer-to-peer.",
        "Worker clones are point-in-time restorations; refreshing the pool means cloning from a newer checkpoint.",
      ]}
      faqs={[
        { q: "How fast is the burst?", a: "The clone restores each worker in roughly the restore wall (bench: 2.27–2.77 s per restore) plus setup — measure N on your host; the demo script prints per-step times." },
        { q: "What CI systems does this work with?", a: "Anything whose runner is a Linux process: self-hosted GitHub Actions runners, Buildkite, Woodpecker, Forgejo Actions, Jenkins agents. The runner is just a warmed workload." },
        { q: "What about test isolation?", a: "Each clone gets its own root and identity — independent workloads, not shared-memory threads." },
      ]}
      cta={{ heading: "Run the CI-burst proof", body: "One script: N=10 workers from one warm checkpoint, with the rollback beat included." }}
      related={[
        { href: "/guides/burst-ci-workers-from-checkpoint", label: "Burst CI capacity from a warm checkpoint" },
        { href: "/guides/clone-checkpoint-into-n-workers", label: "Clone one checkpoint into N workers" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
