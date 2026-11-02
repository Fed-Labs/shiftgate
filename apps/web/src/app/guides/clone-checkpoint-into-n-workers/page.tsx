import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Clone One Checkpoint Into N Workers",
  description:
    "Clone one warm checkpoint into N independent running workers on one machine: identity and root isolation, sizing, rolling seeds with keep_last, and where cloning ends and migrating begins.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Clone One Checkpoint Into N Workers"
      lede="Cloning turns a single prepared process state into a fleet: N independent running workloads restored from one checkpoint, each with its own root and identity. This is the mechanic behind CI bursts, test parallelism, and replicated warm workers."
      sections={[
        {
          heading: "What clone does",
          blocks: [
            { kind: "code", body: "shiftgate clone CHECKPOINT_ID --count 10 --prefix ci\nshiftgate clone list" },
            { kind: "bullets", items: [
              "Each clone restores the checkpoint as a new workload named from --prefix (ci-1 … ci-10).",
              "Each gets its own root and virtual identity — they are independent processes, not threads of one.",
              "Storage is reflink-copied where the filesystem allows; otherwise copied outright.",
              "clone is same-machine. Moving work to another host is a migrate.",
            ] },
          ],
        },
        {
          heading: "Workflows it unlocks",
          blocks: [
            { kind: "bullets", items: [
              "CI burst: a warmed runner state becomes a worker pool with no per-worker warm-up.",
              "Test parallelism: N independent roots for suites that want isolation without N VMs.",
              "Load shapes: N warm copies of a service behind your own router, refreshed from a rolling seed.",
            ] },
            { kind: "code", body: "# rolling seed: keep the last 5 prepared states, clone from the newest\nshiftgate workload policy runner --every 30m --keep 5" },
          ],
        },
        {
          heading: "Sizing and lifecycle",
          blocks: [
            { kind: "bullets", items: [
              "Budget disk first: N roots + the checkpoint store (deduplicated content-addressed chunks help).",
              "Memory is per-worker after restore — a 100 MiB heap × 10 workers is 1 GiB resident.",
              "Clones outlive the shell that made them; clean up with clone rollback (destructive, prompts / --yes) or workload delete.",
              "When the seed is stale (dependency or toolchain change), checkpoint the prepared workload again and clone from the new id.",
            ] },
          ],
        },
        {
          heading: "One seed, two machines",
          blocks: [
            { kind: "p", text: "To fan out across hosts, move the seed: migrate the prepared workload to the destination (preflight first), then clone there. If the move fails at any stage, the state machine rolls back and the seed on the source keeps running — a failed move never destroys the source." },
            { kind: "code", body: "shiftgate migrate runner --to ci-2 --preflight\nshiftgate migrate runner --to ci-2   # on ci-2: shiftgate clone CHECKPOINT_ID --count 10 --prefix ci" },
          ],
        },
      ]}
      honestEdges={[
        "Clone is same-machine; there is no cross-machine clone.",
        "Linux x86_64, privileged agent, CRIU-capable kernel.",
        "Clones restore state — they do not share memory with each other or the seed.",
        "Removing a clone set is destructive; the CLI prompts on a terminal and requires --yes in scripts.",
      ]}
      faqs={[
        { q: "What is the largest sensible N?", a: "Whatever fits disk and memory with headroom. The demo runs 10; the stress story runs 50. Measure the restore wall (bench: 2.27–2.77 s per restore) against your N." },
        { q: "Do clones share the checkpoint's writes?", a: "No — after restore each worker is fully independent. The checkpoint itself is immutable." },
        { q: "Can clones auto-refresh from a new checkpoint?", a: "Clones are point-in-time restorations. Keep a rolling seed with a periodic policy and clone from the newest checkpoint when you want a refreshed pool." },
        { q: "Is cloning a paid feature?", a: "Same-machine cloning is free. Cross-machine movement is a migrate — cold free, live a paid plan feature on every path." },
      ]}
      cta={{ heading: "Clone your first pool", body: "The CI-burst proof kit does exactly this end-to-end, with measured times." }}
      related={[
        { href: "/guides/burst-ci-workers-from-checkpoint", label: "Burst CI capacity from a warm checkpoint" },
        { href: "/guides/fleet-checkpoint-policy", label: "Designing a fleet checkpoint policy" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
