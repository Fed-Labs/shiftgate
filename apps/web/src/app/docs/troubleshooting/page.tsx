import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Troubleshooting",
  description: "Common issues, diagnostics, and solutions for SHIFTGATE.",
};

const ISSUES = [
  {
    title: "Agent is unreachable",
    symptoms: "The CLI cannot connect to the local agent, or a peer reports the destination as unreachable.",
    causes: [
      "The agent process is not running",
      "The configured Unix socket path is wrong",
      "Remote mTLS certificates, names, or firewall rules are invalid",
    ],
    solutions: [
      "Check service status and logs: systemctl status shift-agent; journalctl -u shift-agent",
      "Confirm --listen matches the CLI --agent endpoint",
      "Run ./bin/shift doctor on both machines",
      "Verify remote certificates and that TCP 443 reaches the destination agent",
    ],
  },
  {
    title: "Migration failed: incompatible GPU",
    symptoms: "Migration fails at the VALIDATE stage with a GPU compatibility error.",
    causes: [
      "Source and target machines have different GPU models",
      "CUDA driver versions don't match between machines",
      "Target machine doesn't have enough GPU memory",
      "Multi-GPU topology doesn't match",
    ],
    solutions: [
      "Run ./bin/shift machine on each host and compare inventory",
      "Require a vendor checkpoint/restore adapter for GPU context state; otherwise reinitialize device state",
      "Match declared device requirements to available hardware",
      "Do not copy generic /dev files as a substitute for supported device state",
    ],
  },
  {
    title: "Checkpoint failed",
    symptoms: "Checkpoint creation fails with a CRIU error or timeout.",
    causes: [
      "Kernel does not have CONFIG_CHECKPOINT_RESTORE enabled",
      "CRIU version is unsupported or its kernel check fails",
      "Process uses features CRIU cannot checkpoint (e.g., certain io_uring ops)",
      "Insufficient disk space for checkpoint data",
    ],
    solutions: [
      "Check kernel config: zcat /proc/config.gz | grep CHECKPOINT_RESTORE",
      "Install a distribution-supported CRIU build; SHIFT does not replace the kernel",
      "Check available disk space on the machine",
      "Review agent logs for the specific CRIU error message",
      "Some processes may need to be configured to avoid non-checkpointable features",
    ],
  },
  {
    title: "Permission denied",
    symptoms: "Control-plane calls return 403 Forbidden or local CLI commands fail with 'permission denied'.",
    causes: [
      "The control-plane API key lacks the required scope or role",
      "A non-root agent cannot change to the workload UID/GID",
      "State, socket, TLS, or workload paths are not readable by the service account",
    ],
    solutions: [
      "Rotate the API key and verify organization roles in the control plane",
      "Run the agent with the identity required by your workloads",
      "Check ownership and mode bits on every explicitly configured path",
    ],
  },
];

export default function TroubleshootingPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Diagnostics
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Troubleshooting
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Common issues and how to resolve them. If you can&apos;t find your
            problem here, reach out for help.
          </p>
        </div>

        <div className="space-y-8">
          {ISSUES.map((issue) => (
            <div key={issue.title} className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
              <div className="p-6">
                <h2 className="text-lg font-semibold mb-4">{issue.title}</h2>

                <div className="mb-4">
                  <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-2">
                    Symptoms
                  </p>
                  <p className="text-sm text-text-secondary">{issue.symptoms}</p>
                </div>

                <div className="mb-4">
                  <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-2">
                    Possible causes
                  </p>
                  <ul className="space-y-1.5">
                    {issue.causes.map((cause) => (
                      <li key={cause} className="flex items-start gap-2 text-sm text-text-secondary">
                        <span className="text-status-error mt-1 shrink-0">•</span>
                        {cause}
                      </li>
                    ))}
                  </ul>
                </div>

                <div>
                  <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-2">
                    Solutions
                  </p>
                  <ul className="space-y-1.5">
                    {issue.solutions.map((solution) => (
                      <li key={solution} className="flex items-start gap-2 text-sm text-text-secondary">
                        <span className="text-accent mt-1 shrink-0">→</span>
                        {solution}
                      </li>
                    ))}
                  </ul>
                </div>
              </div>
            </div>
          ))}
        </div>

        {/* Getting help */}
        <div className="mt-16 rounded-lg border border-border bg-bg-elevated p-6">
          <h2 className="text-lg font-semibold mb-4">How to get help</h2>
          <div className="grid gap-4 sm:grid-cols-3">
            <div>
              <p className="text-sm font-medium mb-1">Documentation</p>
              <p className="text-sm text-text-secondary">
                You&apos;re here. Browse the sidebar for guides and references.
              </p>
            </div>
            <div>
              <p className="text-sm font-medium mb-1">GitHub Issues</p>
              <p className="text-sm text-text-secondary">
                Report bugs and request features at github.com/shiftgate/shiftgate.
              </p>
            </div>
            <div>
              <p className="text-sm font-medium mb-1">Enterprise support</p>
              <p className="text-sm text-text-secondary">
                Enterprise customers have access to dedicated support with SLA-backed response times.
              </p>
            </div>
          </div>
        </div>
      </Container>
    </Section>
  );
}
