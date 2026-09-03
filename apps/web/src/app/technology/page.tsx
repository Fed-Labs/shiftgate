"use client";

import { motion } from "framer-motion";
import { Lock, Key, Shield, FileCheck, Zap, Database } from "lucide-react";
import { Container, Section, Card, Mono } from "@/components/ui";
import { SiteHeader, SiteFooter } from "@/components/site-header";

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  whileInView: { opacity: 1, y: 0 },
  viewport: { once: true },
  transition: { duration: 0.5, ease: [0.16, 1, 0.3, 1] as const },
};

const stateClasses = [
  {
    name: "Portable",
    description:
      "State that transfers cleanly between any two machines. Memory pages, registers, file content, process metadata.",
    examples: ["Anonymous memory", "File-backed pages", "Stack/heap", "Thread state"],
    dotClass: "bg-accent",
  },
  {
    name: "Machine-specific",
    description:
      "State tied to hardware or kernel features. Requires compatible target or translation layer.",
    examples: ["GPU memory", "Device handles", "NUMA placement", "CPU-specific registers"],
    dotClass: "bg-status-warning",
  },
  {
    name: "External",
    description:
      "State that cannot be captured. Must be re-established or handled by the application.",
    examples: ["Network connections", "Inotify watches", "Kernel modules", "Hardware interrupts"],
    dotClass: "bg-status-error",
  },
];

const migrationStages = [
  { stage: "CREATED", description: "Migration request initiated" },
  { stage: "DISCOVER", description: "Enumerate process state and dependencies" },
  { stage: "VALIDATE", description: "Verify target compatibility and resources" },
  { stage: "SNAPSHOT", description: "CRIU checkpoint of process tree" },
  { stage: "PREPARE", description: "Package checkpoint, compute manifest" },
  { stage: "TRANSFER", description: "Encrypted transfer to target agent" },
  { stage: "VERIFY", description: "Validate manifest signatures on target" },
  { stage: "RESTORE", description: "CRIU restore on target machine" },
  { stage: "POST_VALIDATE", description: "Verify restored process state" },
  { stage: "SWITCH", description: "Redirect clients, update routing" },
  { stage: "COMMIT", description: "Finalize migration, acknowledge completion" },
  { stage: "CLEANUP", description: "Remove checkpoint data from source" },
  { stage: "COMPLETED", description: "Migration finished successfully" },
];

