"use client";

import React, { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { StateObject } from "@/components/state";
import { formatBytes, cn } from "@/lib/utils";
import type { AgentAction } from "@/lib/types";
import Link from "next/link";

const actions: Array<{ key: AgentAction; label: string; states: string[]; danger?: boolean }> = [
  { key: "start", label: "START", states: ["registered", "stopped"] },
  { key: "pause", label: "PAUSE", states: ["running"] },
  { key: "resume", label: "RESUME", states: ["paused"] },
  { key: "checkpoint", label: "CHECKPOINT", states: ["running"] },
  { key: "stop", label: "STOP", states: ["running", "paused"], danger: true },
  { key: "delete", label: "DELETE", states: ["registered", "stopped", "failed"], danger: true },
];

export default function WorkloadsPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";
  const queryClient = useQueryClient();
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const workloadsQuery = useQuery({ queryKey: ["workloads", orgId], queryFn: () => api.listWorkloads(orgId), enabled: !!orgId });
  const machinesQuery = useQuery({ queryKey: ["machines", orgId], queryFn: () => api.listMachines(orgId), enabled: !!orgId });

  const reconcile = useMutation({
    mutationFn: () => api.reconcile(orgId),
    onSuccess: (r) => setMessage(`Reconciled ${r.reconciled_workloads} workload(s)`),
    onError: (e: unknown) => setMessage(e instanceof Error ? e.message : "Reconciliation failed"),
  });

  const command = useMutation({
    mutationFn: (input: { machineId: string; workloadId: string; action: AgentAction }) =>
      api.agentCommand(orgId, input.machineId, { action: input.action as never, workload_id: input.workloadId, timeout_seconds: 30 }),
    onSuccess: (_r, input) => {
      setMessage(`${input.action} command completed`);
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
    },
    onError: (e: unknown) => setMessage(e instanceof Error ? e.message : "Agent command failed"),
    onSettled: () => setBusyKey(null),
  });

  const run = (machineId: string, workloadId: string, action: AgentAction) => {
    if (!machineId) return;
    setBusyKey(`${workloadId}:${action}`);
    setMessage(null);
    command.mutate({ machineId, workloadId, action });
  };

  const workloads = workloadsQuery.data || [];
  const machineMap = new Map((machinesQuery.data || []).map((m) => [m.machine_id, m]));

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      <div className="flex items-baseline justify-between mb-10">
        <div>
          <span className="tlabel block mb-2">fleet</span>
          <h1 className="font-display text-3xl font-medium text-text">Workloads</h1>
        </div>
        <button
          onClick={() => reconcile.mutate()}
          disabled={reconcile.isPending || command.isPending}
          className="font-mono text-[10px] tracking-[0.14em] text-text-muted border border-border px-3 py-2 hover:text-text transition-colors disabled:opacity-40"
        >
          {reconcile.isPending ? "SYNCING…" : "SYNC AGENTS"}
        </button>
      </div>

      {(command.isPending || message) && (
        <p className={cn("mb-6 font-mono text-[11px]", command.isPending ? "text-accent" : message?.includes("completed") || message?.includes("Reconciled") ? "text-status-healthy" : "text-status-error")}>
          {command.isPending ? "DISPATCHING AGENT COMMAND…" : message}
        </p>
      )}

      {workloadsQuery.isLoading ? (
        <p className="font-mono text-[11px] text-text-muted">READING WORKLOADS…</p>
      ) : workloads.length === 0 ? (
        <div className="border border-border-subtle py-20 text-center">
          <p className="font-mono text-[12px] text-text-secondary mb-2">NO REPORTED WORKLOADS</p>
          <p className="text-text-muted text-sm">Workloads appear after an online agent reports them.</p>
        </div>
      ) : (
        <div className="border-t border-border-subtle">
          {workloads.map((w) => {
            const state = w.status.state ?? (typeof w.status.status === "string" ? w.status.status : undefined);
            const status = String(state || "unknown").toLowerCase();
            const cpu = typeof w.status.cpu_percent === "number" ? w.status.cpu_percent : undefined;
            const memory = typeof w.status.memory_bytes === "number" ? w.status.memory_bytes : undefined;
            const machineName = w.machine_id ? machineMap.get(w.machine_id)?.name : undefined;

            return (
              <div key={w.id} className="grid grid-cols-[1fr_auto] md:grid-cols-[1.6fr_1fr_1fr_auto] gap-4 items-center py-4 border-b border-border-subtle">
                {/* identity */}
                <Link href={`/app/workloads/${w.id}`} className="flex items-center gap-4 min-w-0 group">
                  <StateObject layers={{ process: 0.6, memory: 0.5, filesystem: 0.3, network: 0.2, device: 0.3, environment: 0.1 }} active={status === "running"} size="sm" className="w-10 shrink-0" />
                  <div className="min-w-0">
                    <p className="font-mono text-[13px] text-text truncate group-hover:text-accent transition-colors">{w.name}</p>
                    <p className="font-mono text-[10px] text-text-muted">{machineName ? machineName.toUpperCase() : "UNASSIGNED"}</p>
                  </div>
                </Link>

                {/* readouts */}
                <div className="hidden md:block font-mono text-[11px] text-text-muted tabular-nums">
                  {cpu !== undefined ? `CPU ${cpu}%` : "CPU —"}
                  <span className="mx-2 text-text-faint">/</span>
                  {memory !== undefined ? formatBytes(memory) : "MEM —"}
                </div>

                {/* status */}
                <span className={cn("hidden md:block font-mono text-[10px] tracking-[0.14em]", status === "running" ? "text-status-healthy" : status === "stopped" ? "text-text-muted" : "text-status-warning")}>
                  {status.toUpperCase()}
                </span>

                {/* actions */}
                <div className="flex items-center gap-1.5">
                  <Link href={`/app/workloads/${w.id}`} className="font-mono text-[10px] tracking-[0.12em] text-bg bg-accent px-3 py-1.5 hover:bg-accent-dim transition-colors">
                    MOVE
                  </Link>
                  {actions.filter((a) => a.states.includes(status)).map((a) => (
                    <button
                      key={a.key}
                      disabled={!w.machine_id || command.isPending}
                      onClick={() => run(w.machine_id!, w.id, a.key)}
                      className={cn(
                        "font-mono text-[10px] tracking-[0.1em] px-2 py-1.5 border transition-colors disabled:opacity-30",
                        a.danger ? "text-status-error border-status-error/30 hover:bg-status-error/10" : "text-text-muted border-border hover:text-text"
                      )}
                    >
                      {busyKey === `${w.id}:${a.key}` ? "…" : a.label}
                    </button>
                  ))}
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
