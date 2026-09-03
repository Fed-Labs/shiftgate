import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Workloads",
  description: "Workload specs, lifecycle, resource requirements, device policies, and network policies in SHIFTGATE.",
};

const LIFECYCLE_STATES = [
  { state: "registered", description: "Spec recorded by the local agent; process is not running." },
  { state: "running", description: "Agent launched the process and adopted its process group." },
  { state: "paused", description: "Process group stopped for checkpoint or failure handling." },
  { state: "checkpointed", description: "Execution state captured. Process may be paused." },
  { state: "restoring", description: "State is being restored on a target machine." },
  { state: "stopped", description: "Process terminated. State may still exist as a checkpoint." },
];

export default function WorkloadsPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Core resource
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Workloads
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            A workload is the unit of portable computation in SHIFTGATE. It
            encapsulates a running process along with its resource requirements,
            file system paths, device policies, and network configuration.
          </p>
        </div>

        {/* Workload spec */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Workload specification</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            A workload is registered with a selected agent. Its explicit root and
            command define what runs; resource and device requirements are used
            to validate migration targets rather than schedule it onto another
            host.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <div className="border-b border-border-subtle px-5 py-2">
              <span className="text-xs font-mono text-text-muted">workload spec</span>
            </div>
            <div className="p-5">
              <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                <code>{`{
  "name": "demo",
  "command": ["/usr/bin/python3", "-m", "http.server", "8080"],
  "root_path": "/absolute/path/to/demo",
  "working_dir": "/absolute/path/to/demo",
  "resources": {
    "cpu_cores": 2,
    "memory_bytes": 2147483648,
    "storage_bytes": 104857600
  },
  "network_policy": "reconnect"
}`}</code>
              </pre>
            </div>
          </div>
        </div>

        {/* Spec fields */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Spec fields</h2>
          <div className="space-y-3">
            {[
              { field: "command", description: "Executable argument vector launched by the agent with the workload's environment." },
              { field: "root_path", description: "Required explicit directory captured as the filesystem asset; / and unrestricted home directories are rejected." },
              { field: "resources", description: "CPU, memory, storage, and declared GPUs used by compatibility checking." },
              { field: "device_requirements", description: "GPU requirements matched against inventory; state restore still requires a vendor checkpoint adapter." },
              { field: "network_policy", description: "preserve, reconnect, or drain. Preserve requests CRIU TCP repair only when capability checks permit it." },
            ].map((item) => (
              <div key={item.field} className="rounded-lg border border-border bg-bg-elevated p-4">
                <code className="text-sm font-mono text-accent mb-1 block">{item.field}</code>
                <p className="text-sm text-text-secondary">{item.description}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Creating a workload */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Creating a workload</h2>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`shift workload create demo \\
  --path "$PWD/demo" \\
  --start -- /usr/bin/python3 -m http.server 8080`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Creating workload...
  ✓ Workload ID:  wl_k8m2p4q7
  ✓ Root:         /absolute/path/to/demo
  ✓ Status:       running`}</code>
            </pre>
          </div>
        </div>

        {/* Lifecycle */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Workload lifecycle</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-6">
            A workload transitions through a persisted set of local runtime
            states. Restore and migration transitions are also recorded in
            their own encrypted records.
          </p>

          {/* Lifecycle diagram */}
          <div className="flex items-center gap-2 flex-wrap mb-6">
            {LIFECYCLE_STATES.map((s, i) => (
              <div key={s.state} className="flex items-center gap-2">
                <div className="rounded-md border border-border bg-bg-surface px-3 py-1.5 text-xs font-mono text-text-secondary">
                  {s.state}
                </div>
                {i < LIFECYCLE_STATES.length - 1 && (
                  <span className="text-text-muted text-xs">→</span>
                )}
              </div>
            ))}
          </div>

          <div className="space-y-2">
            {LIFECYCLE_STATES.map((s) => (
              <div key={s.state} className="flex items-start gap-4 rounded-md border border-border-subtle bg-bg-elevated/50 px-4 py-3">
                <code className="text-xs font-mono text-accent shrink-0 w-28 pt-0.5">{s.state}</code>
                <p className="text-sm text-text-secondary">{s.description}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Resource requirements */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Resource requirements</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            SHIFTGATE compares declared resources with a destination&apos;s inventory
            before restore. It does not schedule workloads onto another host.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Resource</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Format</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Example</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["cpu_cores", "Number", "2"],
                  ["memory_bytes", "Integer", "2147483648"],
                  ["storage_bytes", "Integer", "1073741824"],
                  ["gpus", "Array of GPU requirements", "[]"],
                ].map(([resource, format, example]) => (
                  <tr key={resource}>
                    <td className="px-5 py-3 font-mono text-xs text-accent">{resource}</td>
                    <td className="px-5 py-3 text-text-secondary text-xs">{format}</td>
                    <td className="px-5 py-3 font-mono text-xs text-text-muted">{example}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </Container>
    </Section>
  );
}
