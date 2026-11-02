import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "ML/GPU Fleets: Move Warm Training and Inference State",
  description:
    "Use case: ML and GPU fleets. Evacuate a GPU node mid-run without requeueing — checkpoint the warm process, move it to another node you own, burst inference workers from one checkpoint.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Use case"
      title="ML/GPU Fleets: Move Warm Training and Inference State"
      lede="The problem: a GPU node needs maintenance or rebalancing, and the job on it requeues from zero — hours of warm caches, resident model state, and optimizer progress replaced by a queue wait. The pattern: move the running process instead of restarting it."
      sections={[
        {
          heading: "The three steps",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate checkpoint create train-01 --leave-running\nshiftgate connect gpu-west --to https://gpu-west:8443 --machine-id GPU_WEST_ID\nshiftgate migrate train-01 --to gpu-west --preflight\nshiftgate migrate train-01 --to gpu-west --mode live --pre-copy-passes 4" },
            { kind: "p", text: "Preflight answers the device question first: GPU contexts port only to matching checkpoint-capable hardware, and what cannot travel is reported before anything moves. For burst inference on one host: shiftgate clone CHECKPOINT_ID --count 4 --prefix infer." },
          ],
        },
        {
          heading: "Why it fits ML estates",
          blocks: [
            { kind: "bullets", items: [
              "Warm state is the asset: tokenizer caches, compiled kernels, data-loader state — all of it travels with the process image.",
              "Rebalancing becomes a move, not a requeue: send the warm process to the node with headroom.",
              "The source keeps running through failures — a failed move never destroys the source.",
            ] },
          ],
        },
        {
          heading: "Limitations (read before you plan)",
          blocks: [
            { kind: "bullets", items: [
              "GPU device state is portable only to matching checkpoint-capable hardware; model/process state rides along, device context may not.",
              "State size dominates downtime: multi-GB resident sets mean multi-second moves — measure with your model's real footprint.",
              "A live move is a measured freeze reported as downtime, and live mode is a paid plan feature on every path (cold is free).",
              "Linux x86_64; privileged agent; CRIU kernel.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "We do not claim transparent GPU migration — device portability is narrow and reported at preflight.",
        "A live move is a measured freeze, not zero downtime.",
        "Live mode is a paid plan feature on every path, including peer-to-peer.",
      ]}
      faqs={[
        { q: "What about my CUDA context?", a: "Device state ports only to matching checkpoint-capable hardware. Preflight says yes or no before the move; plan for the no case." },
        { q: "Can I burst inference workers from one checkpoint?", a: "Yes — clone is the burst primitive on one host; each worker is an independent process with its own identity." },
        { q: "Is this a GPU product?", a: "No — it is workload mobility whose wedge is expensive restarts; ML/GPU is one of the four shapes it serves." },
      ]}
      cta={{ heading: "Prove the move CPU-side first", body: "Then point preflight at the GPU destination and read its verdict." }}
      related={[
        { href: "/guides/gpu-node-drain-without-requeue", label: "Draining a GPU node without requeueing" },
        { href: "/guides/criu-limitations-explained", label: "CRIU limitations explained" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