export default function TechnologyPage() {
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
                Technology
              </p>
              <h1 className="text-4xl md:text-5xl font-bold tracking-tight mb-6">
                How SHIFTGATE works.
              </h1>
              <p className="text-lg text-text-secondary leading-relaxed max-w-2xl">
                Built on CRIU (Checkpoint/Restore In Userspace). Encrypted
                transport. Verified state. No magic — just careful systems work.
              </p>
            </motion.div>
          </Container>
        </Section>

        {/* Pipeline diagram */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Checkpoint/Restore pipeline
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-12">
                From process to process.
              </h2>
            </motion.div>

            <motion.div {...fadeUp}>
              <Card className="p-8 overflow-x-auto">
                <div className="min-w-[800px]">
                  {/* Pipeline visualization */}
                  <div className="flex items-center justify-between gap-2 mb-8">
                    {[
                      "Process State",
                      "CRIU Checkpoint",
                      "Chunk Store",
                      "Encrypted Transfer",
                      "Restore",
                    ].map((label, i, arr) => (
                      <div key={label} className="flex items-center gap-2 flex-1">
                        <div className="flex-1">
                          <div className="border border-accent/30 bg-accent/5 rounded-md p-4 text-center">
                            <Mono className="text-xs text-text-secondary">
                              {label}
                            </Mono>
                          </div>
                        </div>
                        {i < arr.length - 1 && (
                          <div className="text-text-muted text-lg">→</div>
                        )}
                      </div>
                    ))}
                  </div>

                  {/* Technical details */}
                  <div className="grid grid-cols-5 gap-4 text-xs">
                    <div className="space-y-1">
                      <div className="text-text-muted font-mono">Memory pages</div>
                      <div className="text-text-muted font-mono">Registers</div>
                      <div className="text-text-muted font-mono">File descriptors</div>
                    </div>
                    <div className="space-y-1">
                      <div className="text-text-muted font-mono">ptrace freeze</div>
                      <div className="text-text-muted font-mono">Page dump</div>
                      <div className="text-text-muted font-mono">Metadata capture</div>
                    </div>
                    <div className="space-y-1">
                      <div className="text-text-muted font-mono">Chunked storage</div>
                      <div className="text-text-muted font-mono">Deduplication</div>
                      <div className="text-text-muted font-mono">Manifest signing</div>
                    </div>
                    <div className="space-y-1">
                      <div className="text-text-muted font-mono">AES-256-GCM</div>
                      <div className="text-text-muted font-mono">Mutual TLS</div>
                      <div className="text-text-muted font-mono">Per-machine keys</div>
                    </div>
                    <div className="space-y-1">
                      <div className="text-text-muted font-mono">CRIU restore</div>
                      <div className="text-text-muted font-mono">State validation</div>
                      <div className="text-text-muted font-mono">Client redirect</div>
                    </div>
                  </div>
                </div>
              </Card>
            </motion.div>
          </Container>
        </Section>

        {/* State classes */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                State classification
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Three kinds of state.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                Not all state is equal. SHIFTGATE classifies process state into
                three categories to determine migration strategy.
              </p>
            </motion.div>

            <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
              {stateClasses.map((cls, i) => (
                <motion.div key={cls.name} {...fadeUp} transition={{ delay: i * 0.08 }}>
                  <Card className="p-6 h-full">
                    <div className="flex items-center gap-2 mb-4">
                      <div className={`w-2 h-2 rounded-full ${cls.dotClass}`} />
                      <h3 className="font-semibold">{cls.name}</h3>
                    </div>
                    <p className="text-sm text-text-secondary leading-relaxed mb-4">
                      {cls.description}
                    </p>
                    <div className="space-y-1">
                      {cls.examples.map((ex) => (
                        <div
                          key={ex}
                          className="text-xs font-mono text-text-muted"
                        >
                          {ex}
                        </div>
                      ))}
                    </div>
                  </Card>
                </motion.div>
              ))}
            </div>
          </Container>
        </Section>

        {/* Migration stages */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Migration stages
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Thirteen stages.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                Every migration passes through a deterministic state machine.
                Each stage is logged and can be audited.
              </p>
            </motion.div>

            <motion.div {...fadeUp}>
              <Card className="p-6">
                <div className="space-y-3">
                  {migrationStages.map((item, i) => (
                    <div key={item.stage} className="flex items-start gap-4">
                      <div className="w-12 shrink-0">
                        <Mono className="text-xs text-accent">{item.stage}</Mono>
                      </div>
                      <div className="flex-1 text-sm text-text-secondary">
                        {item.description}
                      </div>
                      {i < migrationStages.length - 1 && (
                        <div className="text-text-muted text-xs">↓</div>
                      )}
                    </div>
                  ))}
                </div>
              </Card>
            </motion.div>
          </Container>
        </Section>

        {/* Incremental checkpoints */}
        <Section className="border-t border-border-subtle">
          <Container>
            <div className="grid grid-cols-1 md:grid-cols-2 gap-8">
              <motion.div {...fadeUp}>
                <div className="flex items-center gap-2 mb-4">
                  <Database className="w-4 h-4 text-accent" />
                  <p className="text-xs font-mono uppercase tracking-widest text-accent">
                    Available now
                  </p>
                </div>
                <h2 className="text-2xl md:text-3xl font-bold tracking-tight mb-4">
                  Incremental checkpoints.
                </h2>
                <p className="text-text-secondary leading-relaxed mb-4">
                  Incremental checkpoints use retained CRIU parent images and encrypted
                  content-addressed chunks. Repeated chunk ciphertext is not stored or
                  sent again; savings depend entirely on workload state.
                </p>
                <div className="bg-bg rounded-md border border-border-subtle p-4 space-y-2">
                  <div className="flex justify-between text-xs">
                    <Mono className="text-text-muted">Capture unit</Mono>
                    <Mono className="text-text-secondary">CRIU image chain</Mono>
                  </div>
                  <div className="flex justify-between text-xs">
                    <Mono className="text-text-muted">Storage unit</Mono>
                    <Mono className="text-text-secondary">Encrypted chunks</Mono>
                  </div>
                  <div className="flex justify-between text-xs">
                    <Mono className="text-text-muted">Restore input</Mono>
                    <Mono className="text-text-secondary">Validated lineage</Mono>
                  </div>
                </div>
              </motion.div>

              <motion.div {...fadeUp} transition={{ delay: 0.1 }}>
                <div className="flex items-center gap-2 mb-4">
                  <Zap className="w-4 h-4 text-status-warning" />
                  <p className="text-xs font-mono uppercase tracking-widest text-status-warning">
                    Implemented boundary
                  </p>
                </div>
                <h2 className="text-2xl md:text-3xl font-bold tracking-tight mb-4">
                  Live mode.
                </h2>
                <p className="text-text-secondary leading-relaxed mb-4">
                  Runs CRIU pre-dump while the process is running, then creates a final
                  authoritative checkpoint with the process stopped. Downtime depends on
                  dirty state, disk, network, and restore time.
                </p>
                <div className="bg-bg rounded-md border border-border-subtle p-4">
                  <p className="text-xs text-text-muted leading-relaxed">
                    Live mode has no transparent dirty-page convergence or CoW filesystem
                    adapter. Run `shift doctor` and validate your workload before production.
                  </p>
                </div>
              </motion.div>
            </div>
          </Container>
        </Section>

        {/* Security */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Security
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-12">
                Encrypted and verified.
              </h2>
            </motion.div>

            <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
              {[
                {
                  icon: Lock,
                  title: "Encrypted transport",
                  description:
                    "All checkpoint data encrypted with AES-256-GCM. Keys derived per-migration.",
                },
                {
                  icon: Key,
                  title: "Per-machine keys",
                  description:
                    "Each machine has its own key pair. Checkpoints encrypted for the target only.",
                },
                {
                  icon: Shield,
                  title: "Mutual TLS",
                  description:
                    "Agent-to-agent communication uses mutual TLS. Both sides authenticate.",
                },
                {
                  icon: FileCheck,
                  title: "Manifest signatures",
                  description:
                    "Every checkpoint includes a signed manifest. SHA-256 verification on restore.",
                },
                {
                  icon: Shield,
                  title: "No plaintext at rest",
                  description:
                    "Checkpoint data is never written to disk unencrypted. Keys stored in kernel keyring.",
                },
                {
                  icon: FileCheck,
                  title: "Audit trail",
                  description:
                    "Every migration logged with timestamps, checksums, and authentication events.",
                },
              ].map((item, i) => (
                <motion.div key={item.title} {...fadeUp} transition={{ delay: i * 0.05 }}>
                  <Card className="p-5 h-full">
                    <item.icon className="w-4 h-4 text-accent mb-3" />
                    <h3 className="font-semibold text-sm mb-2">{item.title}</h3>
                    <p className="text-xs text-text-secondary leading-relaxed">
                      {item.description}
                    </p>
                  </Card>
                </motion.div>
              ))}
            </div>
          </Container>
        </Section>

        {/* Bottom */}
        <Section className="border-t border-border-subtle">
          <Container narrow>
            <motion.div {...fadeUp} className="text-center">
              <h2 className="text-2xl md:text-3xl font-bold tracking-tight mb-4">
                Built for engineers.
              </h2>
              <p className="text-text-secondary leading-relaxed">
                SHIFTGATE is open source. Read the code, audit the crypto,
                understand the tradeoffs. No black boxes.
              </p>
            </motion.div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
