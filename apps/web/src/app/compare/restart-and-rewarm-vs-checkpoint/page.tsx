import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Restart-and-Rewarm vs Checkpoint/Restore",
  description:
    "Restart-and-rebuild vs checkpoint/restore: what a re-warm actually costs in minutes and dollars, when a restart is the right answer anyway, and the measured restore alternative.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Comparison"
      title="Restart-and-Rewarm vs Checkpoint/Restore"
      lede="The default recovery plan in most shops is restart and re-warm: redeploy, refetch, rebuild caches, re-attach. It works — and it silently taxes every maintenance window, every rebalance, and every burst. Here is the arithmetic."
      sections={[
        {
          heading: "The two strategies side by side",
          blocks: [
            { kind: "table", head: ["Today's approach", "What it costs", "SHIFT's frame"], rows: [
              ["Restart-and-rebuild (redeploy, re-warm, re-attach)", "Minutes to hours of warm-up; lost in-memory state; re-run of expensive work", "State moves as-is: a warm checkpoint restores in seconds (see benchmarks), and clone turns one warm checkpoint into many"],
            ] },
          ],
        },
        {
          heading: "Counting the re-warm",
          blocks: [
            { kind: "bullets", items: [
              "Dependency fetch + toolchain warm-up: minutes per worker, per event — multiplied by every burst worker and every emergency restart.",
              "Cache rebuilds: the first jobs after a restart run cold; tail latency shows up as 'the queue is weird today'.",
              "Lost in-memory work: batch jobs, training runs, encodes — anything without a resume point pays full price.",
              "Human cost: restart is also the moment the runbook gets discovered at 3 a.m.",
            ] },
            { kind: "p", text: "Against that: a checkpoint restore is 2.27–2.77 s wall on the bench (including resume and health validation), and a checkpoint of a ~100 MiB-heap workload captured in 914–973 ms while the process kept running." },
          ],
        },
        {
          heading: "When restart is the right answer",
          blocks: [
            { kind: "bullets", items: [
              "Stateless services behind a load balancer — nothing to preserve.",
              "When the image itself is the fix (dependency drift, corrupted environment).",
              "When the restore path has not been drilled — an untested restore is a restart with extra steps.",
            ] },
          ],
        },
        {
          heading: "The hybrid most teams land on",
          blocks: [
            { kind: "p", text: "Keep restart for stateless tiers; checkpoint the expensive warm states (CI runners, caches, batch jobs) and restore/clone from them. The CI-burst pattern is exactly this: one warm checkpoint, N workers, no re-warm — and when a move fails, the source keeps running rather than turning a move into a restart." },
            { kind: "code", body: "shiftgate checkpoint create runner --leave-running\nshiftgate clone CHECKPOINT_ID --count 10 --prefix ci" },
          ],
        },
      ]}
      honestEdges={[
        "Checkpoint/restore adds tooling and a privileged agent; restart adds nothing to install. The trade is operational, not only technical.",
        "A restore is measured (seconds), not instant; a live move is a measured freeze reported as downtime.",
        "Linux x86_64, CRIU-capable kernel; some workloads genuinely cannot checkpoint (see limitations).",
        "Pre-1.0: drill restores before trusting them in the runbook.",
      ]}
      faqs={[
        { q: "What is 're-warm' exactly?", a: "Everything a restarted process must redo before it is useful: fetch dependencies, rebuild caches, re-scan state, re-attach. It is usually the dominant cost of a restart and rarely measured." },
        { q: "Does checkpointing slow my workload down?", a: "Only during capture — the dump freezes briefly (bench: 914–973 ms) and resumes automatically with --leave-running. Periodic policies schedule captures without operator action." },
        { q: "Can a checkpoint replace backups?", a: "No. Checkpoints are operational restore points with a retention window; backups are a separate disaster-recovery discipline. Do not merge the two runbooks." },
      ]}
      cta={{ heading: "Price your own re-warm", body: "Time one cold restart of your heaviest worker, then time a restore from its checkpoint." }}
      related={[
        { href: "/guides/burst-ci-workers-from-checkpoint", label: "Burst CI capacity from a warm checkpoint" },
        { href: "/compare/vm-live-migration-vs-shift", label: "VM live migration vs process-level migration" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
