import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "VM Live Migration vs Process-Level Migration",
  description:
    "VM live migration vs moving the workload itself: shared storage and hypervisor requirements, license and OS baggage, measured freeze windows, and which problems each model actually solves.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Comparison"
      title="VM Live Migration vs Process-Level Migration"
      lede="VM migration moves the machine. Process-level migration moves the workload — the process image and the filesystem state it actually uses. The right choice depends on whether your problem lives inside the machine or at the machine boundary."
      sections={[
        {
          heading: "The two models side by side",
          blocks: [
            { kind: "table", head: ["Today's approach", "What it costs", "SHIFT's frame"], rows: [
              ["VM migration (live migration / evacuation)", "Moves the whole machine — OS, cruft, licenses; needs shared storage or hypervisor cooperation; heavyweight per move", "Moves the workload: explicitly-scoped filesystem state plus the process image, agent-to-agent over TLS, no shared storage, no hypervisor"],
            ] },
          ],
        },
        {
          heading: "What VM migration does better",
          blocks: [
            { kind: "bullets", items: [
              "Whole-machine identity: kernel, drivers, and every stray file come along — maximal compatibility for arbitrary guests.",
              "Ecosystem maturity: hypervisor tooling, DR suites, vendor support contracts.",
              "Opaque workloads: no cooperation needed from what runs inside.",
            ] },
          ],
        },
        {
          heading: "What process-level migration does better",
          blocks: [
            { kind: "bullets", items: [
              "No shared storage, no hypervisor, no whole-OS image to move per event — chunks are deduplicated content-addressed state.",
              "Scope discipline: only the declared workload root and process image travel; OS cruft and licenses stay put.",
              "The move can be a clone: one warm checkpoint becomes N workers on one host — a shape VM migration does not have.",
              "Cross-distro destinations: the workload may restore on a different distro/kernel, subject to the compatibility report.",
            ] },
          ],
        },
        {
          heading: "The measured freeze, honestly",
          blocks: [
            { kind: "p", text: "Both models freeze for a final delta; neither is zero-downtime. Process-level numbers from the documented harness: 3.20–3.52 s downtime live (17 MB / 118 MB states, loopback), with 107–116 ms of transfer inside the live freeze (cold mode's transfer window is the larger 215–223 ms) and a 2.27–2.77 s restore wall. VM live migration's frozen window is famously small and famously sensitive to memory dirty rate — compare your own numbers, not vendor slides." },
          ],
        },
      ]}
      honestEdges={[
        "SHIFT is Linux x86_64, pre-1.0, privileged agent, CRIU-capable kernel — a process image, not a VM image.",
        "A live move is a measured freeze reported as downtime; live mode is a paid plan feature on every path.",
        "TCP state needs CRIU TCP repair where permitted; VM migration carries sockets implicitly — different trade.",
        "If you need whole-machine identity (kernel, drivers, arbitrary guest OS), VM migration is the right tool.",
      ]}
      faqs={[
        { q: "Is this a replacement for vMotion/Live Migration?", a: "For moving expensive workloads between machines you own, it is a lighter alternative. For evacuating whole VMs with arbitrary guest OSes, keep your hypervisor." },
        { q: "Do I need shared storage?", a: "No — state moves over an authenticated peer channel as encrypted chunks. That is the structural difference from VM live migration." },
        { q: "Which is faster?", a: "Different constants. VM migration amortizes a whole machine; process migration moves only workload state. The benchmarks publish ours; measure the VM side yourself." },
      ]}
      cta={{ heading: "Compare on your own hardware", body: "One workload, one migrate, one reported downtime number." }}
      related={[
        { href: "/guides/live-migration-without-shared-storage", label: "Live migration without shared storage" },
        { href: "/compare/restart-and-rewarm-vs-checkpoint", label: "Restart-and-rewarm vs checkpoint/restore" },
        { href: "/docs/benchmarks", label: "The benchmark harness" },
      ]}
    />
  );
}
