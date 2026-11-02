import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "rsync + Homemade Scripts vs a Migration State Machine",
  description:
    "rsync and scatter-shot homemade scripts vs a migration state machine: integrity, dedup transfer, ordering under failure, and the cost of maintaining your own mobility layer.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Comparison"
      title="rsync + Homemade Scripts vs a Migration State Machine"
      lede="rsync is a fine file copier and a poor migration protocol. The gap is not transfer speed — it is what happens between 'copy started' and 'workload running elsewhere', especially when something fails in the middle."
      sections={[
        {
          heading: "The two approaches side by side",
          blocks: [
            { kind: "table", head: ["Today's approach", "What it costs", "SHIFT's frame"], rows: [
              ["rsync + homemade scripts", "Copy without integrity semantics; ordering encoded in shell; failures discovered at restore; every team maintains its own layer", "One state machine with persisted stages, content-addressed encrypted chunks (dedup transfer), and a commit gate: the move is only committed after destination health validation"],
            ] },
          ],
        },
        {
          heading: "What rsync does not give you",
          blocks: [
            { kind: "bullets", items: [
              "A coherent instant: files copied while the process runs are not a checkpoint — the image must be captured frozen, then moved.",
              "Integrity semantics: a byte-identical-looking tree can still be a torn copy. Authenticated chunks fail closed on tamper or truncation.",
              "Dedup as a protocol: content-addressed chunks mean the destination that already has yesterday's state gets only the delta — as a first-class behavior, not an rsync flag you hope works.",
              "Recovery: when the script dies at 73%, what state are both machines in? Persisted stages answer that question; shell does not.",
            ] },
          ],
        },
        {
          heading: "What the state machine actually encodes",
          blocks: [
            { kind: "code", body: "shiftgate migrate app --to dst --preflight\nshiftgate migrate app --to dst\n# CREATED → DISCOVER → VALIDATE → SNAPSHOT → PREPARE → TRANSFER\n# → VERIFY → RESTORE → POST_VALIDATE → SWITCH → COMMIT" },
            { kind: "p", text: "Every arrow in that sequence is a decision your script currently encodes implicitly: when to freeze, what to copy, when to verify, when to retire the source. The commit gate makes the last decision explicit — only after the destination restores and passes health validation. Any failure before it rolls back with the source preserved." },
          ],
        },
        {
          heading: "The maintenance argument",
          blocks: [
            { kind: "p", text: "Homemade mobility layers are written in the good season and maintained in the bad one: kernel upgrades, CRIU changes, new failure modes, the one script path nobody tested. A shared layer amortizes that across every user — and its failures are tested on purpose (the e2e suite fails moves to prove the source survives)." },
          ],
        },
      ]}
      honestEdges={[
        "SHIFT is CRIU-based: Linux x86_64, privileged agent, CRIU-capable kernel — same engine limits as your scripts.",
        "A live move is a measured freeze reported as downtime; live mode is a paid plan feature (cold is free).",
        "rsync is still the right tool for copying files. This comparison is about moving running state.",
        "Pre-1.0: pin versions and drill the restore path.",
      ]}
      faqs={[
        { q: "Can I use rsync as the transport?", a: "No — transport is the chunk protocol over an mTLS peer channel, deduplicated by content address. rsync remains fine for out-of-band file syncing." },
        { q: "What if I only move cold data?", a: "If there is no running state to preserve, file sync plus your own orchestration may be enough. The state machine earns its keep when the process is alive and expensive." },
        { q: "How do I see what moved?", a: "The manifest: what was captured, sizes, dedup, out-of-root dependencies, and the migration record with per-stage outcomes." },
      ]}
      cta={{ heading: "Break it on purpose", body: "Point a move at a dead destination and watch the rollback hold — that is the part rsync cannot show you." }}
      related={[
        { href: "/compare/criu-scripts-vs-shift", label: "Hand-rolled CRIU scripts vs SHIFT" },
        { href: "/guides/migrate-running-process-between-linux-servers", label: "Migrate a running process between Linux servers" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
