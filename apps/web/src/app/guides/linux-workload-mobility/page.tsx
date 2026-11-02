import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Linux Workload Mobility: Moving Running State Between Machines (2027 Guide)",
  description:
    "The pillar guide to moving running state between Linux machines: checkpoint/restore vs VM migration, cold vs live moves, the commit gate that keeps the source alive, and the workflows that pay for it — CI burst, evacuation, failover.",
});

const honestEdges = [
  "Linux x86_64 only; a privileged agent and a CRIU-capable kernel are required (criu check must pass).",
  "A live move is a freeze measured and reported as downtime — never a zero-downtime event (benchmark workloads: 3.2–3.5 s live vs 3.7–5.5 s cold on loopback).",
  "Live migration is a paid plan feature on every path, including peer-to-peer; the free tier is cold-only (2 machines, 10 GB) and always includes checkpoints, restores, same-machine clones, and dry-run/preflight.",
  "TCP socket state needs CRIU TCP repair where the kernel permits it; unsupported state is rejected, not faked.",
  "clone (one checkpoint → N workers) is same-machine; cross-machine is a migrate.",
  "Automatic failover needs a fencing provider (disabled by default — fail-closed); a manual trigger on a partitioned source can leave two live copies.",
  "Pre-1.0 (v0.2.x): a MINOR bump may break automation; releases keep an N-1 interop and rollback window.",
];

export default function Page() {
  return (
    <GuidePage
      eyebrow="Pillar guide"
      title="Linux Workload Mobility: Moving Running State Between Machines"
      lede="Workload mobility is the practice of moving a running Linux process — memory, file descriptors, sockets, and the filesystem state it depends on — from one machine to another (or into N copies) without rebuilding it. This guide covers how it works, when it beats the alternatives, and the four workflows that pay for it."
      sections={[
        {
          heading: "What actually moves",
          blocks: [
            { kind: "p", text: "Not a VM image and not a container image: the running thing itself. A checkpoint/restore engine (CRIU on Linux) freezes a coherent process image at a point in time — process trees, threads, signals, timers, memory pages, open files, and environment — and a mobility layer packages that state, moves it, reconstructs it on a compatible destination, and resumes from exactly where it stopped." },
            { kind: "bullets", items: [
              "Process state: pid trees, threads, signals, timers, credentials.",
              "Memory: heaps, stacks, mapped regions — byte for byte.",
              "Filesystem: the workload's declared root and its open files; files outside the root are resolved, counted, and reported, never silently captured.",
              "Network: socket state where CRIU TCP repair permits; otherwise reconnect.",
              "Device state: GPU contexts only on matching checkpoint-capable hardware — with caveats.",
            ] },
          ],
        },
        {
          heading: "The design center: the commit gate",
          blocks: [
            { kind: "p", text: "Naive mobility is a leap of faith: dump here, copy, restore there, hope. The failure mode everyone hits at least once is a move that dies midway and leaves two broken copies — or no running copy at all." },
            { kind: "p", text: "A staged state machine fixes the story. Each stage (discover, validate, snapshot, prepare, transfer, verify, restore, post-validate, switch, commit) persists its outcome before the next stage begins, and the source is only retired after the destination restores and passes its health check. Any failure rolls back with the source preserved. The invariant: state moves, and a failed move never destroys the source." },
            { kind: "code", body: "shiftgate connect ci-2 --to https://second-host:8443 --machine-id MACHINE_ID\nshiftgate migrate runner --to ci-2 --preflight   # dry run: admitted or not, nothing moves\nshiftgate migrate runner --to ci-2               # cold move (free tier path)\nshiftgate migrate runner --to ci-2 --mode live --pre-copy-passes 4   # paid: freeze only for the final delta\nshiftgate status MIGRATION_ID                    # downtime is reported, not estimated" },
          ],
        },
        {
          heading: "Cold vs live: what your downtime number means",
          blocks: [
            { kind: "p", text: "Downtime here is precisely defined: the workload's freeze instant → the destination's restore call returned with health validated. A cold move freezes for the whole transfer. A live move runs pre-dump passes while the workload keeps running and freezes only for the final delta — so less state crosses the wire inside the freeze." },
            { kind: "table", head: ["Measure", "Cold", "Live (4 pre-copy passes)"], rows: [
              ["Downtime, ~17 MB state (loopback bench)", "3.75 – 3.91 s", "3.20 – 3.29 s"],
              ["Downtime, ~118 MB state (loopback bench)", "4.61 – 5.50 s", "3.39 – 3.52 s"],
              ["Transfer inside the freeze", "all of it", "107 – 223 ms"],
            ] },
            { kind: "p", text: "These are single-box numbers from the documented harness (docs/benchmarks.md) — rerun it on your hardware. The honest takeaway is the shape: live mode buys down the frozen window as state grows, while the fixed costs (final dump + ~2.3–2.8 s restore wall) dominate small states." },
          ],
        },
        {
          heading: "The four workflows that pay for it",
          blocks: [
            { kind: "bullets", items: [
              "CI burst: one warmed runner checkpoint cloned into N workers — no cold starts on hardware you own (see the CI-burst guide).",
              "Maintenance-window evacuation: move the running workload off the host, patch, bring it back — instead of fitting the outage into the window.",
              "Warm standby / failover: checkpoints replicate to a standby; the newest copy restores when the source is provably dead or on command.",
              "Scale rebalancing: move a warm workload to the machine with the headroom instead of requeueing it from zero.",
            ] },
          ],
        },
        {
          heading: "Where it does not fit",
          blocks: [
            { kind: "p", text: "Workload mobility is not VM migration: no hypervisor, no whole-OS image, no shared-storage requirement, and no promise that a different kernel or distro will restore cleanly — compatibility is checked before state moves and surfaced honestly. Windows/macOS are out of scope. Transparent TCP state is bounded by CRIU TCP repair. GPU contexts ride along only on matching checkpoint-capable hardware. If your problem is moving whole machines, this is the wrong tool; if your problem is moving expensive state inside machines, it is the right one." },
          ],
        },
      ]}
      honestEdges={honestEdges}
      faqs={[
        { q: "Is workload mobility the same as live migration?", a: "Live migration is one mode of workload mobility: the workload keeps running while most state is pre-copied, then freezes only for the final delta. Cold migration (freeze → move → restore) is mobility too, and is free on SHIFT's free tier." },
        { q: "Do I need shared storage between the machines?", a: "No. State travels over an authenticated peer channel as content-addressed encrypted chunks — only the chunks the destination is missing move. There is no NAS requirement and no hypervisor." },
        { q: "What happens if the move fails halfway?", a: "The state machine refuses to commit and the move rolls back with the source running. A failed move never destroys the source — that invariant is tested by failing moves on purpose in the e2e suite." },
        { q: "How big can the state be?", a: "The benchmarks move 17 MB and 118 MB states; the stress suite exercises huge-memory arenas, 1500-process trees, and hostile links. Your practical bound is transfer time and disk on both ends — run the proof kit on your own numbers before planning around it." },
      ]}
      cta={{
        heading: "Prove it on your own hardware",
        body: "The reproducible path takes minutes: one warm checkpoint, N workers, one move, one proven rollback. Every step prints its measured duration.",
      }}
      related={[
        { href: "/guides/criu-checkpoint-restore-tutorial", label: "CRIU checkpoint/restore: a 15-minute working example" },
        { href: "/guides/migrate-running-process-between-linux-servers", label: "Migrate a running process between Linux servers" },
        { href: "/compare/vm-live-migration-vs-shift", label: "VM live migration vs process-level migration" },
      ]}
    />
  );
}
