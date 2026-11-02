"use client";

import React from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { StateObject, ResourceReadout, LoadingState, ErrorState, MessageLine } from "@/components/state";
import { MigrateDialog } from "@/components/migrate-dialog";
import { canOperate, cn, errorMessage, formatBytes } from "@/lib/utils";
import {
  actionsForState,
  normalizeWorkloadState,
  primaryWorkloadAction,
  timeoutForAction,
} from "@/lib/workload-status";
import { migrationIsActive } from "@/lib/migration-view";
import type { AgentAction } from "@/lib/types";
import Link from "next/link";

type Outcome = { kind: "success" | "error"; text: string } | null;

export default function WorkloadDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = React.use(params);
  const orgId = useAuth((s) => s.organization?.id || "");
  const role = useAuth((s) => s.organization?.role);
  const queryClient = useQueryClient();
  const [outcome, setOutcome] = useState<Outcome>(null);
  const [migrating, setMigrating] = useState(false);
  const [confirming, setConfirming] = useState<AgentAction | null>(null);

  const workloadQuery = useQuery({
    queryKey: ["workloads", orgId],
    queryFn: () => api.listWorkloads(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  });
  const machinesQuery = useQuery({
    queryKey: ["machines", orgId],
    queryFn: () => api.listMachines(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  });
  const migrationsQuery = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
  });

  const workload = workloadQuery.data?.find((item) => item.id === id);
  const machine = machinesQuery.data?.find((item) => item.machine_id === workload?.machine_id);
  const moving = (migrationsQuery.data || []).some((m) => m.workload_id === id && migrationIsActive(m));

  const command = useMutation({
    mutationFn: (action: AgentAction) => {
      if (!machine) throw new Error("No reachable source agent assigned");
      return api.agentCommand(orgId, machine.machine_id, {
        action: action as never,
        workload_id: id,
        timeout_seconds: timeoutForAction(action),
      });
    },
    onSuccess: (_r, action) => {
      setOutcome({ kind: "success", text: `${action} command completed` });
      setConfirming(null);
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
      // Checkpoint dispatch registers a checkpoint server-side: refresh the
      // checkpoint list or the new entry stays invisible for the cache TTL.
      queryClient.invalidateQueries({ queryKey: ["checkpoints", orgId] });
    },
    onError: (error: unknown) => setOutcome({ kind: "error", text: errorMessage(error, "Agent command failed") }),
    onSettled: () => setConfirming(null),
  });

  if (workloadQuery.isLoading || machinesQuery.isLoading) {
    return <LoadingState label="READING WORKLOAD…" />;
  }

  if (workloadQuery.isError || machinesQuery.isError) {
    const failed = workloadQuery.isError ? workloadQuery : machinesQuery;
    return (
      <div className="p-10">
        <ErrorState
          title="COULD NOT LOAD THIS WORKLOAD"
          message={errorMessage(failed.error, "The control plane did not answer the query. This is a connection or session problem — not a missing workload.")}
          onRetry={() => {
            void workloadQuery.refetch();
            void machinesQuery.refetch();
          }}
        />
        <Link href="/app/workloads" className="font-mono text-[11px] text-accent mt-4 inline-block">← ALL WORKLOADS</Link>
      </div>
    );
  }

  if (!workload) {
    return (
      <div className="p-10">
        <p className="font-mono text-[12px] text-text-secondary">WORKLOAD NOT FOUND</p>
        <p className="text-text-muted text-sm mt-2">
          No workload with id <span className="font-mono">{id}</span> is registered in this organization.
        </p>
        <Link href="/app/workloads" className="font-mono text-[11px] text-accent mt-2 inline-block">← ALL WORKLOADS</Link>
      </div>
    );
  }

  const state = normalizeWorkloadState(workload.status);
  const running = state === "running";
  const deleted = state === "deleted";
  const specs = actionsForState(state);
  const primary = primaryWorkloadAction(state);
  const checkpointAction = specs.find((a) => a.key === "checkpoint") ?? null;
  const dangerAction = specs.find((a) => a.key === "stop") ?? specs.find((a) => a.key === "delete") ?? null;
  const operator = canOperate(role);
  const blocked = !machine || moving || !operator || command.isPending;
  const blockReason = moving
    ? "A migration is in flight for this workload"
    : !operator
    ? "Operating on workloads requires the operator role"
    : undefined;
  const spec = workload.spec as { root_path?: string; command?: string[] };

  return (
    <div className="max-w-5xl mx-auto px-5 md:px-10 py-10">
      <div className="flex items-baseline justify-between mb-2">
        <Link href="/app/workloads" className="font-mono text-[10px] text-text-muted hover:text-text">← WORKLOADS</Link>
        <span className="font-mono text-[10px] text-text-faint">{workload.id.slice(0, 8)}</span>
      </div>

      <div className="flex items-baseline justify-between mb-10">
        <h1 className="font-display text-3xl font-medium text-text">{workload.name}</h1>
        <span className={cn("font-mono text-[11px] tracking-[0.16em]", running ? "text-status-healthy" : deleted ? "text-text-faint" : "text-text-muted")}>
          {state.toUpperCase()}
        </span>
      </div>

      {deleted && (
        <div className="border border-border-subtle bg-bg-elevated p-4 mb-8">
          <p className="font-mono text-[11px] tracking-[0.14em] text-text-secondary mb-1">DELETED</p>
          <p className="text-xs text-text-muted">
            This workload was deleted and is kept as a record only. It is hidden from the fleet list.
          </p>
        </div>
      )}

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

          {/* Actions — only the ones legal for this state ever render. */}
          {!deleted && (
            <div className="mt-6 flex flex-col gap-2">
              <button
                onClick={() => setMigrating(true)}
                disabled={blocked}
                title={blockReason}
                className="block text-center font-mono text-[12px] tracking-[0.2em] text-bg bg-accent px-5 py-3 hover:bg-accent-dim transition-colors disabled:opacity-30 disabled:pointer-events-none"
              >
                MOVE →
              </button>
              <div className="grid grid-cols-3 gap-2">
                {checkpointAction ? (
                  <Action
                    onClick={() => command.mutate("checkpoint")}
                    disabled={blocked}
                    title={blockReason}
                  >
                    CHECKPOINT
                  </Action>
                ) : (
                  <Action onClick={() => undefined} disabled title="Not available in this state">CHECKPOINT</Action>
                )}
                {primary ? (
                  <Action onClick={() => command.mutate(primary.key)} disabled={blocked} title={blockReason}>
                    {primary.label}
                  </Action>
                ) : (
                  <Action onClick={() => undefined} disabled title="No lifecycle command is legal in this state">
                    {moving ? "MIGRATING…" : "—"}
                  </Action>
                )}
                {confirming && dangerAction ? (
                  <div className="grid grid-cols-1 gap-1">
                    <Action onClick={() => command.mutate(confirming)} disabled={blocked} danger>
                      {dangerAction.confirm ?? `CONFIRM ${dangerAction.label}`}
                    </Action>
                    <Action onClick={() => setConfirming(null)} disabled={command.isPending}>CANCEL</Action>
                  </div>
                ) : dangerAction ? (
                  <Action
                    onClick={() => setConfirming(dangerAction.key)}
                    disabled={blocked}
                    title={blockReason}
                    danger
                  >
                    {dangerAction.label}
                  </Action>
                ) : (
                  <Action onClick={() => undefined} disabled title="Not available in this state">DELETE</Action>
                )}
              </div>
              {(state === "checkpointed" || state === "failed") && (
                <Link
                  href="/app/checkpoints"
                  className="block text-center font-mono text-[10px] tracking-[0.14em] text-text-secondary border border-border px-3 py-2 hover:text-text transition-colors"
                >
                  RESTORE FROM CHECKPOINT →
                </Link>
              )}
            </div>
          )}

          <div className="mt-4">
            <MessageLine
              pendingLabel="DISPATCHING…"
              message={outcome?.text ?? null}
              pending={command.isPending}
              success={outcome?.kind === "success"}
            />
          </div>
        </div>

        {/* Readouts */}
        <div>
          <span className="tlabel block mb-4">resource readout</span>
          <WorkloadMetrics status={workload.status} />

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

// Metrics are shown only when the deployment actually reports them on the
// workload's status. Nothing here is invented: a missing field renders as
// "not reported", never as a placeholder number.
function WorkloadMetrics({ status }: { status: Record<string, unknown> }) {
  const cpu = typeof status.cpu_percent === "number" ? status.cpu_percent : undefined;
  const memory = typeof status.memory_bytes === "number" ? status.memory_bytes : undefined;

  if (cpu === undefined && memory === undefined) {
    return (
      <div className="border border-border-subtle p-4">
        <p className="font-mono text-[11px] text-text-muted mb-1">NO METRICS REPORTED</p>
        <p className="text-xs text-text-secondary leading-relaxed">
          This deployment does not report live resource telemetry for this workload. CPU and memory
          appear here only when an agent reports them on the workload status.
        </p>
      </div>
    );
  }

  return (
    <div className="border-t border-border-subtle">
      {cpu !== undefined ? (
        <ResourceReadout label="CPU" value={cpu.toFixed(1)} unit="%" fraction={Math.min(1, cpu / 100)} />
      ) : (
        <Row label="CPU" value="not reported" />
      )}
      {memory !== undefined ? (
        <ResourceReadout label="MEMORY" value={formatBytes(memory)} fraction={undefined} accent />
      ) : (
        <Row label="MEMORY" value="not reported" />
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

function Action({ children, onClick, disabled, danger, title }: { children: React.ReactNode; onClick: () => void; disabled?: boolean; danger?: boolean; title?: string }) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={cn(
        "font-mono text-[10px] tracking-[0.12em] px-2 py-2 border transition-colors disabled:opacity-30 disabled:pointer-events-none",
        danger ? "text-status-error border-status-error/30 hover:bg-status-error/10" : "text-text-secondary border-border hover:text-text hover:border-border-strong"
      )}
    >
      {children}
    </button>
  );
}
