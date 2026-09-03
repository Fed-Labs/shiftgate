"use client";

import { motion } from "framer-motion";
import { Terminal, ArrowRight, BookOpen } from "lucide-react";
import { Container, Section, Card, Badge, Mono } from "@/components/ui";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import Link from "next/link";

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  whileInView: { opacity: 1, y: 0 },
  viewport: { once: true },
  transition: { duration: 0.5, ease: [0.16, 1, 0.3, 1] as const },
};

const cliCommands = [
  {
    cmd: "shift machines",
    description: "Show machines known to the control plane",
  },
  {
    cmd: "shift workloads",
    description: "Show workloads managed by the selected agent",
  },
  {
    cmd: "shift checkpoint create my-workload",
    description: "Capture a running workload through CRIU",
  },
  {
    cmd: "shift migrate my-workload --to https://host:8443 --machine-id ID",
    description: "Migrate to an authenticated peer agent",
  },
  {
    cmd: "shift status",
    description: "Show current migration status and agent health",
  },
];

const apiEndpoints = [
  { method: "GET", path: "/api/v1/machines", description: "List machines" },
  { method: "GET", path: "/api/v1/workloads", description: "List workloads" },
  { method: "POST", path: "/api/v1/checkpoints", description: "Create checkpoint" },
  { method: "POST", path: "/api/v1/migrations", description: "Start migration" },
  { method: "GET", path: "/api/v1/migrations/:id", description: "Get migration status" },
  { method: "DELETE", path: "/api/v1/migrations/:id", description: "Cancel migration" },
  { method: "GET", path: "/api/v1/audit", description: "Query audit logs" },
];

const methodColors: Record<string, string> = {
  GET: "text-status-info",
  POST: "text-status-healthy",
  DELETE: "text-status-error",
};

