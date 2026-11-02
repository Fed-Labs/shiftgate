import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "CRIU Checkpoint/Restore: A 15-Minute Working Example",
  description:
    "A working CRIU checkpoint/restore tutorial: what the kernel needs, a runnable example with measured times, TCP repair and process-tree limits, and where a wrapper with a commit gate fits.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="Guide"
      title="CRIU Checkpoint/Restore: A 15-Minute Working Example"
      lede="CRIU (Checkpoint/Restore In Userspace) is the kernel-facing engine that freezes a running process tree and reconstructs it elsewhere. This tutorial gets you to a working checkpoint/restore in about fifteen minutes, then shows the parts raw CRIU leaves to you."
      sections={[
        {
          heading: "What the machine needs first",
          blocks: [
            { kind: "bullets", items: [
              "Linux x86_64 with checkpoint/restore support (kernel config + namespaces + cgroups v2 helps).",
              "CRIU 3.17+ or 4.x on PATH, and criu check must pass — run it before anything else.",
              "Root (or the required capabilities): dump/restore touches process internals, so there is no unprivileged shortcut.",
            ] },
            { kind: "code", body: "criu check\n# or, end-to-end with the wrapper:\nshiftgate doctor" },
            { kind: "p", text: "doctor reports real gaps (kernel features, privileges, archive tooling) instead of substituting a fake checkpoint, and exits non-zero when a required check fails. Certified distro/CRIU lines live in docs/support-matrix.json." },
          ],
        },
        {
          heading: "The 15-minute path",
          blocks: [
            { kind: "p", text: "Using the SHIFT wrapper around CRIU keeps the example to the pieces that matter — capture, inspect, restore — while every step below maps to a raw criu dump/restore underneath." },
            { kind: "code", body: "curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash\nshiftgate doctor\n\nmkdir -p ~/criu-demo\nshiftgate workload create demo --path ~/criu-demo --start -- python3 -c 'import time; a=bytearray(50*1024*1024); i=0\nwhile True:\n i+=1; a[i%len(a)]=i%251; print(\"tick\", i, flush=True); time.sleep(1)'\n\nshiftgate checkpoint create demo --leave-running   # measured: 914–973 ms on the bench box\nshiftgate checkpoint list --workload demo\nshiftgate restore CHECKPOINT_ID                    # restore wall 2.27–2.77 s, then it resumes" },
            { kind: "p", text: "The checkpoint is encrypted, content-addressed, and immutable — corrupt or unauthenticated chunks fail closed rather than restoring garbage. --leave-running (the default) resumes the workload after the dump; --stop-when-done leaves it stopped." },
          ],
        },
        {
          heading: "What raw CRIU leaves to you",
          blocks: [
            { kind: "bullets", items: [
              "Ordering: when to freeze, when to copy, when to retire the source. Get this wrong and a failed copy kills the running workload.",
              "Integrity: a half-written image set is undetectable without content addressing and authentication.",
              "State machine: a crash mid-migrate needs persisted stage transitions or you debug two broken copies.",
              "File scope: which files travel with the process. Out-of-root open files must be resolved and reported, not assumed.",
              "TCP state: sockets need CRIU TCP repair where the kernel permits it; otherwise plan for reconnect.",
            ] },
            { kind: "p", text: "That gap is exactly what the staged wrapper exists for: every stage persists before the next begins, and the source is only retired after destination health validation — a failed move never destroys the source." },
          ],
        },
        {
          heading: "Measured numbers (and how to re-measure)",
          blocks: [
            { kind: "table", head: ["Operation", "Bench box", "Notes"], rows: [
              ["Full checkpoint", "914 – 973 ms", "~17 MB state, ~100 MiB heap methodology"],
              ["Restore wall (eager)", "2.10 – 2.77 s", "includes resume + health validation"],
              ["Time to first execution (lazy)", "142 – 179 ms", "restore --lazy starts before memory is materialized"],
            ] },
            { kind: "p", text: "Single-machine loopback numbers from the documented harness in docs/benchmarks.md — not guarantees. Re-run the harness before planning around them." },
          ],
        },
      ]}
      honestEdges={[
        "Linux x86_64, privileged agent, CRIU-capable kernel; criu check must pass.",
        "Kernel skew: restoring across kernels/distributions works only where CRIU permits — compatibility is checked and reported, never assumed.",
        "TCP repair where the kernel permits; GPU contexts only on matching checkpoint-capable hardware.",
        "A live move is a measured freeze reported as downtime; live mode is a paid plan feature (cold moves and dry runs are free).",
      ]}
      faqs={[
        { q: "Can CRIU checkpoint any process?", a: "Most user-space process trees can be dumped; the hard cases are kernel-facing state (some devices, certain socket states) and files outside the declared root — those are rejected or surfaced as limitations, not faked." },
        { q: "Do I have to stop the workload to checkpoint it?", a: "No. The dump freezes the process for the capture (bench: 914–973 ms) and resumes it automatically; --leave-running is the default." },
        { q: "What is lazy restore?", a: "restore --lazy starts the process before all its memory is materialized; a lazy-pages daemon streams pages in on demand. Where the memory is already fast local disk it can even be slightly slower to first execution — the win appears when bulk transfer would otherwise block startup." },
        { q: "Where is the honest limitations list?", a: "docs/limitations.md in the repo — TCP repair, nested PID namespace, SIGTERM behavior, io_uring/seccomp edges, failover fencing. It is part of the product." },
      ]}
      cta={{ heading: "Run the working example", body: "Install once, then the proof kit prints measured times for checkpoint, clone, migrate, and the rollback beat." }}
      related={[
        { href: "/guides/criu-limitations-explained", label: "CRIU limitations explained" },
        { href: "/guides/criu-lazy-restore", label: "Lazy restore explained" },
        { href: "/docs/benchmarks", label: "The benchmark harness" },
      ]}
    />
  );
}
