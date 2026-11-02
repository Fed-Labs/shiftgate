import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Hand-Rolled CRIU Scripts vs SHIFT",
  description:
    "Hand-rolled CRIU scripts vs a staged migration state machine: where dump/restore scripts break (ordering, integrity, the failed move), what a commit gate changes, and the honest edges of each approach.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Comparison"
      title="Hand-Rolled CRIU Scripts vs SHIFT"
      lede="If you have written `criu dump` → `rsync` → `criu restore` as a shell script, you own a mobility layer now. This comparison is about what that layer must decide — and which decisions a script tends to get wrong on the day it matters."
      sections={[
        {
          heading: "The two approaches side by side",
          blocks: [
            { kind: "table", head: ["Today's approach", "What it costs", "SHIFT's frame"], rows: [
              ["Checkpoint scripts + scp/rsync (hand-rolled CRIU or app dumps)", "Fragile ordering, no integrity story, source kept alive by luck; every app reimplements freeze/copy/restore", "One state machine with persisted stages, content-addressed encrypted chunks (dedup transfer), and a commit gate: the move is only committed after destination health validation"],
            ] },
          ],
        },
        {
          heading: "Where the scripts break",
          blocks: [
            { kind: "bullets", items: [
              "Ordering: freeze, copy, restore, retire — in that order, with aborts at each arrow. Scripts usually retire the source before they know the destination works.",
              "Integrity: a truncated tarball restores garbage. Authenticated content-addressed chunks fail closed instead.",
              "Crash mid-move: without persisted stage transitions you debug two broken copies. The state machine resumes or rolls back from the last persisted stage.",
              "The failed move: the one event the script never rehearses. The invariant here is tested on purpose — a failed move never destroys the source.",
            ] },
          ],
        },
        {
          heading: "What stays the same",
          blocks: [
            { kind: "p", text: "CRIU is still the engine under both approaches — same kernel requirements, same limits (TCP repair, device state, kernel skew). The difference is the layer around it: what happens between the dump and the restore, and what happens when anything in between fails." },
            { kind: "code", body: "shiftgate migrate app --to dst --preflight   # what a script cannot ask: would this move be admitted?\nshiftgate migrate app --to dst               # commit gate on: retire source only after health validation" },
          ],
        },
        {
          heading: "When hand-rolling is fine",
          blocks: [
            { kind: "p", text: "One machine, one workload, a restore you can rehearse by hand, and no one on call — a script is honest tooling for that. The calculus flips when the move crosses machines, when the workload is expensive to re-warm, or when someone else has to run the runbook at 3 a.m." },
          ],
        },
      ]}
      honestEdges={[
        "SHIFT is CRIU under the hood — it inherits every CRIU limitation (Linux x86_64, privileged agent, TCP repair, kernel skew).",
        "A live move is a measured freeze reported as downtime; live mode is a paid plan feature (cold is free).",
        "Pre-1.0: state formats follow the versioning promise; pin versions in your automation.",
        "If your script already rehearses failures and verifies integrity, it is solving the same problem — at the cost of maintaining it forever.",
      ]}
      faqs={[
        { q: "Can I keep my own scripts and use SHIFT pieces?", a: "The CLI is the typed surface — checkpoint create/restore/migrate are individually usable. The commit gate is the part that only exists when the whole move runs as one state machine." },
        { q: "What does the state machine add over set -euo pipefail?", a: "Persisted stage transitions, compatibility preflight, health-validated commit, and a rollback path that keeps the source running. Error handling stops; recovery is what matters." },
        { q: "Is this just wrapping criu?", a: "It wraps the operational contract — ordering, integrity, failure — not the kernel facility. CRIU stays the engine; the docs say exactly where it stops." },
      ]}
      cta={{ heading: "Run the proof against your script's scenario", body: "The rollback beat in the CI-burst kit is the exact case scripts fail." }}
      related={[
        { href: "/compare/rsync-and-scatter-vs-shift", label: "rsync + homemade scripts vs a migration state machine" },
        { href: "/compare/shift-vs-manual-checkpointing", label: "Manual checkpointing vs SHIFT" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
