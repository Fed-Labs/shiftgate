"use client";

import { motion } from "framer-motion";
import {
  Cpu,
  MemoryStick,
  FileCode,
  Monitor,
  Network,
  Terminal,
  AlertTriangle,
  Check,
} from "lucide-react";
import { Container, Section, Card, Badge, Mono } from "@/components/ui";
import { SiteHeader, SiteFooter } from "@/components/site-header";

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  whileInView: { opacity: 1, y: 0 },
  viewport: { once: true },
  transition: { duration: 0.5, ease: [0.16, 1, 0.3, 1] as const },
};

const migratableItems = [
  {
    icon: Cpu,
    title: "Processes",
    description:
      "Running processes with full execution state — registers, stack, heap, open threads. The process doesn't know it moved.",
    detail: "PID preservation optional",
  },
  {
    icon: MemoryStick,
    title: "Memory",
    description:
      "Full address space including anonymous pages, file-backed mappings, and shared memory segments. Incremental transfer for large working sets.",
    detail: "Deduplicated on transfer",
  },
  {
    icon: FileCode,
    title: "File descriptors",
    description:
      "Open files, pipes, sockets, and epoll instances through CRIU. TCP repair is opt-in and capability-gated; other connections use the workload's reconnect policy.",
    detail: "Policy-driven connections",
  },
  {
    icon: Monitor,
    title: "GPU state",
    description:
      "Detected and compatibility-checked before migration. Restore requires a vendor adapter that explicitly advertises checkpoint support on matching hardware.",
    detail: "Vendor adapter required",
  },
  {
    icon: Network,
    title: "Network connections",
    description:
      "Listeners and sockets follow CRIU where supported. Otherwise SHIFTGATE reports the boundary and applies drain or reconnect behavior from workload policy.",
    detail: "Capability-gated",
  },
  {
    icon: Terminal,
    title: "Terminal sessions",
    description:
      "PTY and terminal process state can be carried by CRIU when the host supports it. Emulators and scrollback remain outside the checkpoint unless an adapter provides them.",
    detail: "CRIU-dependent",
  },
];

const limitations = [
  {
    title: "GPU migration requires compatible hardware",
    description:
      "Moving GPU state is impossible without a vendor checkpoint/restore adapter. Matching hardware is also required; generic device files are not treated as GPU state.",
  },
  {
    title: "Linux only",
    description:
      "SHIFTGATE uses CRIU (Checkpoint/Restore In Userspace) which is Linux-specific. macOS and Windows support is on the roadmap but not available today.",
  },
  {
    title: "Kernel feature requirements",
    description:
      "Certain features require specific kernel versions. ptrace-based memory access needs Linux 4.11+. Userfaultfd for live migration needs 4.3+. We check these at install time.",
  },
  {
    title: "Some kernel objects are not migratable",
    description:
      "Inotify watches, fanotify groups, and certain io_uring submissions cannot be checkpointed today. These are documented per-workload.",
  },
];

const useCases = [
  {
    title: "Desktop ↔ Laptop continuity",
    description: "Start a build on one Linux host. Checkpoint it, restore it on another compatible Linux host, and continue from the captured compiler process state and scoped files.",
    workflow: ["shift checkpoint create build-42", "shift migrate build-42 --to https://host:8443"],
  },
  {
    title: "CI workloads to faster hardware",
    description:
      "Move checkpointed CI work to a faster Linux runner after compatibility checks, rather than re-running completed stages.",
    workflow: ["shift workloads", "shift migrate ci-job-891 --to https://runner:8443"],
  },
  {
    title: "ML training to GPU machines",
    description:
      "Prepare data and validate non-GPU pipeline logic first. GPU context movement requires an explicit vendor checkpoint adapter and matching hardware; otherwise restart that stage safely from a checkpoint.",
    workflow: ["shift checkpoint create training-run-7", "shift migrate training-run-7 --to https://gpu-node:8443"],
  },
];