export default function DevelopersPage() {
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
                Developers
              </p>
              <h1 className="text-4xl md:text-5xl font-bold tracking-tight mb-6">
                Built for the command line.
              </h1>
              <p className="text-lg text-text-secondary leading-relaxed max-w-2xl">
                SHIFTGATE is a CLI-first tool with a REST API underneath.
                Script it, automate it, integrate it. The interface stays out
                of your way.
              </p>
            </motion.div>
          </Container>
        </Section>

        {/* CLI */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <div className="flex items-center gap-2 mb-4">
                <Terminal className="w-4 h-4 text-accent" />
                <p className="text-xs font-mono uppercase tracking-widest text-accent">
                  CLI
                </p>
              </div>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                The command line interface.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                Every operation is available from the CLI. Pipe-friendly output,
                JSON mode for scripting, consistent subcommand structure.
              </p>
            </motion.div>

            <div className="space-y-3">
              {cliCommands.map((item, i) => (
                <motion.div key={item.cmd} {...fadeUp} transition={{ delay: i * 0.05 }}>
                  <Card className="p-4">
                    <div className="flex flex-col sm:flex-row sm:items-center gap-2 sm:gap-6">
                      <div className="flex items-center gap-2 shrink-0">
                        <span className="text-text-muted text-xs select-none">$</span>
                        <Mono className="text-sm text-text">{item.cmd}</Mono>
                      </div>
                      <p className="text-xs text-text-muted sm:ml-auto">
                        {item.description}
                      </p>
                    </div>
                  </Card>
                </motion.div>
              ))}
            </div>

            {/* Full CLI example */}
            <motion.div {...fadeUp} className="mt-8">
              <Card className="p-6">
                <p className="text-xs font-mono uppercase tracking-widest text-text-muted mb-4">
                  Typical workflow
                </p>
                <div className="space-y-1.5">
                  <Mono className="text-sm text-text-muted"># See what&apos;s running</Mono>
                  <div className="flex items-start gap-3">
                    <span className="text-text-muted text-xs select-none shrink-0 w-4">$</span>
                    <Mono className="text-sm text-text-secondary">shift workloads</Mono>
                  </div>
                  <div className="h-2" />
                  <Mono className="text-sm text-text-muted"># Checkpoint a workload</Mono>
                  <div className="flex items-start gap-3">
                    <span className="text-text-muted text-xs select-none shrink-0 w-4">$</span>
                    <Mono className="text-sm text-text-secondary">shift checkpoint create training-run-7</Mono>
                  </div>
                  <div className="h-2" />
                  <Mono className="text-sm text-text-muted"># Move to an authenticated peer and wait</Mono>
                  <div className="flex items-start gap-3">
                    <span className="text-text-muted text-xs select-none shrink-0 w-4">$</span>
                    <Mono className="text-sm text-text-secondary">shift migrate training-run-7 --wait --to https://target:8443 --machine-id ID</Mono>
                  </div>
                  <div className="h-2" />
                  <Mono className="text-sm text-text-muted"># Inspect workload logs</Mono>
                  <div className="flex items-start gap-3">
                    <span className="text-text-muted text-xs select-none shrink-0 w-4">$</span>
                    <Mono className="text-sm text-text-secondary">shift logs workload-id</Mono>
                  </div>
                  <div className="h-2" />
                  <Mono className="text-sm text-text-muted"># List local workloads after the move</Mono>
                  <div className="flex items-start gap-3">
                    <span className="text-text-muted text-xs select-none shrink-0 w-4">$</span>
                    <Mono className="text-sm text-text-secondary">shift workloads</Mono>
                  </div>
                </div>
              </Card>
            </motion.div>
          </Container>
        </Section>

        {/* REST API */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                REST API
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                HTTP API.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-12">
                The CLI talks to the local agent; the control plane exposes its own
        authenticated REST API. Both use JSON contracts and fail closed on invalid input.
              </p>
            </motion.div>

            <motion.div {...fadeUp}>
              <Card className="overflow-hidden">
                <div className="divide-y divide-border-subtle">
                  {apiEndpoints.map((endpoint) => (
                    <div
                      key={`${endpoint.method}-${endpoint.path}`}
                      className="flex items-center gap-4 p-4 hover:bg-bg-hover/50 transition-colors"
                    >
                      <Mono
                        className={`text-xs font-bold w-14 shrink-0 ${methodColors[endpoint.method] || "text-text-muted"}`}
                      >
                        {endpoint.method}
                      </Mono>
                      <Mono className="text-sm text-text-secondary flex-1">
                        {endpoint.path}
                      </Mono>
                      <span className="text-xs text-text-muted hidden sm:block">
                        {endpoint.description}
                      </span>
                    </div>
                  ))}
                </div>
              </Card>
            </motion.div>

            {/* API example */}
            <motion.div {...fadeUp} className="mt-8">
              <Card className="p-6">
                <p className="text-xs font-mono uppercase tracking-widest text-text-muted mb-4">
                  Example request
                </p>
                <div className="bg-bg rounded-md border border-border-subtle p-4 overflow-x-auto">
                  <pre className="text-sm text-text-secondary leading-relaxed">
{`curl -X POST https://<control-plane-host>/v1/migrations \\
  -H "Authorization: Bearer sg_key_..." \\
  -H "Content-Type: application/json" \\
  -d '{
    "workload_id": "training-run-7",
    "target_machine": "gpu-node-1",
    "encrypted": true,
    "verify_after_restore": true
  }'`}
                  </pre>
                </div>
              </Card>
            </motion.div>
          </Container>
        </Section>

        {/* SDK */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                SDK
              </p>
              <h2 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Programmatic access.
              </h2>
              <p className="text-text-secondary max-w-2xl mb-8">
                A TypeScript SDK wraps the REST API for use in your own tools
                and automation.
              </p>
              <Badge variant="info">Conceptual — API subject to change</Badge>
            </motion.div>

            <motion.div {...fadeUp} className="mt-8">
              <Card className="p-6">
                <div className="bg-bg rounded-md border border-border-subtle p-4 overflow-x-auto">
                  <pre className="text-sm text-text-secondary leading-relaxed">
{`import { Shiftgate } from "@shiftgate/sdk";

const sg = new Shiftgate({
  apiKey: process.env.SHIFTGATE_API_KEY,
});

// List machines
const machines = await sg.machines.list();

// Create a checkpoint
const checkpoint = await sg.checkpoints.create({
  workloadId: "training-run-7",
});

// Wait for checkpoint to complete
await checkpoint.waitForReady();

// Migrate to target
const migration = await sg.migrations.create({
  checkpointId: checkpoint.id,
  targetMachine: "gpu-node-1",
});

// Stream migration events
for await (const event of migration.events()) {
  console.log(event.stage, event.progress);
}`}
                  </pre>
                </div>
              </Card>
            </motion.div>
          </Container>
        </Section>

        {/* Documentation CTA */}
        <Section className="border-t border-border-subtle">
          <Container narrow>
            <motion.div {...fadeUp} className="text-center">
              <BookOpen className="w-6 h-6 text-accent mx-auto mb-4" />
              <h2 className="text-2xl md:text-3xl font-bold tracking-tight mb-4">
                Full documentation.
              </h2>
              <p className="text-text-secondary leading-relaxed mb-8">
                Installation guides, API reference, configuration options,
                troubleshooting. Everything you need to get started.
              </p>
              <Link
                href="/docs"
                className="inline-flex items-center gap-2 text-sm text-accent hover:text-accent-dim transition-colors"
              >
                Read the docs
                <ArrowRight className="w-4 h-4" />
              </Link>
            </motion.div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
