import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Platforms and Compatibility",
  description: "Certified platforms, the CRIU support policy, GPU evidence rules, and the compatibility codes SHIFT refuses migrations with.",
};

const MATRIX = [
  {
    line: "Ubuntu 24.04 · x86_64",
    kernel: "6.8 – 6.14",
    criu: "4.0 – 4.3",
    status: "certified",
    note: "The release CI e2e and stress lanes run the full checkpoint/restore suite on ubuntu-24.04 runners with kernel 6.8+ and CRIU 4.x from the criu PPA.",
  },
  {
    line: "Ubuntu 22.04 · x86_64",
    kernel: "5.15",
    criu: "3.17 – 4.2",
    status: "untested",
    note: "Conservative LTS configuration consistent with the CRIU support policy; not exercised by the release CI lane. Experimental only — verify on the target host before relying on it.",
  },
  {
    line: "Debian 12 · x86_64",
    kernel: "6.1 – 6.12",
    criu: "3.17 – 4.2",
    status: "untested",
    note: "Benchmarks ran CRIU 3.17.1 on a 6.12 kernel in a bookworm environment — functional evidence only. Experimental only.",
  },
  {
    line: "Any other Linux · x86_64",
    kernel: "5.10+",
    criu: "3.17 – 4.3",
    status: "unknown",
    note: "Any other distribution/kernel combination. A passing criu check is not certification: unknown combinations are blocked for automatic operations until run through the support-matrix verification script and added to the matrix.",
  },
];

const STATUS_COLORS: Record<string, string> = {
  certified: "text-status-healthy",
  untested: "text-status-warning",
  unknown: "text-status-offline",
  unsupported: "text-status-error",
};

const CODES = [
  {
    code: "STATE_FORMAT_UPGRADE_REQUIRED",
    meaning: "The checkpoint was written by a newer minor state-format version than this agent supports. It may carry features this agent cannot restore.",
    remedy: "Upgrade the destination agent to at least the minor version named in the error, then restore again. The checkpoint is never partially restored.",
  },
  {
    code: "STATE_FORMAT_UNSUPPORTED",
    meaning: "The state is older than the supported floor, a different major format line, or not a SHIFT state file at all.",
    remedy: "Restore with an agent from the era that wrote the checkpoint, or upgrade the destination to the checkpoint's format line.",
  },
  {
    code: "KERNEL_INCOMPATIBLE",
    meaning: "The destination kernel is older than the source kernel. CRIU state cannot be restored onto an older kernel.",
    remedy: "Restore onto the same or a newer kernel than the source.",
  },
  {
    code: "KERNEL_CROSS_MAJOR_NOT_CERTIFIED",
    meaning: "Source and destination kernels differ in major version and no certified policy entry covers the pair.",
    remedy: "Stay within one kernel major, or certify the kernel pair in the compatibility policy after running checkpoint/restore tests for it.",
  },
  {
    code: "DISTRO_INCOMPATIBLE",
    meaning: "The source→destination distribution pair is refused by the compatibility policy.",
    remedy: "Move to a distribution pair the policy admits, or add an explicit allow rule for the pair after validating it.",
  },
];

export default function PlatformsPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Compatibility
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Platforms and compatibility
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            What SHIFT runs on, what is actually certified, and exactly how it
            refuses a restore or migration that would not work. The matrix is
            policy, not marketing: an uncertified line is not silently treated
            as supported.
          </p>
        </div>

        {/* CRIU policy */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">CRIU policy</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Real checkpoint/restore requires CRIU{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">3.17+</code>{" "}
            or{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">4.x</code>,
            and{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">criu check</code>{" "}
            must pass on the kernel you run.{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate doctor</code>{" "}
            probes CRIU and the kernel for real — it reports the gap instead of
            printing an optimistic yes, and the agent never produces a fake
            checkpoint.
          </p>
        </div>

        {/* Support matrix */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Support matrix</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Certified lines run the full checkpoint/restore suite in the release
            CI lanes. Everything else is exactly what its status says:
            &quot;untested&quot; lines are <em>experimental only</em>, and
            &quot;unknown&quot; combinations are <em>blocked for automatic
            operations</em> until they are verified and certified.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Line</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Kernel</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">CRIU</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Status</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {MATRIX.map((row) => (
                  <tr key={row.line}>
                    <td className="px-5 py-3">
                      <div className="font-mono text-xs text-accent">{row.line}</div>
                      <div className="text-xs text-text-muted mt-1 leading-relaxed">{row.note}</div>
                    </td>
                    <td className="px-5 py-3 font-mono text-xs text-text-secondary">{row.kernel}</td>
                    <td className="px-5 py-3 font-mono text-xs text-text-secondary">{row.criu}</td>
                    <td className={`px-5 py-3 font-mono text-xs font-semibold ${STATUS_COLORS[row.status]}`}>{row.status}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="mt-4 rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-2">
              What each status means
            </p>
            <ul className="text-sm text-text-secondary space-y-1.5">
              <li><span className="text-status-healthy font-mono text-xs">certified</span> — allowed; exercised by the release CI lanes.</li>
              <li><span className="text-status-warning font-mono text-xs">untested</span> — experimental only; plausible but not exercised by release CI.</li>
              <li><span className="text-status-offline font-mono text-xs">unknown</span> — blocked for automatic operations until verified and added to the matrix.</li>
              <li><span className="text-status-error font-mono text-xs">unsupported</span> — blocked for automatic operations; the feature does not exist in the implementation.</li>
            </ul>
          </div>
        </div>

        {/* GPUs */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">GPU lines</h2>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent w-1/4">nvidia · cuda</td>
                  <td className={`px-5 py-3 font-mono text-xs w-24 ${STATUS_COLORS.unknown}`}>unknown</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">
                    External GPU evidence is required before certification: no local
                    hardware claim is made. GPU context state is restored only when the
                    vendor driver advertises a checkpoint adapter; otherwise applications
                    must reconnect or reinitialize their device state.
                  </td>
                </tr>
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent">amd · rocm</td>
                  <td className={`px-5 py-3 font-mono text-xs ${STATUS_COLORS.unsupported}`}>unsupported</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">
                    ROCm process checkpoint/restore is not available in the current
                    implementation.
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        {/* Compatibility codes */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Compatibility refusal codes</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Compatibility is checked before any state moves, on the same inputs
            for a preflight and a real migration. Refusals are precise errors
            that preserve the source — the failure mode is always a clean
            refusal, never a corrupt restore.
          </p>
          <div className="space-y-3">
            {CODES.map((item) => (
              <div key={item.code} className="rounded-lg border border-border bg-bg-elevated p-5">
                <code className="text-sm font-mono text-accent block mb-2">{item.code}</code>
                <p className="text-sm text-text-secondary leading-relaxed mb-1">{item.meaning}</p>
                <p className="text-sm text-text-muted leading-relaxed">Remedy: {item.remedy}</p>
              </div>
            ))}
          </div>
        </div>
      </Container>
    </Section>
  );
}
