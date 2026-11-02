import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Research Computing: Move the Job to the Idle Node",
  description:
    "Use case: research computing and HPC. Checkpoint a running simulation or job and move it to the idle node instead of waiting in the queue — measured freezes, honest limits, no shared storage.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Use case"
      title="Research Computing: Move the Job to the Idle Node"
      lede="The problem: a lab node needs maintenance (or the queue is unfair), and the multi-hour simulation on it restarts from the last checkpoint file — or from zero. The pattern: capture the running job and move it to the node with the idle cycles."
      sections={[
        {
          heading: "The three steps",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate checkpoint create sim-42 --leave-running\nshiftgate connect idle-3 --to https://idle-3:8443 --machine-id IDLE3_ID\nshiftgate migrate sim-42 --to idle-3 --preflight\nshiftgate migrate sim-42 --to idle-3 --mode live --pre-copy-passes 4" },
            { kind: "p", text: "The migration record reports the measured freeze; the job continues from the state the checkpoint caught. For schedulers with preemption hooks (Slurm prolog/epilog), the same primitives script into the batch system." },
          ],
        },
        {
          heading: "Why research computing cares",
          blocks: [
            { kind: "bullets", items: [
              "Queue avoidance: the idle node is capacity you already pay for.",
              "Maintenance without lost compute: evacuate, patch, return — no re-run of the expensive part.",
              "Air-gap friendly: peer-to-peer moves work inside the estate; the hosted platform is optional for moving state.",
            ] },
          ],
        },
        {
          heading: "Limitations (read before you plan)",
          blocks: [
            { kind: "bullets", items: [
              "Node compatibility is real: kernel skew and CPU features are checked at preflight; mismatched nodes are rejected before the move.",
              "Job state vs job files: the declared root travels; scratch on out-of-root paths does not.",
              "A live move is a measured freeze reported as downtime; live mode is a paid plan feature (cold is free).",
              "Linux x86_64; privileged agent; CRIU-capable kernel on every participating node.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "We make no scheduler-integration claims beyond the documented Slurm hooks — no queue magic.",
        "A failed move never destroys the source: the job on the original node keeps running.",
        "Pre-1.0: rehearse with a small job before the allocation-critical one.",
      ]}
      faqs={[
        { q: "Does this replace checkpoint files in my app?", a: "No — application-level checkpoints remain the recovery path for catastrophic loss. This moves the running process for operational reasons (maintenance, rebalance)." },
        { q: "What about MPI jobs?", a: "Multi-process trees checkpoint as a unit, but tightly-coupled distributed jobs across nodes are beyond today's scope — this is a machine-to-machine move of one workload tree." },
        { q: "Can we run it fully detached?", a: "Yes — agent and CLI are local-only; peer moves never need the internet. See the air-gapped guide." },
      ]}
      cta={{ heading: "Move a test job to an idle node", body: "Preflight first; the compatibility report is where the honest answers live." }}
      related={[
        { href: "/guides/air-gapped-workload-migration", label: "Workload migration in air-gapped environments" },
        { href: "/guides/migrate-running-process-between-linux-servers", label: "Migrate a running process between Linux servers" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
