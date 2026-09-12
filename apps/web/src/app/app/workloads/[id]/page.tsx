"use client";

import React from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { StateObject, ResourceReadout } from "@/components/state";
import { MigrateDialog } from "@/components/migrate-dialog";
import { cn } from "@/lib/utils";
import type { AgentAction } from "@/lib/types";
import Link from "next/link";

export default function WorkloadDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = React.use(params);
  const orgId = useAuth((s) => s.organization?.id || "");
  const queryClient = useQueryClient();
  const [message, setMessage] = useState<string | null>(null);
  const [migrating, setMigrating] = useState(false);

  const workloadQuery = useQuery({ queryKey: ["workloads", orgId], queryFn: () => api.listWorkloads(orgId), enabled: !!orgId });
  const machinesQuery = useQuery({ queryKey: ["machines", orgId], queryFn: () => api.listMachines(orgId), enabled: !!orgId });

  const workload = workloadQuery.data?.find((item) => item.id === id);
  const machine = machinesQuery.data?.find((item) => item.machine_id === workload?.machine_id);

  const command = useMutation({
    mutationFn: (action: AgentAction) => {
      if (!machine) throw new Error("No reachable source agent assigned");
      return api.agentCommand(orgId, machine.machine_id, {
        action: action as never,
        workload_id: id,
        timeout_seconds: action === "checkpoint" ? 120 : 30,
      });
    },
    onSuccess: () => {
      setMessage("Agent command completed");
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
    },
    onError: (error: unknown) => setMessage(error instanceof Error ? error.message : "Agent command failed"),
  });

  if (workloadQuery.isLoading || machinesQuery.isLoading) {
    return <div className="p-10 font-mono text-[11px] text-text-muted">READING WORKLOAD…</div>;
  }

  if (!workload) {
    return (
      <div className="p-10">
        <p className="font-mono text-[12px] text-text-secondary">WORKLOAD NOT FOUND</p>
        <Link href="/app/workloads" className="font-mono text-[11px] text-accent mt-2 inline-block">← ALL WORKLOADS</Link>
      </div>
    );
  }

  const status = String(workload.status.state || "unknown").toLowerCase();
  const running = status === "running";
  const canStart = status === "registered" || status === "stopped";
  const canStop = running || status === "paused";
  const spec = workload.spec as { root_path?: string; command?: string[] };

  return (
    <div className="max-w-5xl mx-auto px-5 md:px-10 py-10">
      <div className="flex items-baseline justify-between mb-2">
        <Link href="/app/workloads" className="font-mono text-[10px] text-text-muted hover:text-text">← WORKLOADS</Link>
        <span className="font-mono text-[10px] text-text-faint">{workload.id.slice(0, 8)}</span>
      </div>

      <div className="flex items-baseline justify-between mb-10">
        <h1 className="font-display text-3xl font-medium text-text">{workload.name}</h1>
        <span className={cn("font-mono text-[11px] tracking-[0.16em]", running ? "text-status-healthy" : "text-text-muted")}>
          {status.toUpperCase()}
        </span>
      </div>

      <div className="grid md:grid-cols-[320px_1fr] gap-10">
        {/* State object + location */}
        <div>
          <StateObject
            layers={{ process: 0.7, memory: 0.6, filesystem: 0.4, network: 0.3, device: 0.4, environment: 0.2 }}
            active={running}
            size="lg"
            className="mb-4"
          />
          <div className="border-y border-border-subtle divide-y divide-border-subtle">
            <Row label="LOCATION" value={machine ? machine.name.toUpperCase() : "UNASSIGNED"} />
            <Row label="ROOT" value={spec?.root_path ?? "—"} />
            <Row label="COMMAND" value={(spec?.command ?? []).join(" ") || "—"} />
          </div>

          {/* Actions */}
          <div className="mt-6 flex flex-col gap-2">
            <button
              onClick={() => setMigrating(true)}
              disabled={!machine}
              className="block text-center font-mono text-[12px] tracking-[0.2em] text-bg bg-accent px-5 py-3 hover:bg-accent-dim transition-colors disabled:opacity-30 disabled:pointer-events-none"
            >
              MOVE →
            </button>
            <div className="grid grid-cols-3 gap-2">
              <Action onClick={() => command.mutate("checkpoint")} disabled={!machine || command.isPending || !canStop}>CHECKPOINT</Action>
              <Action onClick={() => command.mutate(canStart ? "start" : "pause")} disabled={!machine || command.isPending}>
                {canStart ? "START" : "PAUSE"}
              </Action>
              <Action onClick={() => command.mutate("stop")} disabled={!machine || command.isPending || !canStop} danger>STOP</Action>
            </div>
          </div>

          {(command.isPending || message) && (
            <p className={cn("mt-4 font-mono text-[11px]", command.isPending ? "text-accent" : message?.includes("completed") ? "text-status-healthy" : "text-status-error")}>
              {command.isPending ? "DISPATCHING…" : message}
            </p>
          )}
        </div>

        {/* Readouts */}
        <div>
          <span className="tlabel block mb-4">live readout</span>
          <div className="border-t border-border-subtle">
            <ResourceReadout label="CPU" value="42.0" unit="%" fraction={0.42} />
            <ResourceReadout label="MEMORY" value="2.40" unit="GB" fraction={0.3} />
            <ResourceReadout label="GPU" value="18.0" unit="%" fraction={0.18} />
            <ResourceReadout label="STATE" value="8.31" unit="GB" fraction={0.5} accent />
          </div>

          <div className="mt-8 border border-border-subtle p-4">
            <p className="tlabel mb-2">execution boundary</p>
            <p className="text-xs text-text-secondary leading-relaxed">
              Commands are dispatched through shift-control to the selected source agent.
              Checkpoint data remains encrypted on agents and never passes through the dashboard.
            </p>
          </div>
        </div>
      </div>

      {migrating && (
        <MigrateDialog
          workload={workload}
          machines={machinesQuery.data || []}
          onClose={() => setMigrating(false)}
        />
      )}
    </div>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="grid grid-cols-[90px_1fr] gap-4 py-2.5 items-baseline">
      <span className="tlabel">{label}</span>
      <span className="font-mono text-[11px] text-text-secondary truncate">{value}</span>
    </div>
  );
}

function Action({ children, onClick, disabled, danger }: { children: React.ReactNode; onClick: () => void; disabled?: boolean; danger?: boolean }) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      className={cn(
        "font-mono text-[10px] tracking-[0.12em] px-2 py-2 border transition-colors disabled:opacity-30 disabled:pointer-events-none",
        danger ? "text-status-error border-status-error/30 hover:bg-status-error/10" : "text-text-secondary border-border hover:text-text hover:border-border-strong"
      )}
    >
      {children}
    </button>
  );
}
