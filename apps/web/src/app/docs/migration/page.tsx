import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Migration",
  description: "Cold and pre-copy-assisted migration, the migration pipeline, compatibility checking, and failure handling.",
};

const PIPELINE_STAGES = [
  { name: "CREATE", description: "Migration request received. Target machine selected." },
  { name: "DISCOVER", description: "Source machine reports full workload state and dependencies." },
  { name: "VALIDATE", description: "Target machine compatibility verified against workload requirements." },
  { name: "SNAPSHOT", description: "Source workload state captured with a full dump or CRIU pre-copy followed by the authoritative final dump." },
  { name: "PREPARE", description: "State serialized, compressed, and encrypted for transfer." },
  { name: "TRANSFER", description: "Encrypted state transferred to target machine over authenticated channel." },
  { name: "VERIFY", description: "Target validates integrity hashes and decrypts state." },
  { name: "RESTORE", description: "Process state restored on target machine via CRIU." },
  { name: "POST_VALIDATE", description: "Restored workload validated — process running, resources allocated." },
  { name: "SWITCH", description: "Traffic and ownership transferred from source to target." },
  { name: "COMMIT", description: "Migration finalized. Source workload terminated." },
  { name: "CLEANUP", description: "Temporary state files removed on both machines." },
  { name: "COMPLETED", description: "Migration complete. Workload running on target machine." },
];

