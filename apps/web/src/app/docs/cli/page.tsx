import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "CLI Reference",
  description: "Reference for the shift commands implemented by this agent with syntax, flags, and behavior.",
};

interface Command {
  name: string;
  syntax: string;
  description: string;
  flags?: string[];
}

interface CommandGroup {
  title: string;
  commands: Command[];
}

const COMMAND_GROUPS: CommandGroup[] = [
  {
    title: "General",
    commands: [
      {
        name: "doctor",
        syntax: "shift doctor",
        description: "Verify the local Linux host, CRIU kernel support, GNU tar, state paths, and agent prerequisites.",
      },
      {
        name: "status",
        syntax: "shift status",
        description: "Show local agent health, machine identity, workload count, and recent activity.",
      },
    ],
  },
  {
    title: "Machine management",
    commands: [
      {
        name: "machines",
        syntax: "shift machines",
        description: "Show machines known to the control plane, including identity, capabilities, and recent status metadata.",
      },
      {
        name: "identity",
        syntax: "shift identity",
        description: "Show the local agent's generated Ed25519 machine identity.",
      },
      {
        name: "machine",
        syntax: "shift machine",
        description: "Inspect local hardware, kernel, storage, GPU inventory, and CRIU capabilities.",
      },
      {
        name: "global option",
        syntax: "shift --agent unix://PATH|https://HOST:PORT",
        description: "Select the local agent endpoint for every command; remote endpoints require mTLS credentials.",
      },
    ],
  },
  {
    title: "Workload management",
    commands: [
      {
        name: "workload list",
        syntax: "shift workloads",
        description: "List workloads managed by the selected agent.",
        flags: ["--json"],
      },
      {
        name: "workload create",
        syntax: "shift workload create NAME --path PATH -- COMMAND [ARGS...]",
        description: "Register an explicitly scoped local workload with the connected agent; use --start to launch it immediately.",
        flags: [
          "--name <name>",
          "--path <dir>",
          "--workdir <dir>",
          "--start",
          "--memory <size>",
          "--cpu <count>",
          "--network preserve|reconnect|drain",
          "--env KEY=VALUE",
        ],
      },
      {
        name: "workload inspect",
        syntax: "shift workload inspect ID",
        description: "Show a workload spec, runtime state, process identity, and latest checkpoint reference.",
      },
      {
        name: "workload stop",
        syntax: "shift workload stop ID [--timeout SECONDS]",
        description: "Stop a running process group and release its runtime record after the grace period.",
        flags: ["--timeout <seconds>"],
      },
    ],
  },
  {
    title: "Checkpoint management",
    commands: [
      {
        name: "checkpoint list",
        syntax: "shift checkpoints [WORKLOAD]",
        description: "List checkpoint summaries retained by the selected agent.",
        flags: ["--json"],
      },
      {
        name: "checkpoint create",
        syntax: "shift checkpoint create WORKLOAD [--parent CHECKPOINT_ID]",
        description: "Create a full checkpoint by default, or an incremental checkpoint when a retained parent is supplied.",
        flags: ["--parent <checkpoint-id>", "--leave-running", "--tcp-state", "--timeout <seconds>"],
      },
      {
        name: "restore",
        syntax: "shift restore CHECKPOINT_ID [--timeout SECONDS]",
        description: "Restore locally through the selected agent, validate health, and commit only after validation succeeds.",
      },
      {
        name: "mirror",
        syntax: "shift checkpoint mirror CHECKPOINT_ID",
        description: "Retry publication to configured local or S3-compatible object storage after a transient failure.",
      },
    ],
  },
  {
    title: "Migration",
    commands: [
      {
        name: "migrate",
        syntax: "shift migrate WORKLOAD --to https://HOST:PORT --machine-id DESTINATION_ID",
        description: "Migrate a running workload to a different machine. SHIFTGATE checkpoints the workload, transfers state, restores on the target, and validates before switching.",
        flags: [
          "--mode live|cold",
          "--to <peer-url>",
          "--server-name <tls-name>",
          "--timeout <seconds>",
          "--wait",
        ],
      },
    ],
  },
  {
    title: "Authentication",
    commands: [
      {
        name: "agent authentication",
        syntax: "mTLS or Unix SO_PEERCRED",
        description: "Remote peers use mutual TLS; local Unix connections use peer credentials. Configure control-plane API credentials in the service environment.",
      },
      {
        name: "output",
        syntax: "shift --json COMMAND",
        description: "Emit machine-readable JSON for supported commands.",
      },
    ],
  },
];

export default function CliPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Reference
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            CLI reference
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Every implemented <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">shift</code> command
            with syntax, flags, and descriptions.
          </p>
        </div>

        <div className="space-y-12">
          {COMMAND_GROUPS.map((group) => (
            <div key={group.title}>
              <h2 className="text-lg font-semibold mb-6 pb-2 border-b border-border-subtle">
                {group.title}
              </h2>

              <div className="space-y-6">
                {group.commands.map((cmd) => (
                  <div key={cmd.name} className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
                    <div className="p-5">
                      <div className="flex items-start justify-between gap-4 mb-3 flex-wrap">
                        <h3 className="text-sm font-semibold font-mono text-text">
                          {cmd.syntax}
                        </h3>
                      </div>
                      <p className="text-sm text-text-secondary leading-relaxed mb-3">
                        {cmd.description}
                      </p>
                      {cmd.flags && cmd.flags.length > 0 && (
                        <div>
                          <p className="text-xs font-mono text-text-muted uppercase tracking-wider mb-2">
                            Flags
                          </p>
                          <div className="flex flex-wrap gap-2">
                            {cmd.flags.map((flag) => (
                              <code
                                key={flag}
                                className="text-xs font-mono bg-bg-surface border border-border-subtle rounded px-2 py-1 text-text-secondary"
                              >
                                {flag}
                              </code>
                            ))}
                          </div>
                        </div>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>
      </Container>
    </Section>
  );
}
