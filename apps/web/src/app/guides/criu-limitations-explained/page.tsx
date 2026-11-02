import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "CRIU Limitations Explained (TCP Repair, GPU State, Kernel Skew)",
  description:
    "The honest CRIU limitations list: TCP repair requirements, GPU and device state, kernel skew and distro restore, process trees, io_uring and seccomp edges, and what a wrapper can and cannot hide.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="CRIU Limitations Explained (TCP Repair, GPU State, Kernel Skew)"
      lede="Checkpoint/restore is powerful and bounded. Knowing the boundaries is what separates a tool you can run in production from a demo that surprises you at 3 a.m. Here is the limits list, why each exists, and how a mobility layer should behave around it: reject or surface, never fake."
      sections={[
        {
          heading: "TCP state and TCP repair",
          blocks: [
            { kind: "p", text: "Migrating a live TCP connection requires CRIU's TCP repair mode — a kernel facility that lets the checkpoint capture socket state and the restore re-establish it. Where the kernel permits repair, socket state ports; where it does not, the socket is not transparently migratable. Unsupported network state is marked portable, adaptable, or external — and rejected or surfaced as a limitation at preflight." },
            { kind: "p", text: "The honest engineering choice: request TCP state explicitly (checkpoint --tcp-state), and design services to tolerate reconnect anyway. A reconnecting client is a smaller outage than a fake portability claim." },
          ],
        },
        {
          heading: "GPU and device state",
          blocks: [
            { kind: "p", text: "Device contexts are only as portable as the hardware and drivers allow. GPU contexts checkpoint to matching checkpoint-capable hardware; mismatched devices are a preflight rejection, not a silent degradation. Model and process state rides with the memory image; device context may not — plan for both outcomes." },
          ],
        },
        {
          heading: "Kernel skew and distro restore",
          blocks: [
            { kind: "p", text: "A checkpoint is kernel-adjacent state. Restoring on a different kernel or distribution works where CRIU's compatibility rules permit and fails where they do not — resource names, cgroup layouts, and kernel features drift. Compatibility is checked before the move (preflight) and reported; certified pairs are listed in docs/support-matrix.json." },
          ],
        },
        {
          heading: "Process trees, namespaces, and roots",
          blocks: [
            { kind: "bullets", items: [
              "Multi-process trees checkpoint as a unit — the whole workload, not one PID.",
              "Nested PID namespace and some namespace setups have known edges; check the limitations doc for the current matrix.",
              "Filesystem capture is scoped to the declared root. Files outside the root that the process holds open are resolved, counted, and reported — they do not silently travel.",
              "SIGTERM and signal behavior across dump/restore follows CRIU semantics — verify your shutdown paths in a drill.",
            ] },
          ],
        },
        {
          heading: "io_uring, seccomp, and other kernel-facing state",
          blocks: [
            { kind: "p", text: "Modern kernel surfaces keep moving: io_uring, seccomp filters, LSM interactions, and eBPF attachments are the places where checkpoint support is a per-feature question, not a guarantee. The support matrix and the e2e suite are the truth; anything untested is described as untested." },
          ],
        },
        {
          heading: "What a wrapper can and cannot hide",
          blocks: [
            { kind: "p", text: "A mobility layer cannot make an unportable socket portable. What it can do is make every boundary visible before the move (compatibility preflight), keep the source alive through failures (the commit gate), measure instead of claim (downtime reported per move), and fail closed on corrupt state (authenticated content-addressed chunks). That is the difference between the honest product and the demo." },
          ],
        },
      ]}
      honestEdges={[
        "Linux x86_64 only; privileged agent; CRIU-capable kernel (CRIU 3.17+ / 4.x, criu check must pass).",
        "A live move is a measured freeze, always reported as downtime.",
        "TCP migration requires TCP repair; GPU state requires matching checkpoint-capable hardware.",
        "Live migration is a paid plan feature on every path, including peer-to-peer.",
        "Automatic failover needs a fencing provider (disabled by default — fail-closed); the manual trigger can run two live copies if the source is partitioned.",
        "Pre-1.0: the limits list grows with the test matrix — the support-matrix.json is versioned evidence, not marketing.",
      ]}
      faqs={[
        { q: "Why not just say 'transparent migration'?", a: "Because the kernel disagrees. State that cannot port is rejected or surfaced at preflight — the claim follows the code, not the other way around." },
        { q: "Where is the machine-readable matrix?", a: "docs/support-matrix.json — kernels, CRIU versions, distros, and what is actually certified vs merely attempted." },
        { q: "What if my workload hits a hard limit?", a: "Preflight reports it before anything moves, and a failed move rolls back with the source running. The worst case is a no-op, not a loss." },
      ]}
      cta={{ heading: "Check your own workload", body: "Preflight is free and moves nothing: it is the cheapest way to learn where your boundaries are." }}
      related={[
        { href: "/guides/checkpoint-security-and-encryption", label: "Checkpoint security and encryption" },
        { href: "/guides/criu-tcp-repair", label: "TCP repair: honest socket migration" },
        { href: "/docs/troubleshooting", label: "Limitations and troubleshooting" },
      ]}
    />
  );
}
