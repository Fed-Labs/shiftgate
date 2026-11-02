import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Live Migration Without Shared Storage: A Practical Pattern",
  description:
    "Live-migrate a running Linux process between machines with no NAS or SAN: pre-copy over an authenticated channel, a measured freeze for the final delta, and a commit gate that keeps the source alive on failure.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Live Migration Without Shared Storage: A Practical Pattern"
      lede="Classic live migration assumes a hypervisor and shared storage. Process-level live migration needs neither: pre-copy the process state over an authenticated channel while it keeps running, freeze only for the final delta, and commit only after the destination proves itself."
      sections={[
        {
          heading: "Why shared storage is not required",
          blocks: [
            { kind: "p", text: "Shared storage exists in VM migration so both hosts can see the same disk image. A process-level move instead captures the filesystem state the workload actually uses — its declared root and open files — into content-addressed encrypted chunks, and sends only the chunks the destination is missing. Deduplicated transfer means a destination that already holds yesterday's checkpoint needs only the delta." },
            { kind: "code", body: "shiftgate connect dst --to https://dst:8443 --machine-id DST_ID\nshiftgate migrate app --to dst --mode live --pre-copy-passes 4" },
          ],
        },
        {
          heading: "The live pattern, stage by stage",
          blocks: [
            { kind: "bullets", items: [
              "Pre-copy: CRIU pre-dump passes move the bulk of memory while the workload keeps running; each pass moves only what changed since the last.",
              "Final freeze: one last dump captures the delta. This freeze is the downtime — measured and reported as downtime in the migration record.",
              "Restore + health validation on the destination, then commit: only then is the source retired.",
              "Any failure before commit → rollback, source preserved and running.",
            ] },
            { kind: "table", head: ["State size (bench)", "Cold downtime", "Live downtime (4 passes)", "Transfer in freeze (live)"], rows: [
              ["~17 MB", "3.75 – 3.91 s", "3.20 – 3.29 s", "107 – 223 ms"],
              ["~118 MB", "4.61 – 5.50 s", "3.39 – 3.52 s", "107 – 116 ms"],
            ] },
            { kind: "p", text: "Loopback harness numbers (docs/benchmarks.md). Note the shape: live mode's advantage grows with state size because pre-copy moves the bulk outside the freeze; the fixed costs (final dump + ~2.3–2.8 s restore) stay." },
          ],
        },
        {
          heading: "Choosing pre-copy passes",
          blocks: [
            { kind: "p", text: "Each pre-dump pass buys down the frozen window at the cost of another pass over changing memory. The default is a single pass; --pre-copy-passes N (0–16) caps it. For write-heavy workloads with large heaps, more passes converge toward a small final delta; for small states the passes are overhead — the freeze is already short." },
            { kind: "code", body: "shiftgate migrate app --to dst --mode live --pre-copy-passes 6" },
          ],
        },
        {
          heading: "When to use cold instead",
          blocks: [
            { kind: "p", text: "If a three-second freeze is acceptable, cold mode is simpler and free: freeze, transfer, restore. If the workload cannot pause at all right now, know the honest ceiling — the final freeze exists in every live migration; the pattern minimizes it, it does not delete it." },
          ],
        },
      ]}
      honestEdges={[
        "A live move is a freeze measured and reported as downtime — never a zero-downtime claim.",
        "Live migration is a paid plan feature on every path, including peer-to-peer; the free tier is cold-only.",
        "Linux x86_64, privileged agent, CRIU-capable kernel on both ends.",
        "TCP sockets need CRIU TCP repair where permitted; preflight reports what cannot be ported.",
        "Direct mTLS peers are the supported remote path; the relay is not enabled by default.",
      ]}
      faqs={[
        { q: "Is this the same as VM live migration?", a: "No hypervisor is involved — the process moves, not a virtual machine. That is why no shared storage is needed; see the VM comparison page." },
        { q: "What is the frozen window exactly?", a: "From the final freeze to the destination's health-validated restore. It is reported as downtime in the migration record for every move." },
        { q: "Does the destination need identical hardware?", a: "It needs compatible state: kernel/CRIU compatibility is checked at preflight, and device state (e.g. GPU contexts) only ports to matching checkpoint-capable hardware." },
        { q: "Is live mode free?", a: "No — live migration is a paid plan feature on every path, including peer-to-peer. Cold migration, preflight/dry-run, checkpoints, restores, and same-machine clones are free." },
      ]}
      cta={{ heading: "Measure your own frozen window", body: "Run the proof kit with a real workload of yours — every stage prints its measured duration." }}
      related={[
        { href: "/guides/migrate-running-process-between-linux-servers", label: "Migrate a running process between Linux servers" },
        { href: "/guides/criu-pre-dump", label: "Pre-dump passes: buying down the frozen window" },
        { href: "/compare/vm-live-migration-vs-shift", label: "VM live migration vs process-level migration" },
      ]}
    />
  );
}
