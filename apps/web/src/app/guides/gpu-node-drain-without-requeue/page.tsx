import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Draining a GPU Node Without Requeueing the Job",
  description:
    "Take a GPU box down for maintenance or rebalancing without requeueing the job from zero: checkpoint the warm process, move it to another node you own, and keep the source alive if the move fails.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Draining a GPU Node Without Requeueing the Job"
      lede="The hidden cost of GPU fleet maintenance is not the maintenance — it is the requeue: hours of warm-up, caches, and resident state thrown away because a node had to move. Process-level mobility lets the job carry its state to the next node instead."
      sections={[
        {
          heading: "What requeue actually costs",
          blocks: [
            { kind: "p", text: "A training or inference workload that has been running for hours holds a lot more than weights: tokenizer caches, compiled kernels, data-loader state, optimizer moments. Requeueing from a checkpoint file re-instantiates some of it, but the process environment rebuilds from zero — and for many pipelines the resume point is a job restart with a queue wait in front of it." },
            { kind: "p", text: "The evacuation pattern: checkpoint the running process while it works, move it to another node you own, and let it continue from the state the checkpoint caught. The only pause is the measured freeze, reported as downtime." },
          ],
        },
        {
          heading: "The drain, step by step",
          blocks: [
            { kind: "code", body: "# on the GPU node being drained\nshiftgate doctor\nshiftgate checkpoint create train-01 --leave-running\nshiftgate connect gpu-west --to https://gpu-west:8443 --machine-id GPU_WEST_ID\nshiftgate migrate train-01 --to gpu-west --preflight\nshiftgate migrate train-01 --to gpu-west --mode live --pre-copy-passes 4\nshiftgate status MIGRATION_ID" },
            { kind: "p", text: "Preflight answers the GPU question before anything moves: device state ports only to matching checkpoint-capable hardware, and what cannot travel is reported — rejected or surfaced as a limitation, never faked. If the move fails at any stage, the state machine rolls back and the job on the source node keeps running." },
          ],
        },
        {
          heading: "Rebalancing without the queue",
          blocks: [
            { kind: "p", text: "The same move answers the rebalance case: instead of draining a node into the queue and letting the scheduler re-place the work cold, move the warm process to the node with the headroom. For burst capacity on one node, a warm checkpoint clones into N workers — each an independent process with its own identity." },
            { kind: "code", body: "shiftgate clone CHECKPOINT_ID --count 4 --prefix infer" },
          ],
        },
        {
          heading: "Planning numbers",
          blocks: [
            { kind: "p", text: "State size dominates: the bench box moved an ~118 MB state with 3.39–3.52 s live downtime (cold: 4.61–5.50 s) on loopback. A multi-gigabyte GPU process will have a multi-second (or longer) transfer — pre-copy passes move the bulk outside the freeze, but re-measure with your model's actual resident state before you promise a drain window." },
          ],
        },
      ]}
      honestEdges={[
        "GPU context portability is real but narrow: matching checkpoint-capable hardware, documented caveats — we do not claim transparent GPU migration.",
        "Model/process state rides with the checkpoint; device context may not. Preflight tells you before the move.",
        "Linux x86_64, privileged agent, CRIU kernel; live mode is a paid plan feature on every path (cold is free).",
        "A live move is a measured freeze reported as downtime, not a zero-downtime event.",
      ]}
      faqs={[
        { q: "Does my GPU memory move with the process?", a: "Device state is portable only to matching checkpoint-capable hardware and is reported honestly at preflight. Plan for the case where device context does not port — the pattern still saves the CPU-side warm state." },
        { q: "What if the destination GPU is a different model?", a: "Expect the compatibility check to reject or flag it. Same-hardware-class drains are the proven path today." },
        { q: "Is this ML-specific product?", a: "No — the same machinery is the CI-burst and maintenance-window story. The wedge is state continuity for expensive restarts wherever they happen." },
      ]}
      cta={{ heading: "Try the drain pattern", body: "Start with a CPU-side workload to prove the move, then point preflight at the GPU destination." }}
      related={[
        { href: "/use-cases/ml-gpu-fleets", label: "Use case: ML/GPU fleets" },
        { href: "/guides/live-migration-without-shared-storage", label: "Live migration without shared storage" },
        { href: "/guides/criu-limitations-explained", label: "CRIU limitations explained" },
      ]}
    />
  );
}