export default function ProductPage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14">
        {/* Hero */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div
              className="max-w-3xl"
              initial={{ opacity: 0, y: 16 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.6, ease: [0.16, 1, 0.3, 1] as const }}
            >
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Product
              </p>
              <h1 className="text-4xl md:text-5xl font-bold tracking-tight mb-6">
                Move computation
                <br />
                between machines.
              </h1>
              <p className="text-lg text-text-secondary leading-relaxed max-w-2xl">
                SHIFTGATE checkpoints running computation — processes, memory,
                file descriptors, network policy — within the limits reported by
                CRIU and device adapters — and restores it on a different
                machine.
              </p>
              <div className="mt-8 flex flex-wrap gap-3">
                <Badge variant="accent">Linux</Badge>
                <Badge variant="default">CRIU-based</Badge>
                <Badge variant="default">Encrypted transport</Badge>
                <Badge variant="default">Validated restore</Badge>
              </div>
            </motion.div>
          </Container>
        </Section>

        {/* What moves */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                What moves
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Everything about a running process.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                When SHIFTGATE migrates a workload, it captures the full
                execution state. Not just the binary — the live state of a
                running system.
              </p>
            </motion.div>

            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              {migratableItems.map((item, i) => (
                <motion.div key={item.title} {...fadeUp} transition={{ delay: i * 0.05 }}>
                  <Card className="p-5 h-full">
                    <div className="flex items-start gap-3 mb-3">
                      <div className="w-8 h-8 rounded-md border border-border flex items-center justify-center shrink-0">
                        <item.icon className="w-4 h-4 text-text-secondary" />
                      </div>
                      <div>
                        <h3 className="font-semibold text-sm">{item.title}</h3>
                        <Mono className="text-[10px] text-text-muted">
                          {item.detail}
                        </Mono>
                      </div>
                    </div>
                    <p className="text-sm text-text-secondary leading-relaxed">
                      {item.description}
                    </p>
                  </Card>
                </motion.div>
              ))}
            </div>
          </Container>
        </Section>

        {/* What doesn't (yet) */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <div className="flex items-center gap-2 mb-4">
                <AlertTriangle className="w-4 h-4 text-status-warning" />
                <p className="text-xs font-mono uppercase tracking-widest text-status-warning">
                  Limitations
                </p>
              </div>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                What doesn&apos;t move yet.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                We&apos;re honest about what works and what doesn&apos;t.
                Checkpoint/restore is hard. Here are the current boundaries.
              </p>
            </motion.div>

            <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
              {limitations.map((item, i) => (
                <motion.div key={item.title} {...fadeUp} transition={{ delay: i * 0.05 }}>
                  <Card className="p-5 h-full border-status-warning/10">
                    <h3 className="font-semibold text-sm mb-2">{item.title}</h3>
                    <p className="text-sm text-text-secondary leading-relaxed">
                      {item.description}
                    </p>
                  </Card>
                </motion.div>
              ))}
            </div>
          </Container>
        </Section>

        {/* Use cases */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Use cases
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Where it fits.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                SHIFTGATE is for workloads that are expensive to restart and
                need to move between machines.
              </p>
            </motion.div>

            <div className="space-y-6">
              {useCases.map((useCase, i) => (
                <motion.div key={useCase.title} {...fadeUp} transition={{ delay: i * 0.08 }}>
                  <Card className="p-6">
                    <h3 className="text-lg font-semibold mb-2">
                      {useCase.title}
                    </h3>
                    <p className="text-sm text-text-secondary leading-relaxed mb-5 max-w-2xl">
                      {useCase.description}
                    </p>
                    <div className="bg-bg rounded-md border border-border-subtle p-4 overflow-x-auto">
                      {useCase.workflow.map((cmd, j) => (
                        <div key={j} className="flex items-center gap-2">
                          <span className="text-text-muted text-xs select-none">
                            $
                          </span>
                          <Mono className="text-sm text-text-secondary">
                            {cmd}
                          </Mono>
                        </div>
                      ))}
                    </div>
                  </Card>
                </motion.div>
              ))}
            </div>
          </Container>
        </Section>

        {/* Bottom summary */}
        <Section className="border-t border-border-subtle">
          <Container narrow>
            <motion.div {...fadeUp} className="text-center">
              <h2 className="text-2xl md:text-3xl font-bold tracking-tight mb-4">
                Computation should be fluid.
              </h2>
              <p className="text-text-secondary leading-relaxed mb-8">
                Machines are temporary. Your work is what matters. SHIFTGATE
                makes computation portable — encrypted, verified, and fast.
              </p>
              <div className="flex flex-wrap justify-center gap-6 text-sm">
                <div className="flex items-center gap-2">
                  <Check className="w-4 h-4 text-accent" />
                  <span className="text-text-secondary">Open source agent</span>
                </div>
                <div className="flex items-center gap-2">
                  <Check className="w-4 h-4 text-accent" />
                  <span className="text-text-secondary">Encrypted by default</span>
                </div>
                <div className="flex items-center gap-2">
                  <Check className="w-4 h-4 text-accent" />
                  <span className="text-text-secondary">No daemon required</span>
                </div>
              </div>
            </motion.div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
