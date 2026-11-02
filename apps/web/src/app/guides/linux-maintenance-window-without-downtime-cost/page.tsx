import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "The Maintenance Window Playbook",
  description:
    "Evacuate a Linux host for maintenance without losing the running job: the evacuation checklist, measured freeze vs rebuild cost, the rollback drill, and the war-story template for your own postmortem file.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="The Maintenance Window Playbook"
      lede="Kernel patches, hardware swaps, power work, disk migrations — the maintenance window is a recurring tax. The playbook that shrinks it: move the running workload off the host, patch, bring it back, and measure every freeze instead of apologizing for it."
      sections={[
        {
          heading: "The evacuation, before the window opens",
          blocks: [
            { kind: "code", body: "shiftgate doctor                              # both hosts\nshiftgate connect spare --to https://spare:8443 --machine-id SPARE_ID\nshiftgate migrate job-01 --to spare --preflight  # compatibility first\nshiftgate migrate job-01 --to spare              # or --mode live (paid plans)\nshiftgate status MIGRATION_ID                    # downtime: measured, reported" },
            { kind: "p", text: "Do the preflight days before the window — compatibility problems should surface when you have time, not at 2 a.m. The migration record gives you the actual downtime number to put in the change ticket." },
          ],
        },
        {
          heading: "Freeze vs rebuild: the arithmetic",
          blocks: [
            { kind: "p", text: "The bench box restores a checkpoint in 2.27–2.77 s wall and moves an ~17 MB state with 3.2–3.9 s downtime (live vs cold on loopback). Your real comparison is not against zero — it is against re-warm: cache rebuilds, dependency fetches, queue re-scans. Teams that 'evacuate' by restarting routinely lose orders of magnitude more than a measured freeze." },
            { kind: "table", head: ["Approach", "Outage shape", "Failure mode"], rows: [
              ["Restart after patch", "Minutes to hours of re-warm", "The job that cannot resume is simply lost."],
              ["Checkpoint + move", "Measured freeze (seconds on the bench)", "Failed move → rollback, source keeps running."],
              ["Clone the warm state", "Seconds to N workers", "Same-machine only."],
            ] },
          ],
        },
        {
          heading: "The rollback drill (do it before the window)",
          blocks: [
            { kind: "code", body: "shiftgate migrate job-01 --to https://127.0.0.1:1\n# refused → rolled back → source still running" },
            { kind: "p", text: "Once a quarter, fail a move on purpose and watch the invariant hold. The approved caption for your war story: 'the move failed. the source is still running.' — and the failure table in the postmortem gets shorter every time." },
          ],
        },
        {
          heading: "Bringing it back",
          blocks: [
            { kind: "bullets", items: [
              "Patch/swap the original host, doctor it again, then migrate back (or leave the workload where it landed and re-point your routing).",
              "Keep the newest checkpoint of the migrated workload as the rollback root until the window closes.",
              "Write the war story while it is fresh: estate, the move that failed before, the numbers, the rollback, the honest edges. The template is in the repo's docs and the launch kit.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "A live move is a measured freeze reported as downtime — the playbook never claims zero downtime.",
        "Live mode is a paid plan feature on every path; cold evacuation is free.",
        "Linux x86_64; both hosts privileged + CRIU-capable; preflight first, always.",
        "Clone bursts are same-machine; evacuation across hosts is a migrate.",
      ]}
      faqs={[
        { q: "How long does the window need to be?", a: "Measure it: the freeze is downtime in the migration record (seconds on the bench). Add your patch time plus the fail-back move — never the re-warm." },
        { q: "What if the move fails mid-window?", a: "The source keeps running — a failed move never destroys the source. Fix the destination, re-run the move, or abort the window with the workload alive on the original host." },
        { q: "Can I evacuate without the platform?", a: "Yes — peer-to-peer moves are agent-to-agent. The fleet features (audit, history, policy) are what the paid platform adds." },
      ]}
      cta={{ heading: "Rehearse the window", body: "Run the evacuation on a scratch workload this week; the numbers you collect become the change ticket." }}
      related={[
        { href: "/use-cases/homelab-maintenance", label: "Use case: homelab maintenance" },
        { href: "/guides/migrate-running-process-between-linux-servers", label: "Migrate a running process between Linux servers" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit (rollback beat included)" },
      ]}
    />
  );
}
