"use client";

import React from "react";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { MachineView, StateObject } from "@/components/state";
import { formatBytes, formatDuration } from "@/lib/utils";
import { cn } from "@/lib/utils";
import Link from "next/link";

const STAGES = [
  { key: "checkpoint", label: "CHECKPOINT", stages: ["CREATED", "DISCOVER", "VALIDATE", "SNAPSHOT", "PREPARE"] },
  { key: "transfer", label: "TRANSFER", stages: ["TRANSFER"] },
  { key: "restore", label: "RESTORE", stages: ["VERIFY", "RESTORE"] },
  { key: "validation", label: "VALIDATION", stages: ["POST_VALIDATE", "SWITCH", "COMMIT", "CLEANUP"] },
] as const;

function stageIndex(stage: string): number {
  for (let i = 0; i < STAGES.length; i++) {
    if ((STAGES[i].stages as readonly string[]).includes(stage)) return i;
  }
  return -1;
}

export default function MigrationDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = React.use(params);
  const { organization } = useAuth();
  const orgId = organization?.id || "";

  const migrationsQ = useQuery({ queryKey: ["migrations", orgId], queryFn: () => api.listMigrations(orgId), enabled: !!orgId });
  const machinesQ = useQuery({ queryKey: ["machines", orgId], queryFn: () => api.listMachines(orgId), enabled: !!orgId });
  const eventsQ = useQuery({
    queryKey: ["migration-events", orgId, id],
    queryFn: () => api.listMigrationEvents(orgId, id),
    enabled: !!orgId,
    refetchInterval: () => {
      const m = migrationsQ.data?.find((x) => x.id === id);
      return m && (m.status === "running" || m.status === "queued") ? 2000 : false;
    },
  });

  const migration = migrationsQ.data?.find((m) => m.id === id);
  const machines = machinesQ.data || [];
  const events = eventsQ.data || [];

  if (migrationsQ.isLoading) {
    return <div className="p-10 font-mono text-[11px] text-text-muted">READING MIGRATION…</div>;
  }

  if (!migration) {
    return (
      <div className="p-10">
        <p className="font-mono text-[12px] text-text-secondary">MIGRATION NOT FOUND</p>
        <Link href="/app/migrations" className="font-mono text-[11px] text-accent mt-2 inline-block">← ALL MIGRATIONS</Link>
      </div>
    );
  }

  const source = machines.find((m) => m.id === migration.source_machine_id);
  const dest = machines.find((m) => m.id === migration.destination_machine_id);
  // The progress document's shape is agent-defined; read its optional readouts
  // through a typed view rather than `any`.
  const progress = migration.progress as {
    current_stage?: string;
    transfer_speed?: number;
    downtime_ms?: number;
    deduplicated_bytes?: number;
  };
  const currentStage = progress.current_stage || "";
  const idx = stageIndex(currentStage);
  const isFailed = migration.status === "failed";
  const isDone = migration.status === "completed";

  const sorted = [...events].sort((a, b) => b.sequence - a.sequence);
  const latest = sorted[0];
  const bytesDone = latest?.bytes_done ?? 0;
  const bytesTotal = latest?.bytes_total ?? 0;
  const transferProgress = bytesTotal > 0 ? bytesDone / bytesTotal : isDone ? 1 : 0;

  return (
    <div className="max-w-5xl mx-auto px-5 md:px-10 py-10">
      {/* Header */}
      <div className="flex items-baseline justify-between mb-2">
        <Link href="/app/migrations" className="font-mono text-[10px] text-text-muted hover:text-text">← MIGRATIONS</Link>
        <span className="font-mono text-[10px] text-text-faint">{migration.id.slice(0, 8)}</span>
      </div>
      <div className="flex items-baseline justify-between mb-10">
        <h1 className="font-display text-3xl font-medium text-text">Migration</h1>
        <span className={cn("font-mono text-[11px] tracking-[0.16em]", isDone ? "text-status-healthy" : isFailed ? "text-status-error" : "text-accent")}>
          {migration.status.toUpperCase()}
        </span>
      </div>

      {/* Source / channel / destination */}
      <div className="grid md:grid-cols-[1fr_auto_1fr] gap-6 items-stretch mb-10">
        <MachineView
          name={(source?.name ?? migration.source_machine_id.slice(0, 8)).toUpperCase()}
          specs={["SOURCE"]}
          status={isDone ? "idle" : "online"}
          active={!isDone && !isFailed}
        >
          {!isDone && (
            <StateObject layers={{ process: 0.6, memory: 0.6, filesystem: 0.4, network: 0.3, device: 0.4, environment: 0.2 }} active size="md" />
          )}
          {isDone && <span className="tlabel">state departed</span>}
        </MachineView>

        {/* Vertical channel */}
        <div className="hidden md:flex flex-col items-center justify-center px-2">
          <div className="relative w-px h-24 bg-border">
            <div
              className={cn("absolute left-[-2px] w-[5px] h-[5px]", isFailed ? "bg-status-error" : "bg-accent")}
              style={{ top: `${(isDone ? 1 : transferProgress) * 100}%` }}
            />
          </div>
        </div>

        <MachineView
          name={(dest?.name ?? migration.destination_machine_id.slice(0, 8)).toUpperCase()}
          specs={["DESTINATION"]}
          status={isDone ? "online" : "offline"}
          active={isDone}
        >
          {isDone ? (
            <StateObject layers={{ process: 0.7, memory: 0.6, filesystem: 0.4, network: 0.3, device: 0.4, environment: 0.2 }} active size="md" />
          ) : (
            <span className="tlabel">awaiting state</span>
          )}
        </MachineView>
      </div>

      {/* Stage sequence */}
      <div className="border-y border-border-subtle divide-y divide-border-subtle mb-10">
        {STAGES.map((s, i) => {
          const complete = isDone || idx > i;
          const active = !isDone && !isFailed && idx === i;
          const failedHere = isFailed && idx === i;
          return (
            <div key={s.key} className="grid grid-cols-[1fr_auto_auto] gap-4 items-center py-3 px-2">
              <span className={cn("font-mono text-[12px] tracking-[0.14em]", complete ? "text-text-secondary" : active ? "text-text" : "text-text-muted")}>
                {s.label}
              </span>
              <span className="font-mono text-[10px] text-text-faint">
                {s.key === "transfer" && bytesTotal > 0 ? `${formatBytes(bytesDone)} / ${formatBytes(bytesTotal)}` : ""}
              </span>
              <span className={cn("font-mono text-[10px] tracking-[0.14em]", complete ? "text-status-healthy" : active ? "text-accent" : failedHere ? "text-status-error" : "text-text-faint")}>
                {complete ? "COMPLETE" : active ? "ACTIVE" : failedHere ? "FAILED" : "—"}
              </span>
            </div>
          );
        })}
      </div>

      {/* Readouts */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-px bg-border-subtle border border-border-subtle mb-10">
        <Readout label="TRANSFERRED" value={formatBytes(bytesDone)} />
        <Readout label="SPEED" value={progress?.transfer_speed ? `${formatBytes(progress.transfer_speed)}/s` : "—"} />
        <Readout label="DOWNTIME" value={progress?.downtime_ms ? formatDuration(progress.downtime_ms) : "—"} />
        <Readout label="DEDUPLICATED" value={progress?.deduplicated_bytes ? formatBytes(progress.deduplicated_bytes) : "—"} />
      </div>

      {/* Failure */}
      {isFailed && (
        <div className="border border-status-error/40 bg-status-error/5 p-5 mb-10">
          <p className="font-mono text-[11px] tracking-[0.16em] text-status-error mb-2">MIGRATION FAILED</p>
          <p className="text-sm text-text-secondary mb-1">{migration.error_message || "The migration did not complete."}</p>
          <p className="text-xs text-text-muted mb-4">The workload is still running on the source. Nothing was lost.</p>
          <div className="flex gap-3">
            <button className="font-mono text-[11px] tracking-[0.14em] text-accent border border-accent/40 px-4 py-2 hover:bg-accent hover:text-bg transition-colors">RETRY</button>
            <Link href="/app/machines" className="font-mono text-[11px] tracking-[0.14em] text-text-secondary border border-border px-4 py-2 hover:text-text transition-colors">TRY ANOTHER MACHINE</Link>
          </div>
        </div>
      )}

      {/* Event log */}
      <div>
        <span className="tlabel block mb-4">event log</span>
        <div className="border-t border-border-subtle">
          {[...events].sort((a, b) => a.sequence - b.sequence).map((e) => (
            <div key={e.id} className="grid grid-cols-[70px_110px_1fr] gap-4 py-2 border-b border-border-subtle items-baseline">
              <span className="font-mono text-[10px] text-text-faint tabular-nums">{String(e.sequence).padStart(3, "0")}</span>
              <span className="font-mono text-[10px] text-accent-dim tracking-[0.1em]">{e.stage}</span>
              <span className="text-xs text-text-secondary">{e.message}</span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function Readout({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-bg px-5 py-4">
      <div className="tlabel mb-2">{label}</div>
      <div className="readout text-lg">{value}</div>
    </div>
  );
}
