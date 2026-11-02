import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Operations",
  description: "Running SHIFT in production — offline chunk garbage collection, agent updates, retention windows, and entitlement limits.",
};

export default function OperationsPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Operations
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Operating SHIFT
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            The day-2 surface: reclaiming chunk-store space, updating agents
            safely, retention windows, and the limits your plan enforces.
          </p>
        </div>

        {/* Garbage collection */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Garbage collecting checkpoint chunks</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Online sweeping under a running agent is deliberately disabled —
            pruning live state while checkpoints are taken and restored is
            unsafe. <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate gc</code>{" "}
            therefore plans the sweep and prints the exact offline procedure
            instead of failing with a bare error:
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`sudo systemctl stop shift-agent
sudo shift-gc --state-dir /var/lib/shift --grace 24h --dry-run        # plan only
sudo shift-gc --state-dir /var/lib/shift --grace 24h --dry-run=false  # delete orphans past the grace period
sudo systemctl start shift-agent`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <p className="text-sm text-text-secondary leading-relaxed">
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shift-gc</code>{" "}
              ships in the release tarball and is installed by both installers.{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--grace</code>{" "}
              (default 24h) is the minimum age an orphaned chunk must reach
              before deletion — the sweeper records its first observation of an
              orphan and only deletes once the grace period has elapsed, so a
              chunk orphaned by a running checkpoint or restore is never swept
              in the same breath.{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--dry-run</code>{" "}
              (the default) prints the plan and changes nothing.
            </p>
          </div>
          <div className="mt-4 rounded-lg border border-status-warning/20 bg-status-warning/5 p-5">
            <p className="text-sm text-status-warning font-medium mb-2">
              What deletion does and does not touch
            </p>
            <ul className="text-sm text-text-secondary space-y-1.5">
              <li>• Deletes only content-addressed chunks no retained checkpoint manifest references</li>
              <li>• Never touches manifests, checkpoint directories, or workload keys</li>
              <li>• Never touches mirrored cloud objects — cloud-object cleanup is operator-side</li>
            </ul>
          </div>
        </div>

        {/* Updates */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Agent updates</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            The <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate update</code>{" "}
            subcommands manage this machine&apos;s agent through the local API.
            Updates are off until the machine is configured with a release feed
            and the signing keys it trusts.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Command</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">What it does</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["shiftgate update status", "Running and pending versions, available and refused releases, preserved backups, blocked versions, and history."],
                  ["shiftgate update check", "Consult the release feed immediately; every release is evaluated with the reason for each refusal."],
                  ["shiftgate update apply [--version V]", "Install the current selection or one named version."],
                  ["shiftgate update rollback [--backup ID] [--yes]", "Reinstall a preserved binary and block the version it undid."],
                  ["shiftgate update block VERSION [--reason TEXT]", "Keep a bad version out of the automatic loop."],
                  ["shiftgate update unblock VERSION", "Re-arm a blocked version."],
                ].map(([cmd, desc]) => (
                  <tr key={cmd}>
                    <td className="px-5 py-3 font-mono text-xs text-accent whitespace-nowrap">{cmd}</td>
                    <td className="px-5 py-3 text-xs text-text-secondary">{desc}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="text-sm text-text-muted leading-relaxed mt-4">
            A release installs only after its detached Ed25519 signatures verify
            against the configured key ring and the staged binary passes its{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--version</code>{" "}
            self-check. The previous binaries are preserved (three by default)
            so rollback works without network access. An install that would
            interrupt a migration, restore, fork, or incoming transfer exits
            with code 6 and is retried later — the gate is checked before the
            download and again immediately before the swap.
          </p>
        </div>

        {/* Retention */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Retention windows</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Each organization owns three retention windows in days, managed
            with{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate fleet retention [--audit-days N] [--checkpoint-days N] [--deleted-storage-days N]</code>{" "}
            (admin role). An omitted flag leaves its window unchanged and{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">0</code>{" "}
            is the deliberate keep-forever policy. Every change is audited.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Window</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Default</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Enforcement</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["audit_retention_days", "365", "Audit rows past the window are deleted."],
                  ["checkpoint_retention_days", "90", "Checkpoints are marked deleted: the row and its lineage survive so history stays answerable, but the checkpoint is no longer restorable."],
                  ["deleted_storage_retention_days", "30", "Storage objects already soft-deleted are purged after their grace period."],
                ].map(([w, d, e]) => (
                  <tr key={w}>
                    <td className="px-5 py-3 font-mono text-xs text-accent">{w}</td>
                    <td className="px-5 py-3 font-mono text-xs text-text-secondary">{d}</td>
                    <td className="px-5 py-3 text-xs text-text-secondary">{e}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="text-sm text-text-muted leading-relaxed mt-4">
            The control plane enforces the windows on a background sweep
            (configurable via{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">retention_sweep_interval</code>,
            default hourly). The same sweep anonymizes SCIM-deactivated accounts
            after a 30-day grace window. Retention windows are plan-capped: Free
            and Pro up to 365 days, Business and Enterprise up to 36500 — a
            larger request is refused with{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">402 RETENTION_PLAN_LIMIT</code>.
          </p>
        </div>

        {/* Entitlements */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Entitlements and plan limits</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            The public plan catalog is the single source of plan truth: the
            pricing page renders it and entitlement checks enforce it. Current
            limits:
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Plan</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Machines</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Hosted storage</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Live migration</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["Free", "2", "10 GiB", "cold-only"],
                  ["Pro", "10", "120 GiB", "included"],
                  ["Business", "no cap", "500 GiB + 100 GiB per seat (2 TiB cap)", "included"],
                  ["Enterprise (custom)", "no cap", "custom", "included"],
                ].map(([plan, m, s, l]) => (
                  <tr key={plan}>
                    <td className="px-5 py-3 font-mono text-xs text-accent">{plan}</td>
                    <td className="px-5 py-3 font-mono text-xs text-text-secondary">{m}</td>
                    <td className="px-5 py-3 font-mono text-xs text-text-secondary">{s}</td>
                    <td className="px-5 py-3 font-mono text-xs text-text-secondary">{l}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <ul className="text-sm text-text-secondary space-y-1.5 mt-4">
            <li>
              • Live migration is a paid plan feature on every path, including
              peer-to-peer moves. Live mode dispatched through the control
              plane — the migration job route or the dashboard&apos;s MOVE dialog —
              and a peer-to-peer{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate migrate --mode live</code>{" "}
              between your own agents are both refused on a cold-only plan with{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">LIVE_MIGRATION_PLAN_REQUIRED</code>;
              the gate is enforced in the agent and CLI, not only at the API.
            </li>
            <li>
              • Business is a flat plan with 5 seats included and $19 per extra
              seat per month; checkout takes a seat count, and
              machine-registration limits come from the entitlement the
              subscription webhook applies — always the catalog&apos;s numbers,
              never the subscription metadata&apos;s.
            </li>
            <li>
              • Read the live values with{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate fleet entitlement</code>{" "}
              and{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate plans</code>.
            </li>
          </ul>
        </div>

        {/* Incident handling */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Incident handling</h2>
          <p className="text-sm text-text-secondary leading-relaxed">
            Inspect <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">/health</code>,{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">/metrics</code>,{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate doctor</code>,
            migration event history, and audit events in that order. Never
            delete an agent state directory while a migration or restore
            transaction is active — a source marked preserved is intentionally
            retained until an operator confirms destination state and cleanup.
          </p>
        </div>
      </Container>
    </Section>
  );
}
