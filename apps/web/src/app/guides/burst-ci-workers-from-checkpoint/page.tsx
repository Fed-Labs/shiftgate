import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Burst CI Capacity from a Warm Checkpoint",
  description:
    "Warm CI workers without the warm-up bill: checkpoint one prepared runner state and clone it into N independent workers in seconds — burst CI capacity on hardware you own, with measured times.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Burst CI Capacity from a Warm Checkpoint"
      lede="Every self-hosted CI fleet hits the same wall: the queue forms because re-warming a runner costs more than the job does. The fix is to stop paying the warm-up bill per worker — prepare the state once, checkpoint it, and clone it into N independent workers."
      sections={[
        {
          heading: "The warm-up bill",
          blocks: [
            { kind: "p", text: "A useful CI worker is not a booted VM; it is a process environment with fetched dependencies, warm caches, a populated toolchain, and a clean working root. Cold workers pay for all of that before they compile a line — and on a burst day, they pay it while the queue grows." },
            { kind: "p", text: "Checkpoint/restore inverts the economics: pay the warm-up once, capture the state, and reproduce it per worker from the checkpoint instead of from a cold boot." },
          ],
        },
        {
          heading: "The burst path (reproducible, measured)",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate workload create runner --path ~/ci --start -- ./warm-runner.sh\nshiftgate checkpoint create runner --leave-running\nshiftgate checkpoint list --workload runner\nshiftgate clone CHECKPOINT_ID --count 10 --prefix ci\nshiftgate clone list" },
            { kind: "p", text: "One warm checkpoint (bench: 914–973 ms to capture) becomes 10 independent running workloads — each with its own root and identity, reflink-copied where the filesystem allows. The full proof with per-step durations: docs/ci-burst.md and the unattended script scripts/demo-ci-burst.sh." },
            { kind: "p", text: "This is the demonstration path for a reason: it is one 60-second loop that shows the promise twice — the burst (no cold starts) and, with the failed move below, the safety invariant." },
          ],
        },
        {
          heading: "Keeping the source safe while workers churn",
          blocks: [
            { kind: "p", text: "Workers come and go; the prepared state should not die with them. Keep the checkpoint as the seed, clone workers from it, and roll forward with a new checkpoint when the environment changes (new dependency cache, new toolchain). Clones are same-machine; if you need the pool on another host, migrate the seed workload there and clone from its checkpoint." },
            { kind: "code", body: "shiftgate migrate runner --to ci-2 --preflight\nshiftgate migrate runner --to ci-2 --mode live --pre-copy-passes 4   # paid plan feature\nshiftgate migrate runner --to https://127.0.0.1:1                       # deliberate failure: source survives" },
          ],
        },
        {
          heading: "Sizing the burst",
          blocks: [
            { kind: "bullets", items: [
              "Disk: each worker gets its own root (reflink-copied where possible). Budget disk for N workers plus the checkpoint store.",
              "Clone time scales with N and the state size; --count and --parallel shape the run.",
              "Checkpoint freshness: seed from the newest good checkpoint; a periodic policy (keep_last) keeps a rolling seed automatically.",
              "Same-machine by definition — cross-machine pool = migrate + clone at the destination.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "Clone is same-machine; each worker is an independent workload with its own root and identity.",
        "Linux x86_64, privileged agent, CRIU-capable kernel; criu check must pass.",
        "A move to another host is a migrate: a measured freeze reported as downtime; live mode is a paid plan feature on every path.",
        "A failed move never destroys the source — proven by failing moves on purpose in the e2e suite.",
        "Pre-1.0: worker automation should pin versions (see the versioning promise).",
      ]}
      faqs={[
        { q: "How many workers can one checkpoint seed?", a: "The demo uses 10 and 50; your bound is disk and memory on the host. The clone command takes --count N and reports each spawned workload." },
        { q: "Do workers share the checkpoint's memory?", a: "No sharing trickery: each worker restores its own copy from the checkpoint (reflink-copied storage where the filesystem allows). Each is fully independent — crash one and the others continue." },
        { q: "Can I burst onto a second machine?", a: "Migrate the seed workload to the target host first, then clone there. Or run a prepared checkpoint per host." },
        { q: "What breaks when the build environment changes?", a: "The seed goes stale. Re-checkpoint the prepared runner after dependency/toolchain updates — that is the point of a rolling keep_last policy." },
      ]}
      cta={{ heading: "Run the CI-burst proof", body: "Fresh box, one script, N=10 workers and a proven rollback — with per-step measured times." }}
      related={[
        { href: "/guides/clone-checkpoint-into-n-workers", label: "Clone one checkpoint into N workers" },
        { href: "/use-cases/ci-burst-clusters", label: "Use case: CI burst clusters" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
