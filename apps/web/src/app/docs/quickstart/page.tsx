import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";
import { ArrowRight } from "lucide-react";

export const metadata: Metadata = {
  title: "Quickstart",
  description: "Build SHIFTGATE locally and run a real checkpoint/restore workflow on Linux.",
};

const STEPS = [
  {
    number: "01",
    title: "Prepare Linux",
    description: "Use an x86_64 Linux host with CRIU, GNU tar, Go 1.24+, and the kernel capabilities CRIU reports through `shift doctor`.",
    command: "criu --version && tar --version | head -1 && go version",
    output: `CRIU version 4.0
tar (GNU tar) 1.35
go version go1.24.0 linux/amd64`,
  },
  {
    number: "02",
    title: "Build and check the agent",
    description: "Build the CLI and agent from this repository, start a development agent on writable local paths, then verify checkpoint support before creating workloads.",
    command: `make build
mkdir -p ./data/run
./bin/shift-agent --state-dir ./data --listen unix://$PWD/data/agent.sock
# In another terminal:
./bin/shift --agent unix://$PWD/data/agent.sock doctor`,
    output: `Healthy: true
  ✓ CRIU installed and kernel check passed
  ✓ GNU tar available

If doctor reports a kernel capability error, resolve that prerequisite first; SHIFT does not substitute a fake checkpoint.`,
  },
  {
    number: "03",
    title: "Create and start a scoped workload",
    description: "Attach one explicit directory as the workload root. Commands run under the agent, and only that approved root is captured.",
    command: `./bin/shift --agent unix://$PWD/data/agent.sock workload create demo \\
  --path "$PWD/demo" --start -- /usr/bin/python3 -m http.server 8080`,
    output: `Workload wl_example created.
Status: running
Root:   /absolute/path/to/demo`,
  },
  {
    number: "04",
    title: "Checkpoint and restore locally",
    description: "Create an encrypted, content-addressed checkpoint while leaving the source running, then restore it to validate that process and filesystem state round-trip on this host.",
    command: `./bin/shift --agent unix://$PWD/data/agent.sock checkpoint create demo --leave-running
./bin/shift --agent unix://$PWD/data/agent.sock restore CHECKPOINT_ID`,
    output: `Checkpoint ckpt_example created.
Plain:  ... Stored: ...
Checkpoint restored and committed. Process PID: 12345`,
  },
  {
    number: "05",
    title: "Move between two agents",
    description: "After both hosts pass `doctor`, migrate over mutually authenticated HTTPS. Use cold mode for the simplest path or live mode to invoke CRIU pre-copy before its authoritative final dump.",
    command: `./bin/shift migrate demo \\
  --to https://destination.example:8443 \\
  --machine-id DESTINATION_MACHINE_ID \\
  --mode cold`,
    output: `Migration mig_example created.
SNAPSHOT → PREPARE → TRANSFER → VERIFY → RESTORE
POST_VALIDATE → SWITCH → COMMIT → COMPLETED`,
  },
];

export default function QuickstartPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Getting started
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Quickstart
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Run a real checkpoint-backed workflow in five steps. The examples use
            a local development socket; production agents require mutual TLS.
          </p>
        </div>

        <div className="space-y-12">
          {STEPS.map((step, i) => (
            <div key={step.number} className="relative">
              {/* Connector line */}
              {i < STEPS.length - 1 && (
                <div className="absolute left-[18px] top-12 bottom-[-3rem] w-px bg-border-subtle hidden md:block" />
              )}

              <div className="flex gap-5">
                {/* Step number */}
                <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-bg-surface border border-border text-sm font-mono font-semibold text-accent">
                  {step.number}
                </div>

                <div className="min-w-0 flex-1">
                  <h2 className="text-xl font-semibold mb-2">{step.title}</h2>
                  <p className="text-text-secondary text-sm leading-relaxed mb-4">
                    {step.description}
                  </p>

                  {/* Command */}
                  <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
                    <div className="flex items-center gap-2 border-b border-border-subtle px-4 py-2">
                      <div className="h-2 w-2 rounded-full bg-status-error/60" />
                      <div className="h-2 w-2 rounded-full bg-status-warning/60" />
                      <div className="h-2 w-2 rounded-full bg-status-healthy/60" />
                      <span className="ml-2 text-xs text-text-muted font-mono">terminal</span>
                    </div>
                    <div className="p-4">
                      <pre className="text-sm text-accent">
                        <code>{step.command}</code>
                      </pre>
                      {step.output && (
                        <>
                          <div className="my-3 h-px bg-border-subtle" />
                          <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                            <code>{step.output}</code>
                          </pre>
                        </>
                      )}
                    </div>
                  </div>
                </div>
              </div>
            </div>
          ))}
        </div>

        {/* Next steps */}
        <div className="mt-16 rounded-lg border border-border bg-bg-elevated p-6">
          <h3 className="text-sm font-semibold mb-3">Next steps</h3>
          <ul className="space-y-2 text-sm text-text-secondary">
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>Read <a href="/docs/concepts" className="text-text hover:text-accent transition-colors">Concepts</a> to understand the core model</span>
            </li>
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>Explore the <a href="/docs/cli" className="text-text hover:text-accent transition-colors">CLI reference</a> for all available commands</span>
            </li>
            <li className="flex items-center gap-2">
              <ArrowRight size={14} className="text-accent shrink-0" />
              <span>Learn about <a href="/docs/migration" className="text-text hover:text-accent transition-colors">migration modes</a> and their actual stop-the-world boundaries</span>
            </li>
          </ul>
        </div>
      </Container>
    </Section>
  );
}
