import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Machines",
  description: "Machine registration, capabilities, heartbeats, system requirements, and GPU compatibility in SHIFT.",
};

export default function MachinesPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Infrastructure
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Machines
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            A machine is any computer running the SHIFT agent and registered
            with your organization. Machines are the source and destination for
            workload migrations.
          </p>
        </div>

        {/* System requirements */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">System requirements</h2>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["Operating system", "Linux (x86_64)"],
                  ["Kernel", "Checkpoint/restore capable (criu check must pass); certified lines per the support matrix"],
                  ["CRIU", "3.17+ or 4.x; criu check must pass"],
                  ["Required kernel features", "CONFIG_CHECKPOINT_RESTORE, CONFIG_MEMCG, CONFIG_NAMESPACES"],
                  ["Agent", "shift-agent (installed automatically)"],
                  ["Network", "Outbound HTTPS (443) to control plane"],
                ].map(([key, value]) => (
                  <tr key={key} className="border-border-subtle">
                    <td className="px-5 py-3 text-text-muted font-medium w-1/3">{key}</td>
                    <td className="px-5 py-3 text-text-secondary font-mono text-xs">{value}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="text-sm text-text-muted leading-relaxed mt-4">
            Only Ubuntu 24.04 (kernel 6.8–6.14, CRIU 4.x) is certified by the
            release CI lanes today; other lines are untested or unknown and are
            treated accordingly. See{" "}
            <a href="/docs/platforms" className="text-text hover:text-accent transition-colors">Platforms and compatibility</a>{" "}
            for the full matrix and what each status permits.
          </p>
        </div>

        {/* Registering a machine */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Starting a machine agent</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            When an agent starts, it generates a cryptographic Ed25519 identity,
            collects hardware capabilities through its inventory, and exposes
            them to authorized peers and the control plane. The identity lives
            under the configured state directory and is created automatically.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-sm text-text-secondary leading-relaxed mb-4">
              The installer installs the agent to <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">/usr/local/bin/shift-agent</code> and enables it as a systemd service with a local-only configuration; peers are added later by editing <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">/etc/shift/agent.json</code>.
            </p>
            <pre className="text-sm">
              <code className="text-accent">{`systemctl status shift-agent
shiftgate doctor`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`OK    platform             linux/amd64
OK    privileges           agent effective uid 0
OK    criu                 Version: 4.2
OK    cgroup_v2            cgroup v2 workload isolation
OK    archive              GNU tar with metadata support`}</code>
            </pre>
            <p className="text-sm text-text-muted leading-relaxed mt-4">
              From a repository checkout instead: <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">make build &amp;&amp; sudo ./install.sh</code>, or run the built agent directly with <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">sudo ./bin/shift-agent --state-dir /var/lib/shift --listen unix:///run/shift/agent.sock</code>.
            </p>
          </div>
        </div>

        {/* Capabilities and inventory */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Capabilities and inventory</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Each machine reports a capability profile to the control plane. This
            profile is used during migration to determine whether a target machine
            is compatible with a workload&apos;s requirements.
          </p>
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {[
              { label: "CPU", detail: "Architecture, core count, model" },
              { label: "Memory", detail: "Total RAM, NUMA topology" },
              { label: "GPU", detail: "Model, VRAM, CUDA version, driver" },
              { label: "Kernel", detail: "Version, required config flags" },
              { label: "CRIU", detail: "Version, feature set" },
              { label: "Storage", detail: "Available space, filesystem type" },
            ].map((cap) => (
              <div key={cap.label} className="rounded-lg border border-border bg-bg-elevated p-4">
                <p className="text-sm font-semibold mb-1">{cap.label}</p>
                <p className="text-xs text-text-muted">{cap.detail}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Registering with the control plane */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Registering with the control plane</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Registration and capability queries always talk to the control
            plane — the platform endpoint by default — so they need{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate login</code>{" "}
            first and never silently no-op against the local agent. Machines
            can also register themselves through the agent&apos;s presence
            reporter (<code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">SHIFT_CONTROL_*</code>{" "}
            environment). With <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">--control-url</code>{" "}
            set, <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate machines</code>{" "}
            shows the fleet view instead of the local machine.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate machines register \\
  --machine-id workstation --name Workstation --agent-url https://workstation:8443

# fleet query through the control plane's capability projection
shiftgate machines capability cuda_version --kind number --minimum 12`}</code>
            </pre>
          </div>
        </div>

        {/* Heartbeats */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Online/offline status and heartbeats</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Agents report presence to the control plane on a 30-second default
            interval. Presence is derived server-side: a machine that
            self-reports &quot;online&quot; but stops heartbeating reads offline
            after two minutes in the fleet view — a dead host is never shown as
            live. The automatic-failover path uses a tighter judgment: it treats
            the source as gone only when its peer listener is unreachable
            <em>and</em> its control-plane presence is stale (90 seconds without
            a heartbeat) or marked offline.
          </p>
          <div className="flex items-center gap-6 text-sm flex-wrap">
            <div className="flex items-center gap-2">
              <div className="h-2 w-2 rounded-full bg-status-healthy" />
              <span className="text-text-secondary">Online — heartbeat within the presence window</span>
            </div>
            <div className="flex items-center gap-2">
              <div className="h-2 w-2 rounded-full bg-status-offline" />
              <span className="text-text-secondary">Offline — no heartbeat for over 2 minutes</span>
            </div>
          </div>
        </div>

        {/* GPU compatibility */}
        <div>
          <h2 className="text-xl font-semibold mb-4">GPU compatibility</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            GPU requirements are compared with the destination inventory before
            restore. GPU context state is restored only when both the workload
            and vendor driver advertise a checkpoint adapter. The GPU support
            matrix is honest about evidence: NVIDIA/CUDA is <em>unknown</em> —
            certification requires an external hardware runner and no local
            hardware claim is made — and AMD/ROCm is <em>unsupported</em> (ROCm
            process checkpoint/restore is not available in the current
            implementation).
          </p>
          <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-5">
            <p className="text-sm text-status-warning font-medium mb-2">
              Adapter requirements
            </p>
            <ul className="text-sm text-text-secondary space-y-1.5">
              <li>• The destination must satisfy the workload&apos;s device policy</li>
              <li>• Device state is not inferred from generic /dev files</li>
              <li>• Vendor checkpoint and restore support is required for GPU context state</li>
              <li>• Without an adapter, applications must reconnect or reinitialize their device state</li>
            </ul>
          </div>
        </div>
      </Container>
    </Section>
  );
}
