import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Migration",
  description: "Cold and pre-copy-assisted migration, the migration pipeline, compatibility checking, and failure handling.",
};

const PIPELINE_STAGES = [
  { name: "CREATED", description: "Migration request received. Target machine selected." },
  { name: "DISCOVER", description: "Source machine reports full workload state and dependencies." },
  { name: "VALIDATE", description: "Target machine compatibility verified against workload requirements." },
  { name: "SNAPSHOT", description: "Source workload state captured with a full dump or CRIU pre-copy followed by the authoritative final dump." },
  { name: "PREPARE", description: "State serialized, compressed, and encrypted for transfer." },
  { name: "TRANSFER", description: "Encrypted state transferred to target machine over authenticated channel." },
  { name: "VERIFY", description: "Target validates integrity hashes and decrypts state." },
  { name: "RESTORE", description: "Process state restored on target machine via CRIU." },
  { name: "POST_VALIDATE", description: "Restored workload validated — process running, resources allocated." },
  { name: "SWITCH", description: "Traffic and ownership transferred from source to target." },
  { name: "COMMIT", description: "Migration committed only after destination restore and health validation; the source remains preserved on failure." },
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
            to another. SHIFT handles the full pipeline — checkpoint, transfer,
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
                  <span className="font-mono">The whole move</span>
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
                CRIU pre-dump passes run while the workload keeps running: each
                pass parents the previous one and its images transfer as the
                pass completes, so the freeze pays only the final delta. The
                final checkpoint remains authoritative and the process stops
                only for that final transfer — pre-dump passes reduce the frozen
                window, never eliminate it.
              </p>
              <div className="text-xs text-text-muted space-y-1.5">
                <div className="flex justify-between">
                  <span>Downtime</span>
                  <span className="font-mono">Final delta only</span>
                </div>
                <div className="flex justify-between">
                  <span>Complexity</span>
                  <span>Higher</span>
                </div>
                <div className="flex justify-between">
                  <span>Use case</span>
                  <span>Large heaps, warm state</span>
                </div>
              </div>
            </div>
          </div>
          <div className="mt-4 rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-sm text-text-secondary leading-relaxed">
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--pre-copy-passes N</code>{" "}
              (0–16, default 0) caps the passes. Each pass parents the previous
              and the loop ends early once a pass&apos;s delta converges — so 0
              means a single pass and 4 does not promise four; a write-heavy
              workload honestly runs to the cap and pays the longer freeze. The
              migration record reports the split:{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">pre_copy_transferred_bytes</code>{" "}
              against the frozen window&apos;s transfer. A live migration that
              fails before its freeze reports a downtime of 0 ms — the workload ran
              the whole time and stays running on the source — and the
              destination refuses the whole migration up front when its free
              storage cannot hold the reservation the source measured from
              pass 1.
            </p>
          </div>
        </div>

        {/* Preflight */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Preflight without moving anything</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--dry-run</code>{" "}
            (alias{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--preflight</code>)
            answers &quot;would this migration work&quot; before anything moves:
            the agent reaches the destination over the same peer channel a real
            migration uses, runs the same compatibility check on the same
            inputs, and prints the two machines, the network plan the migration
            would apply, and every warning and rejection with its remedy.
            Nothing is frozen, transferred, or recorded — no migration record is
            created and the workload keeps running. Exit 0 means the migration
            would be admitted (warnings allowed); exit 3 means it would be
            rejected.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate migrate demo --to gpu-west --preflight
shiftgate migrate demo --to gpu-west --mode live --pre-copy-passes 4`}</code>
            </pre>
          </div>
        </div>

        {/* Routes */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Direct and relay routes</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            The supported remote path is direct peer mode: the source dials the
            destination&apos;s public HTTPS peer endpoint over TLS 1.3 mutual
            TLS. It works across the internet when DNS, firewall, and NAT
            forwarding are configured, and the local Unix API is never exposed.
            The platform-mediated migration job can also carry a route:
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent w-40">route: direct</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">Peer-to-peer mTLS between the two agents — the default and the supported path today.</td>
                </tr>
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent">route: relay</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">
                    The job record then carries <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">relay_endpoint</code>{" "}
                    (the relay bridge) and <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">relay_ticket</code>{" "}
                    (a one-use ticket for the transfer session, never logged). The
                    authenticated outbound relay ships its ticket and bridge
                    primitives, but agent lifecycle enrollment and route selection
                    are not enabled by default yet — direct public mTLS remains the
                    supported remote migration path, and NAT traversal is not
                    claimed.
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <div className="mt-4 rounded-lg border border-accent/20 bg-accent/5 p-5">
            <p className="text-sm text-accent font-medium mb-1">
              Plan gating
            </p>
            <p className="text-sm text-text-secondary leading-relaxed">
              Live mode dispatched through the control plane — the migration job
              route or the dashboard&apos;s MOVE dialog — is plan-gated: on a
              cold-only tier a live dispatch is refused with{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">402 LIVE_MIGRATION_PLAN_REQUIRED</code>.
              A peer-to-peer{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate migrate --mode live</code>{" "}
              between your own agents never touches the control plane and is not
              tier-gated.
            </p>
          </div>
        </div>

        {/* Compatibility checking */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Compatibility checking</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Before any migration begins, SHIFT validates that the target
            machine satisfies all of the workload&apos;s requirements. This includes
            CPU architecture, available memory, GPU model and VRAM, kernel
            version, CRIU feature set, and any explicitly required vendor adapter.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate migrate demo --to gpu-west --preflight
# the same check a real migration's VALIDATE stage runs`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Compatibility check: wl_k8m2p4q7 → mach_9e4d8c2a

  ✓ CPU:        x86_64 compatible
  ✓ Memory:     64 GB available (32 GB required)
  ✓ Device policy: compatible adapter reported
  ✓ Kernel:     6.8.0 (destination ≥ source required)
  ✓ CRIU:       4.2 (3.17+ or 4.x required)
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
              { title: "Retry is safe", description: "You can retry the migration. SHIFT re-validates compatibility and starts a fresh pipeline." },
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
            After a migration completes, SHIFT reports metrics for each
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
                  ["total_state_bytes", "Total size of the state captured at the source"],
                  ["transferred_bytes", "Total bytes transferred over the network"],
                  ["deduplicated_bytes", "Bytes skipped because the destination already held the chunks"],
                  ["pre_copy_transferred_bytes", "The part of transferred_bytes moved during live pre-copy passes, while the workload kept running (the remainder is the frozen window's transfer)"],
                  ["checkpoint_duration", "Time to capture source state"],
                  ["transfer_duration", "Time to transfer state to target"],
                  ["restore_duration", "Time to restore state on target"],
                  ["downtime", "Duration the workload was actually frozen (live: the final delta only)"],
                  ["transfer_bytes_per_second", "Observed transfer throughput"],
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
            A workload can carry a failover policy: the newest root checkpoint
            is replicated to a standby agent, which holds the newest copies and
            watches the source. Automatic restore is a guarded judgment, never a
            guess from one silence: the standby acts only when the source&apos;s
            peer listener is unreachable <em>and</em> the control plane&apos;s
            presence record for it is stale (90 seconds without a heartbeat) or
            offline — <em>and</em> a control-plane ownership epoch is acquired
            and a configured fencing provider returns fenced. The default
            fencing provider is disabled, so an unconfigured deployment never
            restores automatically. An operator can always trigger the restore
            by hand.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5 mb-4">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate workload failover api --to https://standby:8443 --machine-id STANDBY_ID --keep 3
shiftgate failover                 # source-side: replication ledger
shiftgate standby list             # standby-side: duties held, watch armed
shiftgate standby trigger WORKLOAD_ID  # explicit failover restore`}</code>
            </pre>
          </div>
          <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-5">
            <p className="text-sm text-status-warning font-medium mb-3">
              Manual failover is explicitly unsafe
            </p>
            <p className="text-sm text-text-secondary leading-relaxed">
              A manual trigger probes the source first and prints a warning when
              the source answers, but it can still create two live copies if the
              source is partitioned rather than dead — there is no physical
              fencing on that path. The automatic path leans on the ownership
              epoch and fencing provider instead: lease expiry, source silence,
              or a self-fence declaration alone never authorize a restore. A
              source that returns with an old ownership epoch is rejected from
              ownership and replication operations until an operator resolves
              it. Automatic failover also requires both machines to report
              presence to a control plane; without one, the standby holds its
              replicas and waits for an operator.
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
              The trade is explicit: a half-served workload faults every
              missing page through its lazy-pages daemon. The daemon exits when
              every image page has been transferred or the process is gone —
              typically seconds — and a daemon that dies while pages are still
              unserved takes the workload with it. SHIFT never claims a lazy
              restore succeeded while pages are still unserved. Kernel and CRIU
              userfaultfd support is required (<code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">criu check --feature uffd</code>),
              and the restore is refused up front with the reason when either
              is missing — never silently downgraded. Automatic failover,
              migration restores, and control-plane-dispatched restores are
              always eager: nobody opted into the lazy trade-off there, and a
              migration&apos;s downtime metric must mean the process is fully
              on the destination.
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
