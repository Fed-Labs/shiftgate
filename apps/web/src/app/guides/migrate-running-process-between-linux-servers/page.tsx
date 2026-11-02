import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "How to Migrate a Running Process Between Linux Servers",
  description:
    "Move a running process between Linux servers without rebuilding it: the compatibility check, the transfer, the commit gate, measured downtime, and the failure case where the source keeps running.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="How to Migrate a Running Process Between Linux Servers"
      lede="Migrating a running process means capturing its full state on the source, reconstructing it on the destination, and only then retiring the source. This is the end-to-end recipe, including the failure path that most home-grown scripts get wrong."
      sections={[
        {
          heading: "Before you move anything",
          blocks: [
            { kind: "bullets", items: [
              "Both machines: Linux x86_64, the agent installed, doctor green on each.",
              "The workload has a declared root (the filesystem scope that travels) and, ideally, a health check the destination can run.",
              "Network path between agents: direct mTLS peer channel (the supported remote path today). The authenticated relay ships ticket primitives but is not enabled by default.",
              "Decide cold vs live: cold freezes for the whole transfer (free tier); live pre-copies while running and freezes only for the final delta (a paid plan feature, including peer-to-peer).",
            ] },
          ],
        },
        {
          heading: "The move, step by step",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate connect ci-2 --to https://second-host:8443 --machine-id MACHINE_ID\nshiftgate migrate runner --to ci-2 --preflight   # compatibility report; nothing moves\nshiftgate migrate runner --to ci-2 --mode live --pre-copy-passes 4\nshiftgate status MIGRATION_ID" },
            { kind: "p", text: "The migration is a persisted state machine — CREATED → DISCOVER → VALIDATE → SNAPSHOT → PREPARE → TRANSFER → VERIFY → RESTORE → POST_VALIDATE → SWITCH → COMMIT. Every transition is persisted before the next stage begins. The downtime value in the status output is the workload's freeze instant to the destination's health-validated restore — measured, not estimated." },
            { kind: "table", head: ["Stage", "What it proves"], rows: [
              ["VALIDATE", "The destination is compatible before state moves (kernel, CRIU, device state)."],
              ["TRANSFER", "Content-addressed encrypted chunks; only what the destination lacks crosses the wire."],
              ["POST_VALIDATE", "The destination restored and passed its health check."],
              ["COMMIT", "Only now is the source retired. Anything earlier failure → rollback."],
            ] },
          ],
        },
        {
          heading: "The failure path (the reason to do it this way)",
          blocks: [
            { kind: "code", body: "shiftgate migrate runner --to https://127.0.0.1:1\n# Migration refused at VALIDATE: destination unreachable. Rolled back; source preserved." },
            { kind: "p", text: "Kill the destination mid-transfer, fail its health check, or point at a dead peer: the state machine refuses to commit and the source keeps running. That is the tested invariant — a failed move never destroys the source — and it is what separates a state machine from dump-plus-scp." },
          ],
        },
        {
          heading: "What the numbers look like",
          blocks: [
            { kind: "p", text: "On the documented loopback harness (docs/benchmarks.md): an ~17 MB state moved live with 4 pre-copy passes reported 3.20–3.29 s downtime (cold: 3.75–3.91 s); an ~118 MB state reported 3.39–3.52 s live (cold: 4.61–5.50 s). The transfer inside the live freeze was 107–223 ms. Across a real network the delta grows with state size — re-measure before promising anyone a number." },
          ],
        },
      ]}
      honestEdges={[
        "Linux x86_64; privileged agents on both ends; CRIU-capable kernels.",
        "Downtime is a measured freeze — never a zero-downtime claim.",
        "Live mode is a paid plan feature on every path, including peer-to-peer; cold moves and dry-run/preflight are free.",
        "TCP sockets need CRIU TCP repair where permitted; unsupported state is rejected or surfaced at preflight.",
        "Direct mTLS peer connections are the supported remote path; the relay is not enabled by default.",
      ]}
      faqs={[
        { q: "Do both machines need the same distro and kernel?", a: "Not necessarily identical, but compatible: the preflight compatibility check decides, and mismatches are reported before anything moves. Restoring across kernel skew works only where CRIU permits." },
        { q: "Can I test the move without moving anything?", a: "Yes — migrate --preflight (alias --dry-run) runs the same compatibility check the real move would run and exits 0 if it would be admitted, 3 if the destination is incompatible." },
        { q: "What happens to the source after a successful move?", a: "It is retired only after the destination restores and passes health validation — the COMMIT stage. Before that, it keeps running." },
        { q: "Does this work between my own machines without the platform?", a: "Yes: peer-to-peer moves work agent-to-agent. The platform adds fleet features on top — history and lineage, policies, failover, audit." },
      ]}
      cta={{ heading: "Move one yourself", body: "Two machines, one command each to install, then the proof kit walks the move and the rollback." }}
      related={[
        { href: "/guides/live-migration-without-shared-storage", label: "Live migration without shared storage" },
        { href: "/guides/linux-maintenance-window-without-downtime-cost", label: "The maintenance window playbook" },
        { href: "/compare/rsync-and-scatter-vs-shift", label: "rsync + homemade scripts vs a migration state machine" },
      ]}
    />
  );
}
