import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Troubleshooting",
  description: "Common issues, diagnostics, and solutions for SHIFT.",
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
      "Run shiftgate doctor on both machines",
      "Verify remote certificates and that TCP 443 reaches the destination agent",
    ],
  },
  {
    title: "`shift` types nothing; the CLI is `shiftgate`",
    symptoms: "Running `shift` does nothing visible or shifts shell positional parameters instead of running the CLI.",
    causes: [
      "`shift` is a POSIX shell builtin, so a binary by that name can never be typed",
    ],
    solutions: [
      "Run `shiftgate` — the binary is shiftgate, not shift",
      "A v0.1.0 install left a /usr/local/bin/shift that no shell ever ran; the current installer removes it",
    ],
  },
  {
    title: "Dashboard login fails with \"Failed to fetch\"",
    symptoms: "The browser blocks the control-plane call at login or afterwards.",
    causes: [
      "The control plane serves no CORS header for the dashboard's origin",
    ],
    solutions: [
      "Set SHIFT_ALLOWED_ORIGINS to the exact origin the dashboard is served from (http://localhost:3000 in development) and restart shift-control",
      "The CLI and agents are unaffected either way — they send no Origin header",
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
      "Run shiftgate machine on each host and compare inventory",
      "Require a vendor checkpoint/restore adapter for GPU context state; otherwise reinitialize device state",
      "Match declared device requirements to available hardware",
      "Do not copy generic /dev files as a substitute for supported device state",
    ],
  },
  {
    title: "Migration fails with DESTINATION_UNREACHABLE",
    symptoms: "The source agent cannot reach the destination's peer URL.",
    causes: [
      "The destination's remote_listen is not reachable from the source",
      "A firewall blocks the peer port",
      "The source lacks a client certificate the destination's client CA trusts",
    ],
    solutions: [
      "From the source machine, curl -k https://DEST_HOST:PORT/v1/peer/machine — it must answer JSON",
      "Check remote_listen on the destination agent and any firewall between the machines",
      "In production the peer listener requires mutual TLS; insecure_development is for loopback evaluation only",
    ],
  },
  {
    title: "Migration fails mid-transfer; what happens to my workload?",
    symptoms: "A migration dies partway through with a network or destination error.",
    causes: [
      "Peer connection dropped, destination out of disk, or a compatibility gap surfaced mid-move",
    ],
    solutions: [
      "The move fails safely: any failure rolls back the destination and resumes the source workload",
      "Read the record — it shows ROLLED_BACK with source_preserved: true and the failure code: shiftgate status MIGRATION_ID",
      "If it shows FAILED with 'rollback requires operator intervention', the rollback itself hit an error; the source root is still on disk and the reason names the step",
      "Retrying is safe: SHIFT re-validates compatibility and starts a fresh pipeline",
    ],
  },
  {
    title: "402 LIVE_MIGRATION_PLAN_REQUIRED",
    symptoms: "A dashboard MOVE or migration job with mode live is refused.",
    causes: [
      "The organization's plan is the free tier, which is cold-only",
    ],
    solutions: [
      "Re-dispatch with cold mode (the default), or upgrade the plan in billing",
      "A shiftgate migrate --mode live run from a machine's CLI talks peer-to-peer and is not subject to the tier",
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
      "Install a distribution-supported CRIU build (3.17+ or 4.x); SHIFT does not replace the kernel",
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

const ERROR_CODES = [
  ["DESTINATION_UNREACHABLE", "The source could not reach the destination's peer URL. Check remote_listen, DNS, and firewall."],
  ["DESTINATION_INCOMPATIBLE", "The compatibility report lists every unmet requirement with a severity. Address them or pick another destination."],
  ["DESTINATION_IS_SOURCE", "The resolved destination is the source machine itself."],
  ["DESTINATION_IDENTITY_MISMATCH", "The destination's certificate machine id does not match the requested id."],
  ["LIVE_MIGRATION_PLAN_REQUIRED", "Live mode dispatched through the control plane requires a plan whose tier allows it."],
  ["STATE_FORMAT_UPGRADE_REQUIRED", "The checkpoint was written by a newer minor state format; upgrade the destination agent."],
  ["STATE_FORMAT_UNSUPPORTED", "State older than the supported floor, or a different format line."],
  ["KERNEL_INCOMPATIBLE", "The destination kernel is older than the source kernel."],
  ["KERNEL_CROSS_MAJOR_NOT_CERTIFIED", "A kernel-major crossing with no certified policy entry."],
  ["DISTRO_INCOMPATIBLE", "The distribution pair is refused by the compatibility policy."],
  ["CHECKPOINT_FAILED", "A checkpoint could not be created — including a create racing a checkpoint already in flight."],
  ["POLICY_INVALID", "The requested checkpoint policy is impossible (for example an interval under 10 seconds)."],
  ["CLONE_FAILED", "A clone set was refused up front (for example a larger set of a workload that declares TCP ports)."],
  ["STORAGE_QUOTA_EXCEEDED", "Hosted-storage credential issuance stops over quota; local checkpointing keeps working."],
  ["UPDATE_DEFERRED", "An update install arrived while a migration, restore, fork, or transfer was in flight; retry later."],
  ["GC_OFFLINE_REQUIRED / GC_ROOT_REQUIRED", "Online chunk GC is disabled by design; run the offline shift-gc procedure shiftgate gc prints."],
  ["RETENTION_PLAN_LIMIT", "The requested retention window exceeds the plan's cap."],
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

        {/* Error and refusal codes */}
        <div className="mt-16">
          <h2 className="text-lg font-semibold mb-4 pb-2 border-b border-border-subtle">
            Error and refusal codes
          </h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Codes the system refuses with deliberately — each names the failing
            step and preserves the source state. Migration failures also map to
            CLI exit code 5; a refused preflight exits 3.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                {ERROR_CODES.map(([code, desc]) => (
                  <tr key={code}>
                    <td className="px-5 py-3 font-mono text-xs text-accent w-1/3">{code}</td>
                    <td className="px-5 py-3 text-xs text-text-secondary">{desc}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
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
                Report bugs and request features at github.com/Fed-Labs/shiftgate.
              </p>
            </div>
            <div>
              <p className="text-sm font-medium mb-1">Enterprise support</p>
              <p className="text-sm text-text-secondary">
                Enterprise support is provided under your contract — response
                times and channels are defined there. There is no published SLA
                today.
              </p>
            </div>
          </div>
        </div>
      </Container>
    </Section>
  );
}
