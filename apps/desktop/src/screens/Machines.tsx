// Machines — the registered fleet from the control plane, each machine's
// hardware and SHIFT agent state, plus this machine's live inventory from the
// local agent. Registered machines are what migrations target; the local
// inventory is what compatibility is judged against.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Laptop } from "lucide-react";
import { backend, errorMessage, formatBytes, formatRelative } from "@/lib/backend";
import { control, ControlMachine } from "@/lib/control";
import { useAuth } from "@/lib/store";
import { evaluateSupportPolicy } from "@/lib/support-policy";
import { Badge, Card, Dot, EmptyState, KeyValue, SectionLabel, Spinner } from "@/components/ui";

interface MachineCapabilitiesLike {
  hostname?: string;
  os?: string;
  distribution?: string;
  architecture?: string;
  kernel?: string;
  cpus?: number;
  memory_bytes?: number;
  storage?: Array<{ mountpoint: string; filesystem: string; total_bytes: number; available_bytes: number }>;
  gpus?: Array<{ vendor: string; model: string; checkpoint_restore: boolean }>;
  criu?: { healthy?: boolean; version?: string };
  agent_version?: string;
}

export function MachinesScreen() {
  const { organization, tokens } = useAuth();
  const [selected, setSelected] = useState<string | null>(null);

  // The local inventory is always available (agent-only usage has no control
  // plane) and must stay fresh next to the polled fleet list.
  const localMachine = useQuery({
    queryKey: ["agent-machine"],
    queryFn: backend.machine,
    refetchInterval: 15_000,
    retry: false,
  });
  const machines = useQuery({
    queryKey: ["control-machines", organization?.id],
    queryFn: () => control.machines(organization!.id),
    enabled: !!organization && !!tokens,
    refetchInterval: 15_000,
    retry: false,
  });

  const registered = machines.data ?? [];

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold">Machines</h1>
        <p className="text-[13px] text-text-muted mt-0.5">
          {organization && tokens
            ? `${registered.length} registered · migrations target a machine's agent endpoint`
            : "migrations target a machine's agent endpoint"}
        </p>
      </div>

      {/* Registered fleet — needs the control plane. Scoped here so the local
          machine below is never hidden behind a sign-in wall. */}
      {!organization || !tokens ? (
        <EmptyState
          title="Fleet view needs the control plane"
          description="Sign in and select an organization in Settings to see registered machines across the fleet. The local machine is always shown below."
        />
      ) : machines.isError ? (
        <EmptyState
          title="Could not load registered machines"
          description={errorMessage(machines.error) ?? "The control plane did not answer."}
        />
      ) : machines.isLoading ? (
        <div className="flex items-center justify-center py-12">
          <Spinner className="w-5 h-5" />
        </div>
      ) : registered.length === 0 ? (
        <EmptyState
          title="No machines registered"
          description="Register machines with shiftgate fleet machines register --machine-id ID --name NAME --agent-url URL, or let the agent's presence reporter do it (SHIFT_CONTROL_* settings). This machine still appears below."
        />
      ) : (
        <div className="space-y-2">
          {registered.map((machine) => (
            <MachineCard
              key={machine.id}
              machine={machine}
              localMachineID={localMachine.data?.machine_id}
              expanded={selected === machine.id}
              onToggle={() => setSelected(selected === machine.id ? null : machine.id)}
            />
          ))}
        </div>
      )}

      {/* Local machine inventory */}
      {localMachine.isLoading ? (
        <div className="flex items-center justify-center py-12">
          <Spinner className="w-5 h-5" />
        </div>
      ) : localMachine.isError ? (
        <Card className="p-4">
          <SectionLabel>This machine · live inventory</SectionLabel>
          <p className="text-[13px] text-status-error">
            Cannot read the local inventory: {errorMessage(localMachine.error) ?? "the agent is unreachable"}.
            {" "}Start the agent (shift-agent) and confirm its socket path in Settings.
          </p>
        </Card>
      ) : (
        localMachine.data && (
          <>
            <Card className="p-4">
              <SectionLabel>This machine · live inventory</SectionLabel>
              <MachineDetail capabilities={localMachine.data as unknown as MachineCapabilitiesLike} />
            </Card>
            <SupportPolicyCard capabilities={localMachine.data as unknown as MachineCapabilitiesLike} />
          </>
        )
      )}
    </div>
  );
}

/**
 * The support-matrix policy decision for this machine: whether unattended
 * (automatic) migration and failover may run without an operator. Mirrors
 * `shiftgate verify`; the decision vocabulary and rules come from the shipped
 * support matrix (docs/support-matrix.json).
 */
