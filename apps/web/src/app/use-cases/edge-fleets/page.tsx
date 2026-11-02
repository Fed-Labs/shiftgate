import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Edge Fleets: The 3 A.M. Evacuation",
  description:
    "Use case: edge fleets. Evacuate a remote Linux box at 3 a.m. without a site visit — checkpoint the running workload, move it to the neighboring box, keep the source alive on failure.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Use case"
      title="Edge Fleets: The 3 A.M. Evacuation"
      lede="The problem: a remote box at a site nobody is visiting needs a disk swap, a power fix, or is simply acting up — and the service on it is the site. The pattern: move the running workload to its neighbor before you touch the hardware."
      sections={[
        {
          heading: "The three steps",
          blocks: [
            { kind: "code", body: "shiftgate doctor\nshiftgate checkpoint create edge-app --leave-running\nshiftgate connect edge-2 --to https://edge-2:8443 --machine-id EDGE2_ID\nshiftgate migrate edge-app --to edge-2 --preflight\nshiftgate migrate edge-app --to edge-2" },
            { kind: "p", text: "The maintenance happens on an empty box. If the move fails — flaky link, wrong node — the rollback leaves the service running where it was. That invariant is what makes a remote evacuation safe to attempt unattended." },
          ],
        },
        {
          heading: "Why edge estates are a natural fit",
          blocks: [
            { kind: "bullets", items: [
              "Owned hardware, thin margins on truck rolls: every avoided visit is the ROI.",
              "Small states are fast states: edge services often move in seconds (bench: ~17 MB state, 3.2–3.9 s downtime).",
              "No shared storage to build at every site — chunks move agent-to-agent.",
            ] },
          ],
        },
        {
          heading: "Limitations (read before you plan)",
          blocks: [
            { kind: "bullets", items: [
              "Link quality bounds the move: the bench numbers are loopback; a 150 ms hostile-link path was stress-tested but yours is yours — measure.",
              "The relay path (NAT traversal) is not enabled by default; direct mTLS peers are the supported remote path today.",
              "A live move is a measured freeze reported as downtime; live mode is a paid plan feature.",
              "Automatic failover needs a fencing provider (disabled by default); at the edge prefer deliberate trigger — and know that a manual trigger on a partitioned source can leave two live copies.",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "No NAT-traversal productization: the authenticated relay ships ticket primitives but is not enabled by default.",
        "A failed move never destroys the source — the edge case that matters most is the failed one.",
        "Linux x86_64; privileged agent; CRIU-capable kernel at every site.",
      ]}
      faqs={[
        { q: "What if the link drops mid-move?", a: "The move fails and rolls back — the source keeps running. Re-attempt when the link is back." },
        { q: "Can the neighbor auto-take-over?", a: "Warm-standby failover exists (paid capability), but automatic restore needs a fencing provider (disabled by default); for edge sites, deliberate trigger plus a drill is the honest posture." },
        { q: "How do I manage many sites?", a: "The CLI's fleet surface and the API cover registration and operations; audit and policy are Business-tier fleet features." },
      ]}
      cta={{ heading: "Rehearse the 3 a.m. runbook", body: "One evacuation on a scratch service tells you what your links will actually do." }}
      related={[
        { href: "/use-cases/homelab-maintenance", label: "Use case: homelab maintenance" },
        { href: "/guides/linux-maintenance-window-without-downtime-cost", label: "The maintenance window playbook" },
        { href: "/use-cases/failover-and-dr", label: "Use case: failover and DR" },
      ]}
    />
  );
}
