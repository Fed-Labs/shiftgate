import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Machines",
  description: "Machine registration, capabilities, heartbeats, system requirements, and GPU compatibility in SHIFTGATE.",
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
            A machine is any computer running the SHIFTGATE agent and registered
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
                  ["Kernel version", "5.15 or later (6.x recommended)"],
                  ["CRIU", "Version 3.15 or later"],
                  ["Required kernel features", "CONFIG_CHECKPOINT_RESTORE, CONFIG_MEMCG, CONFIG_NAMESPACES"],
                  ["Agent", "shiftgate-agent (installed automatically)"],
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
            <pre className="text-sm">
              <code className="text-accent">{`sudo ./bin/shift-agent --state-dir /var/lib/shift \\
  --listen unix:///run/shift/agent.sock
./bin/shiftgate doctor`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Registering machine...
  ✓ Machine ID:   mach_7f3a2b1c
  ✓ Identity:     Ed25519 key pair generated
  ✓ Capabilities: 16 vCPU, 64 GB RAM, 1× NVIDIA A100 80GB
  ✓ Kernel:       6.1.0 with CRIU-required features
  ✓ CRIU:         v4.0 healthy`}</code>
            </pre>
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

        {/* Heartbeats */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Online/offline status and heartbeats</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Machines send heartbeats to the control plane every 10 seconds. If no
            heartbeat is received for 30 seconds, the machine is marked offline.
            Offline machines cannot receive new workloads or serve as migration
            targets.
          </p>
          <div className="flex items-center gap-6 text-sm">
            <div className="flex items-center gap-2">
              <div className="h-2 w-2 rounded-full bg-status-healthy" />
              <span className="text-text-secondary">Online — heartbeat &lt; 30s ago</span>
            </div>
            <div className="flex items-center gap-2">
              <div className="h-2 w-2 rounded-full bg-status-offline" />
              <span className="text-text-secondary">Offline — no heartbeat &gt; 30s</span>
            </div>
          </div>
        </div>

        {/* GPU compatibility */}
        <div>
          <h2 className="text-xl font-semibold mb-4">GPU compatibility</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            GPU requirements are compared with the destination inventory before
            restore. GPU context state is restored only when both the workload
            and vendor driver advertise a checkpoint adapter.
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