export default function MigrationPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Moving computation
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Migration
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Migration is the process of moving a running workload from one machine
            to another. SHIFTGATE handles the full pipeline — checkpoint, transfer,
            restore, validate, and switch — automatically.
          </p>
        </div>

        {/* Pipeline */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">The migration pipeline</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-6">
            Every migration passes through a defined sequence of stages. Each
            stage is atomic — if a stage fails, the migration halts and the
            source workload is preserved unchanged.
          </p>
          <div className="space-y-1">
            {PIPELINE_STAGES.map((stage, i) => (
              <div key={stage.name} className="flex items-start gap-4 rounded-md px-4 py-2.5 odd:bg-bg-elevated/50">
                <div className="flex items-center gap-3 shrink-0 w-36">
                  <span className="text-xs font-mono text-text-muted w-6 text-right">{String(i + 1).padStart(2, "0")}</span>
                  <code className="text-xs font-mono text-accent">{stage.name}</code>
                </div>
                <p className="text-sm text-text-secondary pt-px">{stage.description}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Cold vs live */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Cold vs live migration</h2>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="rounded-lg border border-border bg-bg-elevated p-5">
              <h3 className="text-sm font-semibold mb-3">Cold migration</h3>
              <p className="text-sm text-text-secondary leading-relaxed mb-4">
                The workload is paused, fully checkpointed, transferred, and
                restored on the target. The process is interrupted for the
                entire duration of the migration.
              </p>
              <div className="text-xs text-text-muted space-y-1.5">
                <div className="flex justify-between">
                  <span>Downtime</span>
                  <span className="font-mono">Seconds to minutes</span>
                </div>
                <div className="flex justify-between">
                  <span>Complexity</span>
                  <span>Low</span>
                </div>
                <div className="flex justify-between">
                  <span>Use case</span>
                  <span>Batch jobs, non-interactive</span>
                </div>
              </div>
            </div>
            <div className="rounded-lg border border-accent/20 bg-accent/5 p-5">
              <h3 className="text-sm font-semibold mb-3 text-accent">Live migration</h3>
              <p className="text-sm text-text-secondary leading-relaxed mb-4">
                CRIU pre-copy runs before the authoritative final checkpoint.
                The final checkpoint still pauses the workload for the transfer
                and restore stages, while reducing repeated memory work.
              </p>
              <div className="text-xs text-text-muted space-y-1.5">
                <div className="flex justify-between">
                  <span>Downtime</span>
                  <span className="font-mono">Depends on state size</span>
                </div>
                <div className="flex justify-between">
                  <span>Complexity</span>
                  <span>Higher</span>
                </div>
                <div className="flex justify-between">
                  <span>Use case</span>
                  <span>Workloads that benefit from pre-copy</span>
                </div>
              </div>
            </div>
          </div>
        </div>

        {/* Compatibility checking */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Compatibility checking</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Before any migration begins, SHIFTGATE validates that the target
            machine satisfies all of the workload&apos;s requirements. This includes
            CPU architecture, available memory, GPU model and VRAM, kernel
            version, CRIU feature set, and any explicitly required vendor adapter.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate machines
# Then start the migration; the VALIDATE stage records the report.`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Compatibility check: wl_k8m2p4q7 → mach_9e4d8c2a

  ✓ CPU:        x86_64 compatible
  ✓ Memory:     64 GB available (32 GB required)
  ✓ Device policy: compatible adapter reported
  ✓ Kernel:     6.1.0 (≥ 5.15 required)
  ✓ CRIU:       3.18 (≥ 3.15 required)
  ✓ Storage:    500 GB available (100 GB required)

  All checks passed. Target is compatible.`}</code>
            </pre>
          </div>
        </div>

        {/* Failure handling */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Failure handling</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Migrations are designed to be safe. If any stage fails:
          </p>
          <div className="space-y-3">
            {[
              { title: "Source is preserved", description: "The original workload continues running on the source machine. No data is lost." },
              { title: "Automatic rollback", description: "Any state transferred to the target is cleaned up. The target machine is left in its original state." },
              { title: "Failure is recorded", description: "The migration record includes the failed stage, error details, and timestamps for debugging." },
              { title: "Retry is safe", description: "You can retry the migration. SHIFTGATE re-validates compatibility and starts a fresh pipeline." },
            ].map((item) => (
              <div key={item.title} className="flex items-start gap-3 rounded-md border border-border-subtle bg-bg-elevated/50 px-4 py-3">
                <div className="h-1.5 w-1.5 rounded-full bg-accent mt-1.5 shrink-0" />
                <div>
                  <p className="text-sm font-medium mb-0.5">{item.title}</p>
                  <p className="text-sm text-text-secondary">{item.description}</p>
                </div>
              </div>
            ))}
          </div>
        </div>

        {/* Migration metrics */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Migration metrics</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            After a migration completes, SHIFTGATE reports metrics for each
            pipeline stage. These metrics help you understand migration
            performance and optimize workload placement.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Metric</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Description</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["total_duration", "Wall-clock time from CREATE to COMPLETED"],
                  ["checkpoint_duration", "Time to capture source state"],
                  ["transfer_duration", "Time to transfer state to target"],
                  ["transfer_bytes", "Total bytes transferred over the network"],
                  ["restore_duration", "Time to restore state on target"],
                  ["downtime", "Duration the workload was paused (live: stop-the-world only)"],
                  ["dirty_pages", "Number of pages modified during pre-copy (live only)"],
                ].map(([metric, desc]) => (
                  <tr key={metric}>
                    <td className="px-5 py-3 font-mono text-xs text-accent">{metric}</td>
                    <td className="px-5 py-3 text-text-secondary text-xs">{desc}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

        {/* Warm-standby failover */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Warm-standby failover</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            A workload can carry a failover policy: every checkpoint it takes is
            replicated to a standby agent, which holds the newest copies and
            watches the source. When the source&apos;s peer listener is
            unreachable and the control plane&apos;s record for it is stale or
            offline, the standby restores the newest checkpoint there — or an
            operator triggers the same restore by hand.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5 mb-4">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate workload failover api --to https://standby:8443 --keep 3
shiftgate failover            # source-side: policy status
shiftgate standby list        # standby-side: duties held, watch armed
shiftgate standby trigger api # explicit failover restore`}</code>
            </pre>
          </div>
          <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-5">
            <p className="text-sm text-status-warning font-medium mb-3">
              There is no fencing
            </p>
            <p className="text-sm text-text-secondary leading-relaxed">
              A source that is partitioned rather than dead can come back to a
              second live copy. Automatic failover requires a configured
              control plane so the standby can confirm absence; without one,
              only an explicit <code className="font-mono text-xs">standby trigger</code> can act.
            </p>
          </div>
        </div>

        {/* Lazy restore */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Lazy restore</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Restores are eager by default: the full memory image is loaded
            before the process exists. Lazy restore inverts that — the process
            starts immediately and its memory pages stream in on demand through
            a userfaultfd-backed pages daemon. Time-to-running stops depending
            on the size of the address space.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">shiftgate restore ckpt_a1b2c3d4 --lazy</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <p className="text-sm text-text-secondary leading-relaxed">
              The trade is explicit: a half-served workload depends on its
              pages daemon for every missing page. The daemon runs for the
              process&apos;s lifetime and its death is reported honestly —
              SHIFTGATE never claims a lazy restore succeeded while pages are
              still unserved. Kernel userfaultfd support is required.
            </p>
          </div>
        </div>

        {/* Device compatibility */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Device compatibility</h2>
          <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-5">
            <p className="text-sm text-status-warning font-medium mb-3">
              Device state is capability-gated
            </p>
            <ul className="text-sm text-text-secondary space-y-2">
              <li>• Required devices are checked against the destination inventory</li>
              <li>• GPU state is restored only when the vendor adapter advertises checkpoint support</li>
              <li>• Generic device files are never copied as a substitute for device state</li>
              <li>• Applications using external device state should use reconnect or restart hooks</li>
            </ul>
          </div>
        </div>
      </Container>
    </Section>
  );
}
