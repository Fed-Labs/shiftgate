"use client";

import React, { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { StateObject, LoadingState, ErrorState, MessageLine } from "@/components/state";
import { MigrateDialog } from "@/components/migrate-dialog";
import { canOperate, formatBytes, cn, errorMessage } from "@/lib/utils";
import {
  actionsForState,
  normalizeWorkloadState,
  timeoutForAction,
} from "@/lib/workload-status";
import { migrationIsActive } from "@/lib/migration-view";
import type { AgentAction } from "@/lib/types";
import Link from "next/link";

type Outcome = { kind: "success" | "error"; text: string } | null;

export default function WorkloadsPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";
  const role = organization?.role;
  const queryClient = useQueryClient();
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<Outcome>(null);
  const [migrating, setMigrating] = useState<string | null>(null);
  const [confirmingKey, setConfirmingKey] = useState<string | null>(null);

  const workloadsQuery = useQuery({ queryKey: ["workloads", orgId], queryFn: () => api.listWorkloads(orgId), enabled: !!orgId, refetchInterval: 15_000 });
  const machinesQuery = useQuery({ queryKey: ["machines", orgId], queryFn: () => api.listMachines(orgId), enabled: !!orgId, refetchInterval: 15_000 });
  // Lifecycle actions must not run under an in-flight migration: DELETE/STOP
  // mid-move defeats the preservation guarantee the MOVE dialog promises.
  const migrationsQuery = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
  });

  const reconcile = useMutation({
    mutationFn: () => api.reconcile(orgId),
    onSuccess: (r) => {
      setOutcome({ kind: "success", text: `Reconciled ${r.reconciled_workloads} workload(s)` });
      queryClient.invalidateQueries({ queryKey: ["checkpoints", orgId] });
    },
    onError: (e: unknown) => setOutcome({ kind: "error", text: errorMessage(e, "Reconciliation failed") }),
  });

  const command = useMutation({
    mutationFn: (input: { machineId: string; workloadId: string; action: AgentAction }) =>
      api.agentCommand(orgId, input.machineId, {
        action: input.action as never,
        workload_id: input.workloadId,
        timeout_seconds: timeoutForAction(input.action),
      }),
    onSuccess: (_r, input) => {
      setOutcome({ kind: "success", text: `${input.action} command completed` });
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
      // A checkpoint action registers a checkpoint server-side — refresh that
      // list too, or the new checkpoint is invisible until the cache ages out.
      queryClient.invalidateQueries({ queryKey: ["checkpoints", orgId] });
    },
    onError: (e: unknown) => setOutcome({ kind: "error", text: errorMessage(e, "Agent command failed") }),
    onSettled: () => {
      setBusyKey(null);
      setConfirmingKey(null);
    },
  });

  const run = (machineId: string, workloadId: string, action: AgentAction) => {
    if (!machineId) return;
    setBusyKey(`${workloadId}:${action}`);
    setOutcome(null);
    command.mutate({ machineId, workloadId, action });
  };

  const operator = canOperate(role);
  const workloads = workloadsQuery.data || [];
  const machines = machinesQuery.data || [];
  const machineMap = new Map(machines.map((m) => [m.machine_id, m]));
  const movingWorkloads = new Set(
    (migrationsQuery.data || []).filter(migrationIsActive).map((m) => m.workload_id)
  );
  // Tombstones are not fleet members: deleted workloads stay in the backend
  // record but must not clutter the list forever or inflate counts.
  const visibleWorkloads = workloads.filter((w) => normalizeWorkloadState(w.status) !== "deleted");
  const migrateTarget = migrating ? workloads.find((w) => w.id === migrating) : null;

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      <div className="flex items-baseline justify-between mb-10">
        <div>
          <span className="tlabel block mb-2">fleet</span>
          <h1 className="font-display text-3xl font-medium text-text">Workloads</h1>
        </div>
        <button
          onClick={() => reconcile.mutate()}
          disabled={!operator || reconcile.isPending || command.isPending}
          title={operator ? undefined : "Syncing agents requires the operator role"}
          className="font-mono text-[10px] tracking-[0.14em] text-text-muted border border-border px-3 py-2 hover:text-text transition-colors disabled:opacity-40"
        >
          {reconcile.isPending ? "SYNCING…" : "SYNC AGENTS"}
        </button>
      </div>

      <div className="mb-6">
        <MessageLine
          pendingLabel="DISPATCHING AGENT COMMAND…"
          message={outcome?.text ?? null}
          pending={command.isPending}
          success={outcome?.kind === "success"}
        />
      </div>

      {workloadsQuery.isLoading ? (
        <LoadingState label="READING WORKLOADS…" />
      ) : workloadsQuery.isError ? (
        <ErrorState
          title="COULD NOT LOAD WORKLOADS"
          message={errorMessage(workloadsQuery.error, "The control plane did not answer the query. This is a connection or session problem — not an empty fleet.")}
          onRetry={() => void workloadsQuery.refetch()}
        />
      ) : visibleWorkloads.length === 0 ? (
        <div className="border border-border-subtle py-20 text-center">
          <p className="font-mono text-[12px] text-text-secondary mb-2">NO REPORTED WORKLOADS</p>
          <p className="text-text-muted text-sm">Workloads appear after an online agent reports them.</p>
        </div>
      ) : (
        <div className="border-t border-border-subtle">
          {visibleWorkloads.map((w) => {
            const state = normalizeWorkloadState(w.status);
            const cpu = typeof w.status.cpu_percent === "number" ? w.status.cpu_percent : undefined;
            const memory = typeof w.status.memory_bytes === "number" ? w.status.memory_bytes : undefined;
            const machineName = w.machine_id ? machineMap.get(w.machine_id)?.name : undefined;
            const moving = movingWorkloads.has(w.id);
            const blocked = moving || !operator;

            return (
              <div key={w.id} className="grid grid-cols-[1fr_auto] md:grid-cols-[1.6fr_1fr_1fr_auto] gap-4 items-center py-4 border-b border-border-subtle">
                {/* identity */}
                <Link href={`/app/workloads/${w.id}`} className="flex items-center gap-4 min-w-0 group">
                  <StateObject layers={{ process: 0.6, memory: 0.5, filesystem: 0.3, network: 0.2, device: 0.3, environment: 0.1 }} active={state === "running"} size="sm" className="w-10 shrink-0" />
                  <div className="min-w-0">
                    <p className="font-mono text-[13px] text-text truncate group-hover:text-accent transition-colors">{w.name}</p>
                    <p className="font-mono text-[10px] text-text-muted">{machineName ? machineName.toUpperCase() : "UNASSIGNED"}</p>
                    <p className={cn("md:hidden font-mono text-[10px] tracking-[0.14em]", state === "running" ? "text-status-healthy" : state === "stopped" ? "text-text-muted" : "text-status-warning")}>
                      {state.toUpperCase()}
                      {moving && <span className="ml-2 text-accent">· MIGRATING</span>}
                    </p>
                  </div>
                </Link>

                {/* readouts */}
                <div className="hidden md:block font-mono text-[11px] text-text-muted tabular-nums">
                  {cpu !== undefined ? `CPU ${cpu}%` : "CPU —"}
                  <span className="mx-2 text-text-faint">/</span>
                  {memory !== undefined ? formatBytes(memory) : "MEM —"}
                </div>

                {/* status */}
                <span className={cn("hidden md:block font-mono text-[10px] tracking-[0.14em]", state === "running" ? "text-status-healthy" : state === "stopped" ? "text-text-muted" : "text-status-warning")}>
                  {state.toUpperCase()}
                  {moving && <span className="ml-2 text-accent">· MIGRATING</span>}
                </span>

                {/* actions */}
                <div className="flex items-center gap-1.5">
                  <button
                    disabled={!w.machine_id || blocked || command.isPending}
                    title={
                      moving
                        ? "A migration is in flight for this workload"
                        : !operator
                        ? "Operating on workloads requires the operator role"
                        : undefined
                    }
                    onClick={() => setMigrating(w.id)}
                    className="font-mono text-[10px] tracking-[0.12em] text-bg bg-accent px-3 py-1.5 hover:bg-accent-dim transition-colors disabled:opacity-30"
                  >
                    MOVE
                  </button>
                  {actionsForState(state).map((a) => {
                    const key = `${w.id}:${a.key}`;
                    const confirming = confirmingKey === key;
                    const pendingThis = command.isPending && busyKey === key;
                    return (
                      <span key={a.key} className="flex items-center gap-1.5">
                        <button
                          disabled={!w.machine_id || blocked || (command.isPending && busyKey !== key) || (pendingThis && !confirming)}
                          title={
                            moving
                              ? "A migration is in flight for this workload"
                              : !operator
                              ? "Operating on workloads requires the operator role"
                              : undefined
                          }
                          onClick={() => {
                            if (a.confirm && !confirming) {
                              setConfirmingKey(key);
                              return;
                            }
                            run(w.machine_id!, w.id, a.key);
                          }}
                          className={cn(
                            "font-mono text-[10px] tracking-[0.1em] px-2 py-1.5 border transition-colors disabled:opacity-30",
                            a.danger ? "text-status-error border-status-error/30 hover:bg-status-error/10" : "text-text-muted border-border hover:text-text"
                          )}
                        >
                          {busyKey === key ? "…" : confirming ? a.confirm : a.label}
                        </button>
                        {confirming && (
                          <button
                            disabled={pendingThis}
                            onClick={() => setConfirmingKey(null)}
                            className="font-mono text-[10px] tracking-[0.1em] px-2 py-1.5 border border-border text-text-muted hover:text-text transition-colors disabled:opacity-30"
                          >
                            CANCEL
                          </button>
                        )}
                      </span>
                    );
                  })}
                </div>
              </div>
            );
          })}
        </div>
      )}

      {migrateTarget && (
        <MigrateDialog
          workload={migrateTarget}
          machines={machines}
          onClose={() => setMigrating(null)}
        />
      )}
    </div>
  );
}
