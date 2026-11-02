import { GuidePage, contentMetadata } from "@/components/marketing/guide-page";

export const metadata = contentMetadata({
  title: "Multi-Process Trees: Checkpointing the Whole Workload, Not One PID",
  description:
    "Checkpointing multi-process workloads with CRIU: trees as a unit, shared memory and pipes, out-of-root files, process-count limits, and what the stress suite proves at 1,500 processes.",
});

export default function Page() {
  return (
    <GuidePage
      eyebrow="CRIU deep dive"
      title="Multi-Process Trees: Checkpointing the Whole Workload, Not One PID"
      lede="Real workloads are trees: a supervisor, workers, sidecars, shells. Checkpoint/restore has to capture the whole tree coherently — or the restore reconstructs a headless process with dangling pipes. This guide covers how the tree moves as a unit and where the edges are."
      sections={[
        {
          heading: "The tree is the unit",
          blocks: [
            { kind: "p", text: "CRIU dumps a process tree in one frozen instant: parent/child relationships, process groups, shared memory segments, pipes and sockets between members, pending signals. The restore reconstructs the tree with the same internal wiring — members resume as one workload, not as independent processes that must find each other again." },
            { kind: "code", body: "shiftgate workload create stack --path /srv/stack --start -- ./supervisor.sh\nshiftgate checkpoint create stack --leave-running\nshiftgate restore CHECKPOINT_ID    # the whole tree, with its pipes and signals" },
          ],
        },
        {
          heading: "The filesystem boundary is where trees get honest",
          blocks: [
            { kind: "p", text: "Only the declared root travels. When a tree opens files outside its root — a log in /var, a socket in /tmp, a shared library path — those dependencies are resolved, counted, and reported: 'N of M resolved files live outside the workload root and do not travel with the checkpoint.' Sometimes that is fine (a log that can reopen); sometimes it is the bug you would otherwise find at restore." },
            { kind: "bullets", items: [
              "Attach every directory the tree needs as workload paths.",
              "Read the dependency report after every checkpoint of a new workload class.",
              "Do not assume pipes to out-of-root processes survive — they are external state.",
            ] },
          ],
        },
        {
          heading: "Scale: what breaks at 1,500 processes",
          blocks: [
            { kind: "p", text: "The stress suite includes a 1500-process tree scenario alongside huge-memory states, hostile-link transfers, and source death mid-transfer — each skipping explicitly when the environment cannot run it, never faking a pass. Expect checkpoint duration and manifest size to grow with member count; measure your tree before you schedule around it." },
          ],
        },
        {
          heading: "Signals and shutdown semantics",
          blocks: [
            { kind: "p", text: "Pending signals are part of the captured state; SIGTERM behavior across dump/restore follows CRIU semantics. If your supervisor uses signals for lifecycle coordination, rehearse the restore in a drill and verify the members' behavior matches what your runbooks assume." },
          ],
        },
      ]}
      honestEdges={[
        "Out-of-root open files do not travel — they are resolved and reported, never silently captured.",
        "Processes outside the workload tree (foreign sidecars) are external state; pipes to them will not survive as-is.",
        "Linux x86_64, privileged agent, CRIU-capable kernel; nested PID namespace has documented edges.",
        "Checkpoint duration grows with tree size — the 1500-process stress case exists to measure it, not to promise it.",
      ]}
      faqs={[
        { q: "Does every member need its own checkpoint?", a: "No — one checkpoint of the workload captures the whole tree at one instant." },
        { q: "What about my supervisor's unix sockets inside the root?", a: "Sockets inside the captured scope restore with the tree; sockets connecting to out-of-root processes are external state and must be re-established." },
        { q: "How big can the tree be?", a: "The stress suite runs 1500 processes. Your bound is capture time and restore time for your tree's shape — run it and read the measured durations." },
      ]}
      cta={{ heading: "Checkpoint a real tree", body: "Start with your actual supervisor layout; the dependency report will tell you what you missed." }}
      related={[
        { href: "/guides/criu-limitations-explained", label: "CRIU limitations explained" },
        { href: "/guides/criu-checkpoint-restore-tutorial", label: "CRIU checkpoint/restore tutorial" },
        { href: "/docs/benchmarks", label: "The benchmark harness" },
      ]}
    />
  );
}
