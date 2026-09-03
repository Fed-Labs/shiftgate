import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Concepts",
  description: "Core concepts behind SHIFTGATE — workloads, machines, checkpoints, migrations, and state classes.",
};

const CONCEPTS = [
  {
    id: "workloads",
    title: "Workloads",
    subtitle: "The unit of portable computation",
    body: `A workload is the fundamental unit SHIFTGATE manages. It wraps a running process — its command, file system paths, resource requirements, and device policies — into a portable entity that can be moved between machines without the process knowing.

Workloads are not containers or VMs. They are process-level abstractions. SHIFTGATE captures the execution state CRIU and the configured device adapters can represent, then validates it before restoring on a different machine.`,
    diagram: (
      <div className="flex items-center gap-3 flex-wrap">
        <div className="rounded-md border border-border bg-bg-surface px-4 py-3 text-sm font-mono">
          process
        </div>
        <span className="text-text-muted">+</span>
        <div className="rounded-md border border-border bg-bg-surface px-4 py-3 text-sm font-mono">
          resources
        </div>
        <span className="text-text-muted">+</span>
        <div className="rounded-md border border-border bg-bg-surface px-4 py-3 text-sm font-mono">
          policy
        </div>
        <span className="text-text-muted">=</span>
        <div className="rounded-md border border-accent/30 bg-accent/5 px-4 py-3 text-sm font-mono text-accent">
          workload
        </div>
      </div>
    ),
  },
  {
    id: "machines",
    title: "Machines",
    subtitle: "Registered computers running the SHIFTGATE agent",
    body: `A machine is any computer that runs the SHIFTGATE agent and has been registered with your organization. Machines report their capabilities — CPU, memory, GPU model, kernel version — and send periodic heartbeats to the control plane.

Machines are the source and destination for migrations. SHIFTGATE uses machine capabilities to determine compatibility before attempting a migration.`,
    diagram: (
      <div className="grid grid-cols-3 gap-3">
        {["mach_a1b2c3", "mach_d4e5f6", "mach_g7h8i9"].map((id, i) => (
          <div key={id} className="rounded-md border border-border bg-bg-surface p-3">
            <div className="flex items-center gap-2 mb-2">
              <div className={`h-2 w-2 rounded-full ${i < 2 ? "bg-status-healthy" : "bg-status-offline"}`} />
              <span className="text-xs font-mono text-text-muted">{id}</span>
            </div>
            <div className="text-xs text-text-secondary space-y-0.5">
              <div>16 vCPU · 64 GB</div>
              <div>{i === 0 ? "A100 80GB" : i === 1 ? "A100 80GB" : "H100"}</div>
            </div>
          </div>
        ))}
      </div>
    ),
  },
  {
    id: "checkpoints",
    title: "Checkpoints",
    subtitle: "Captured execution state",
    body: `A checkpoint is a snapshot of a workload's execution state at a point in time. It includes the CRIU process image, explicitly scoped filesystem data, and a state inventory that records network and device adapters.

SHIFTGATE supports two types of checkpoints:

• Full checkpoints capture the entire state independently. Larger, but self-contained.
• Incremental checkpoints capture only the pages that changed since the last checkpoint. Smaller and faster, but depend on the parent checkpoint.`,
    diagram: (
      <div className="flex items-center gap-2 flex-wrap">
        <div className="rounded-md border border-border bg-bg-surface px-3 py-2 text-xs font-mono">
          t₀ full
        </div>
        <div className="h-px w-4 bg-border" />
        <div className="rounded-md border border-border bg-bg-surface px-3 py-2 text-xs font-mono">
          t₁ incr
        </div>
        <div className="h-px w-4 bg-border" />
        <div className="rounded-md border border-border bg-bg-surface px-3 py-2 text-xs font-mono">
          t₂ incr
        </div>
        <div className="h-px w-4 bg-border" />
        <div className="rounded-md border border-accent/30 bg-accent/5 px-3 py-2 text-xs font-mono text-accent">
          t₃ full
        </div>
      </div>
    ),
  },
  {
    id: "migrations",
    title: "Migrations",
    subtitle: "Moving a workload between machines",
    body: `A migration is the process of transferring a workload from one machine to another. The pipeline has distinct stages: checkpoint the source, transfer state, restore on the target, validate, and switch.

SHIFTGATE supports two migration modes:

• Cold migration: the workload is paused, checkpointed, transferred, and restored. Simple and reliable, but the process is interrupted.
• Live migration: SHIFTGATE performs a CRIU pre-copy before the authoritative final checkpoint. The final checkpoint still has a stop phase, and network or device state follows its adapter.`,
    diagram: (
      <div className="flex items-center gap-1.5 flex-wrap text-xs font-mono">
        {["snapshot", "transfer", "restore", "validate", "switch"].map((stage, i) => (
          <div key={stage} className="flex items-center gap-1.5">
            <div className={`rounded border px-2.5 py-1.5 ${i === 4 ? "border-accent/30 bg-accent/5 text-accent" : "border-border bg-bg-surface text-text-secondary"}`}>
              {stage}
            </div>
            {i < 4 && <span className="text-text-muted">→</span>}
          </div>
        ))}
      </div>
    ),
  },
  {
    id: "state-classes",
    title: "State Classes",
    subtitle: "Portable, machine-specific, and external",
    body: `Every piece of workload state belongs to one of three classes:

• Portable state can move freely between machines. This includes process memory, CPU registers, and open file contents. Most application state is portable.

• Machine-specific state is tied to hardware. GPU memory, NUMA topology, and device-specific mappings fall into this category. SHIFTGATE checks compatibility and invokes a vendor adapter when one is available.

• External state lives outside the workload. Database connections, network sockets to external services, and shared file system mounts are reconnected or drained according to workload policy.

Understanding state classes helps you predict which workloads migrate cleanly and which need configuration adjustments.`,
    diagram: (
      <div className="grid grid-cols-3 gap-3">
        <div className="rounded-md border border-accent/20 bg-accent/5 p-3">
          <div className="text-xs font-semibold text-accent mb-1">Portable</div>
          <div className="text-xs text-text-secondary">Memory, registers, files</div>
        </div>
        <div className="rounded-md border border-status-warning/20 bg-status-warning/5 p-3">
          <div className="text-xs font-semibold text-status-warning mb-1">Machine-specific</div>
          <div className="text-xs text-text-secondary">GPU, NUMA, devices</div>
        </div>
        <div className="rounded-md border border-status-info/20 bg-status-info/5 p-3">
          <div className="text-xs font-semibold text-status-info mb-1">External</div>
          <div className="text-xs text-text-secondary">DB, network, mounts</div>
        </div>
      </div>
    ),
  },
];

export default function ConceptsPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Foundation
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Core concepts
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            SHIFTGATE is built around a small number of primitives.
            Understanding these concepts makes everything else click.
          </p>
        </div>

        <div className="space-y-16">
          {CONCEPTS.map((concept) => (
            <div key={concept.id} id={concept.id} className="scroll-mt-20">
              <div className="mb-6">
                <h2 className="text-xl font-semibold mb-1">{concept.title}</h2>
                <p className="text-sm text-text-muted">{concept.subtitle}</p>
              </div>

              <div className="prose-invert max-w-none mb-6">
                {concept.body.split("\n\n").map((para, i) => (
                  <p key={i} className="text-text-secondary text-sm leading-relaxed mb-3 last:mb-0">
                    {para}
                  </p>
                ))}
              </div>

              <div className="rounded-lg border border-border-subtle bg-bg-elevated/50 p-4">
                {concept.diagram}
              </div>
            </div>
          ))}
        </div>
      </Container>
    </Section>
  );
}
