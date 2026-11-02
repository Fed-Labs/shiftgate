import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Workload Migration in Air-Gapped Environments",
  description:
    "Move running workloads inside air-gapped estates: local-only operation, offline install and GC, peer-only data paths, what the hosted platform does and does not touch, and the honest limits.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="Workload Migration in Air-Gapped Environments"
      lede="Air-gapped estates are where workload mobility earns its keep — and where every claim gets audited. The short version: the data plane is peer-to-peer between your machines; the agent and CLI run local-only; nothing about a checkpoint requires the internet."
      sections={[
        {
          heading: "What runs where",
          blocks: [
            { kind: "bullets", items: [
              "Agent + CLI: fully local. Default agent endpoint is a Unix socket; CLI talks to it with no network at all.",
              "Checkpoint chunks: agent-to-agent over mTLS peer channels inside your network. Encrypted, content-addressed — the wire format is safe to carry across your DMZ patterns if you choose to.",
              "Control plane (fleet metadata, dashboard, billing): the hosted service is optional for moving state. Peer-to-peer moves work agent-to-agent; the platform adds fleet features (history, policy, failover, audit).",
              "Self-hosted control plane: available under an Enterprise agreement (source-available ELv2 — internal use fine, offering it as a managed service is not).",
            ] },
          ],
        },
        {
          heading: "Offline install and maintenance",
          blocks: [
            { kind: "code", body: "# release tarball, verified by SHA-256, no network after download\nsudo ./install.sh --no-service     # binaries only, air-gap-friendly\nshiftgate doctor\nshiftgate gc --dry-run             # offline chunk GC planning\nsudo shiftgate gc                 # online sweep (agent-side), shift-gc is the offline fallback" },
            { kind: "p", text: "The installer's network use is the download itself — stage the release tarball and install.sh on your transfer medium and verify the SHA-256 from the release document. CRIU comes from your distro packages." },
          ],
        },
        {
          heading: "The estate pattern",
          blocks: [
            { kind: "bullets", items: [
              "Evacuation: move running workloads between your own hosts for maintenance, with the source preserved on any failed move.",
              "Burst: clone warm checkpoints into worker pools on the machine with headroom.",
              "Standby: replicate checkpoints to a spare host; restore on command (automatic failover needs fencing — it has none yet).",
              "Nothing in these flows requires DNS, telemetry, or a hosted service.",
            ] },
          ],
        },
        {
          heading: "What air-gapped buyers should verify",
          blocks: [
            { kind: "bullets", items: [
              "Supply chain: SHA-256-verified releases, release document digest-binding, SBOM in the release pipeline.",
              "Key custody: set SHIFT_MASTER_KEY_FILE in production — the keyfile is the at-rest key boundary.",
              "Support surface: GitHub issues need internet; for truly detached estates, contractual support is an Enterprise conversation.",
              "Entitlements: live migration is a paid plan feature — enforcement is in the agent/CLI (the free tier is cold-only everywhere).",
            ] },
          ],
        },
      ]}
      honestEdges={[
        "The hosted control plane is not required to move state; it is required for fleet features and hosted services.",
        "There is no offline license server — entitlement enforcement is local config plus plan state; a determined root user can patch a fork (we say so).",
        "Automatic failover needs a fencing provider (disabled by default — fail-closed); the manual trigger can run two live copies if the source is partitioned.",
        "Linux x86_64; CRIU from your distro; certified lines in docs/support-matrix.json.",
      ]}
      faqs={[
        { q: "Does a migration ever leave our network?", a: "Not unless you route it that way. Peer moves are agent-to-agent inside your estate; mirroring destinations are whatever object store you configure (local disk works)." },
        { q: "Can we run the whole platform internally?", a: "Yes under an Enterprise agreement — the fleet control plane is source-available (ELv2) and self-hostable as part of that conversation." },
        { q: "How do updates work detached?", a: "Stage the release tarball, verify its digest, run the installer or the update flow against your mirror. The versioning promise describes the interop/rollback window." },
      ]}
      cta={{ heading: "Prove it on an isolated pair", body: "Two air-gapped VMs, the release tarball, and the proof kit — the rollback beat needs no network." }}
      related={[
        { href: "/guides/checkpoint-security-and-encryption", label: "Checkpoint security and encryption" },
        { href: "/use-cases/edge-fleets", label: "Use case: edge fleets" },
        { href: "/docs/security", label: "The security page" },
      ]}
    />
  );
}