function SupportPolicyCard({ capabilities }: { capabilities: MachineCapabilitiesLike }) {
  const decision = evaluateSupportPolicy({
    os: capabilities.os ?? "",
    architecture: capabilities.architecture ?? "",
    distribution: capabilities.distribution ?? "",
    kernel: capabilities.kernel ?? "",
    criuVersion: capabilities.criu?.version ?? "",
    criuCheck: capabilities.criu?.healthy ? "passed" : "failed (agent reports CRIU unavailable or unhealthy)",
  });
  const tone = decision.allowAutomatic ? "healthy" : decision.status === "untested" ? "warning" : "error";

  return (
    <Card className="p-4">
      <SectionLabel>Support policy · unattended operations</SectionLabel>
      <div className="flex items-center gap-2 mt-1.5">
        <Badge tone={tone}>{decision.action.replace(/_/g, " ")}</Badge>
        <span className="text-[12px] text-text-secondary">
          {decision.allowAutomatic
            ? "Automatic migration and failover are permitted on this platform."
            : "Automatic migration and failover are not permitted on this platform without an operator."}
        </span>
      </div>
      <div className="mt-3">
        <KeyValue label="Matrix" value={`v${decision.matrixVersion} · ${decision.status}${decision.platformID ? ` · ${decision.platformID}` : ""}`} />
      </div>
      <p className="text-[12px] text-text-muted leading-relaxed mt-2">{decision.reason}</p>
      <p className="text-[11px] text-text-faint leading-relaxed mt-2">
        Advisory preview of <span className="readout">shiftgate verify</span>, which runs the same
        matrix rules against live evidence. A passing <span className="readout">criu check</span> is
        evidence, not certification — unknown and untested combinations stay operator-gated until
        certified with <span className="readout">scripts/verify-support-matrix.sh</span>.
      </p>
    </Card>
  );
}

function MachineCard({
  machine,
  localMachineID,
  expanded,
  onToggle,
}: {
  machine: ControlMachine;
  localMachineID?: string;
  expanded: boolean;
  onToggle: () => void;
}) {
  const caps = (machine.capabilities ?? {}) as MachineCapabilitiesLike;
  const isLocal = machine.machine_id === localMachineID;
  const status = machine.status;
  const gpus = caps.gpus ?? [];

  return (
    <Card className="overflow-hidden">
      <button
        onClick={onToggle}
        className="w-full flex items-center gap-4 px-4 py-3 text-left hover:bg-bg-hover transition-colors"
      >
        <Laptop className="w-4 h-4 text-text-muted shrink-0" />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="text-[13px] font-medium truncate">{machine.name}</span>
            {isLocal && <Badge tone="accent">this machine</Badge>}
          </div>
          <div className="text-[11px] text-text-faint truncate">
            {caps.distribution || caps.os || "unknown OS"} · {caps.architecture || "?"} ·{" "}
            {caps.cpus ? `${caps.cpus} cores` : "?"} · {formatBytes(caps.memory_bytes)} ·{" "}
            {gpus.length > 0 ? `${gpus.length}× GPU` : "no GPU"}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <span className="text-[11px] text-text-muted">{formatRelative(machine.last_seen_at)}</span>
          <Badge tone={status === "online" ? "healthy" : status === "disabled" ? "offline" : "warning"}>
            <Dot tone={status === "online" ? "healthy" : status === "disabled" ? "offline" : "warning"} />
            {status}
          </Badge>
        </div>
      </button>
      {expanded && (
        <div className="border-t border-border-subtle px-4 py-3 bg-bg-elevated">
          <KeyValue label="Machine ID" value={machine.machine_id} />
          <KeyValue label="Agent endpoint" value={machine.agent_url} />
          <KeyValue label="SHIFT agent" value={caps.agent_version || "—"} />
          <KeyValue label="CRIU" value={caps.criu?.healthy ? caps.criu.version || "healthy" : "unavailable"} />
          <KeyValue
            label="Storage"
            value={
              (caps.storage ?? []).length > 0
                ? caps.storage!.map((s) => `${s.mountpoint} ${formatBytes(s.available_bytes)}/${formatBytes(s.total_bytes)}`).join(" · ")
                : "—"
            }
          />
          <KeyValue
            label="GPU"
            value={gpus.length > 0 ? gpus.map((g) => `${g.vendor} ${g.model}${g.checkpoint_restore ? " (checkpointable)" : ""}`).join(", ") : "none"}
          />
          <KeyValue label="Registered" value={new Date(machine.created_at).toLocaleString()} />
          <KeyValue label="Compatibility" value={compatibilitySummary(caps)} />
        </div>
      )}
    </Card>
  );
}

function compatibilitySummary(caps: MachineCapabilitiesLike): string {
  const notes: string[] = [];
  if (caps.criu?.healthy) notes.push("checkpoint/restore ready");
  else notes.push("no CRIU — cold file migration only");
  if ((caps.gpus ?? []).some((g) => g.checkpoint_restore)) notes.push("GPU state migration supported");
  return notes.join(" · ");
}

export function MachineDetail({ capabilities }: { capabilities: MachineCapabilitiesLike }) {
  return (
    <div className="grid md:grid-cols-2 gap-x-8">
      <div>
        <KeyValue label="Hostname" value={capabilities.hostname ?? "—"} />
        <KeyValue label="OS" value={`${capabilities.distribution ?? capabilities.os ?? "—"} (${capabilities.kernel ?? "?"})`} />
        <KeyValue label="Architecture" value={capabilities.architecture ?? "—"} />
        <KeyValue label="CPU" value={capabilities.cpus ? `${capabilities.cpus} cores` : "—"} />
      </div>
      <div>
        <KeyValue label="Memory" value={formatBytes(capabilities.memory_bytes)} />
        <KeyValue
          label="GPU"
          value={
            (capabilities.gpus ?? []).length > 0
              ? capabilities.gpus!.map((g) => `${g.vendor} ${g.model}`).join(", ")
              : "none"
          }
        />
        <KeyValue label="SHIFT agent" value={capabilities.agent_version ?? "—"} />
        <KeyValue label="CRIU" value={capabilities.criu?.healthy ? capabilities.criu.version || "healthy" : "unavailable"} />
      </div>
    </div>
  );
}
