import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Homelab Maintenance: Evacuate the Box, Patch, Bring It Back",
  description:
    "Use case: homelab maintenance. Checkpoint a running service, restore it on another box, patch the empty host, and bring the workload back — without losing the job that was running on it.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Use case"
      title="Homelab Maintenance: Evacuate the Box, Patch, Bring It Back"
      lede="The problem every homelabber knows: the kernel update that eats a weekend-long encode, the hardware swap that kills a job at hour 61. The pattern: evacuate the running service before the maintenance, and bring it back after — the restart tax disappears."
      sections={[
        {
          heading: "The three steps",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate checkpoint create job --leave-running\nshiftgate connect spare --to https://spare:8443 --machine-id SPARE_ID\nshiftgate migrate job --to spare --preflight\nshiftgate migrate job --to spare" },
            { kind: "p", text: "Patch the now-empty host, doctor it again, and migrate back — or leave the service where it landed and re-point your router. If anything fails mid-move, the source keeps running and you have lost nothing but the attempt." },
          ],
        },
        {
          heading: "Why homelabs adopt it",
          blocks: [
            { kind: "bullets", items: [
              "The hardware is yours — no hypervisor or shared NAS required.",
              "Free tier covers the reality: 2 machines, 10 GB, cold migration — which is exactly a two-box homelab.",
              "clone makes a second shape possible: one warm state, several workers for a burst weekend build.",
            ] },
          ],
        },
        {
          heading: "Limitations (read before you plan)",
          blocks: [
            { kind: "bullets", items: [
              "Linux x86_64 boxes only; the agent wants root and a CRIU-capable kernel (doctor tells you honestly).",
              "A move is a short measured freeze reported as downtime — not instant, and not zero.",
              "Live mode is a paid plan feature; cold evacuation (the homelab case) is free.",
              "Automatic failover needs a fencing provider (disabled by default) — for a lab, trigger failover manually and keep the drill fun.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "Pre-1.0: read the versioning promise before you automate around it.",
        "TCP sockets need CRIU TCP repair where permitted; services that reconnect cleanly move best.",
        "A failed move never destroys the source — proven by failing moves on purpose in the e2e suite.",
      ]}
      faqs={[
        { q: "Do both boxes need identical hardware?", a: "No — compatible state is what matters (kernel/CRIU compatibility is checked at preflight). Different distro may work subject to the compatibility report." },
        { q: "What about my Docker containers?", a: "SHIFT moves a process tree, not container images. A service inside a container is a process tree you can checkpoint only within the documented limits — check the limitations doc for namespace edges." },
        { q: "Is the free tier really enough?", a: "For two machines and cold moves, yes — that is the free tier by design: 2 machines, 10 GB, cold-only, community support." },
      ]}
      cta={{ heading: "Rehearse before the next patch Tuesday", body: "Evacuate a scratch service this weekend; the drill takes minutes." }}
      related={[
        { href: "/guides/linux-maintenance-window-without-downtime-cost", label: "The maintenance window playbook" },
        { href: "/use-cases/edge-fleets", label: "Use case: edge fleets" },
        { href: "/docs/ci-burst", label: "The CI-burst proof kit" },
      ]}
    />
  );
}
