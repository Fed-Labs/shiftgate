"use client";

import React from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { MachineView, StateObject } from "@/components/state";
import { canOperate, errorMessage, formatBytes, formatDateTime, formatDuration } from "@/lib/utils";
import { cn } from "@/lib/utils";
import type { MigrationJob } from "@/lib/types";
import {
  MIGRATION_STAGE_GROUPS,
  migrationIsActive,
  migrationOutcome,
  readMigrationProgress,
  rememberMigrationRoute,
  resolveMigrationRoute,
  stageGroupIndex,
} from "@/lib/migration-view";
import Link from "next/link";
import { useRouter } from "next/navigation";

export default function MigrationDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = React.use(params);
  const { organization } = useAuth();
  const orgId = organization?.id || "";
  const role = organization?.role;
  const queryClient = useQueryClient();
  const router = useRouter();
  const [retryError, setRetryError] = React.useState<string | null>(null);

  const migrationsQ = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
    // The stage tracker reads the job document — poll it while this migration
    // is moving or the header freezes mid-flight.
    refetchInterval: (query) => {
      const m = query.state.data?.find((x) => x.id === id);
      return m && migrationIsActive(m) ? 2_000 : false;
    },
    refetchOnWindowFocus: true,
  });
  const machinesQ = useQuery({ queryKey: ["machines", orgId], queryFn: () => api.listMachines(orgId), enabled: !!orgId });
  const eventsQ = useQuery({
    queryKey: ["migration-events", orgId, id],
    queryFn: () => api.listMigrationEvents(orgId, id),
    enabled: !!orgId,
    refetchInterval: () => {
      const m = migrationsQ.data?.find((x) => x.id === id);
      return m && migrationIsActive(m) ? 2000 : false;
    },
  });

  // RETRY is a real migration create: same workload, endpoints, mode, and
  // route under a fresh idempotency key. The route comes from the record when
  // the server returns it, else from the creating browser's memory — never a
  // silent downgrade of a relay move to direct.
  const retry = useMutation({
    mutationFn: (migration: MigrationJob) =>
      api.createMigration(orgId, {
        workload_id: migration.workload_id,
        source_machine_id: migration.source_machine_id,
        destination_machine_id: migration.destination_machine_id,
        mode: migration.mode,
        route: resolveMigrationRoute(migration),
        idempotency_key: `${migration.id}-retry-${Date.now()}`,
        timeout_seconds: 600,
      }),
    onSuccess: (created) => {
      setRetryError(null);
      rememberMigrationRoute(created.id, resolveMigrationRoute(created) ?? "direct", created.relay_endpoint);
      queryClient.invalidateQueries({ queryKey: ["migrations", orgId] });
      router.push(`/app/migrations/${created.id}`);
    },
    onError: (error: unknown) => setRetryError(errorMessage(error, "Retry failed")),
  });

  const migration = migrationsQ.data?.find((m) => m.id === id);
  const machines = machinesQ.data || [];
  const events = eventsQ.data || [];

  if (migrationsQ.isLoading) {
    return <div className="p-10 font-mono text-[11px] text-text-muted">READING MIGRATION…</div>;
  }

  // A failed fetch is a load error with a retry, never "not found" — and rows
  // that fell past a page boundary must not be misreported as missing.
  if (migrationsQ.isError && !migrationsQ.data) {
    return (
      <div className="p-10">
        <p className="font-mono text-[12px] text-status-error mb-2">COULD NOT LOAD THIS MIGRATION</p>
        <p className="text-sm text-text-secondary mb-4">
          {errorMessage(migrationsQ.error, "The control plane did not answer the query. This is a connection or session problem — not a missing migration.")}
        </p>
        <button
          onClick={() => void migrationsQ.refetch()}
          className="font-mono text-[11px] tracking-[0.14em] text-accent border border-accent/40 px-4 py-2 hover:bg-accent hover:text-bg transition-colors"
        >
          RETRY
        </button>
        <Link href="/app/migrations" className="font-mono text-[11px] text-accent mt-4 inline-block">← ALL MIGRATIONS</Link>
      </div>
    );
  }

  if (!migration) {
    return (
      <div className="p-10">
        <p className="font-mono text-[12px] text-text-secondary">MIGRATION NOT FOUND</p>
        <Link href="/app/migrations" className="font-mono text-[11px] text-accent mt-2 inline-block">← ALL MIGRATIONS</Link>
      </div>
    );
  }

  // Migrations reference machines by their agent-side machine_id slug.
  const source = machines.find((m) => m.machine_id === migration.source_machine_id);
  const dest = machines.find((m) => m.machine_id === migration.destination_machine_id);
  // The progress document is the control-plane contract: current_stage,
  // percent (0..100), downtime_ms, transfer_speed, and the byte counters.
  const readouts = readMigrationProgress(migration.progress);
  const currentStage = readouts.stage;
  const isActive = migrationIsActive(migration);
  const isDone = migration.status === "completed";
  const outcome = migrationOutcome(migration);

  const sorted = [...events].sort((a, b) => b.sequence - a.sequence);
  const latest = sorted[0];
  // Terminal records carry the terminal marker itself as the stage
  // (FAILED / ROLLED_BACK / CANCELLED). Fall back to the last event's stage to
  // name the row where things actually went wrong.
  const trackerStage = (currentStage || latest?.stage || "").toUpperCase();
  const idx = stageGroupIndex(trackerStage);
  const failureIdx = idx >= 0 ? idx : stageGroupIndex((latest?.stage || "").toUpperCase());

  const bytesDone = latest?.bytes_done ?? readouts.transferredBytes ?? 0;
  const bytesTotal = latest?.bytes_total ?? readouts.totalStateBytes ?? 0;
  const transferProgress =
    bytesTotal > 0 ? Math.min(1, bytesDone / bytesTotal) : isDone ? 1 : readouts.percent / 100;

  const rowState = (i: number): "complete" | "active" | "failed" | "rolled" | "none" => {
    if (i === MIGRATION_STAGE_GROUPS.length - 1) {
      if (outcome.kind === "rolled-back") return "rolled";
      if (trackerStage === "ROLLING_BACK" && isActive) return "active";
      return "none";
    }
    if (isDone) return "complete";
    if (isActive) {
      if (idx > i) return "complete";
      if (idx === i) return "active";
      return "none";
    }
    if (failureIdx >= 0 && i < failureIdx) return "complete";
    if (failureIdx === i) return "failed";
    return "none";
  };

  // Tiles describe the record's truth (stage + outcome + machine presence),
  // not a guess derived from "is it done".
  const sourceStatus = source ? (source.status === "online" ? "online" : source.status === "draining" ? "idle" : "offline") : "offline";
  const destStatus = dest ? (dest.status === "online" ? "online" : dest.status === "draining" ? "idle" : "offline") : "offline";
  const sourceDeparted = outcome.kind === "completed";

  return (
    <div className="max-w-5xl mx-auto px-5 md:px-10 py-10">
      {/* Header */}
      <div className="flex items-baseline justify-between mb-2">
        <Link href="/app/migrations" className="font-mono text-[10px] text-text-muted hover:text-text">← MIGRATIONS</Link>
        <span className="font-mono text-[10px] text-text-faint">{migration.id.slice(0, 8)}</span>
      </div>
      <div className="flex items-baseline justify-between mb-2">
        <h1 className="font-display text-3xl font-medium text-text">Migration</h1>
        <span
          className={cn(
            "font-mono text-[11px] tracking-[0.16em]",
            outcome.kind === "completed"
              ? "text-status-healthy"
              : outcome.kind === "needs-operator"
              ? "text-status-error"
              : outcome.kind === "active"
              ? "text-accent"
              : "text-status-warning"
          )}
        >
          {migration.status.toUpperCase()}
          {outcome.kind === "rolled-back" && " · ROLLED BACK"}
          {outcome.kind === "completed-with-warning" && " · CLEANUP FAILED"}
        </span>
      </div>
      <p className="font-mono text-[10px] text-text-faint mb-10">
        updated {formatDateTime(migration.updated_at)}
        {migrationsQ.isRefetchError && <span className="text-status-warning ml-3">· live refresh failing — last known data</span>}
      </p>

      {/* Source / channel / destination */}
      <div className="grid md:grid-cols-[1fr_auto_1fr] gap-6 items-stretch mb-10">
        <MachineView
          name={(source?.name ?? migration.source_machine_id.slice(0, 8)).toUpperCase()}
          specs={["SOURCE"]}
          status={sourceStatus}
          active={isActive && !sourceDeparted}
        >
          {sourceDeparted ? (
            <span className="tlabel">state departed</span>
          ) : outcome.kind === "completed-with-warning" ? (
            <span className="tlabel text-status-warning">state may remain — cleanup failed</span>
          ) : outcome.kind === "rolled-back" ? (
            <span className="tlabel text-status-healthy">state preserved here</span>
          ) : outcome.kind === "needs-operator" ? (
            <span className="tlabel text-status-error">verify state before acting</span>
          ) : outcome.kind === "cancelled" ? (
            <span className="tlabel text-status-warning">cancellation sent — verify state</span>
          ) : (
            <StateObject layers={{ process: 0.6, memory: 0.6, filesystem: 0.4, network: 0.3, device: 0.4, environment: 0.2 }} active size="md" />
          )}
        </MachineView>

        {/* Vertical channel */}
        <div className="hidden md:flex flex-col items-center justify-center px-2">
          <div className="relative w-px h-24 bg-border">
            <div
              className={cn("absolute left-[-2px] w-[5px] h-[5px]", outcome.kind === "needs-operator" ? "bg-status-error" : "bg-accent")}
              style={{ top: `${transferProgress * 100}%` }}
            />
          </div>
        </div>

        <MachineView
          name={(dest?.name ?? migration.destination_machine_id.slice(0, 8)).toUpperCase()}
          specs={["DESTINATION"]}
          status={destStatus}
          active={isDone}
        >
          {isDone ? (
            <StateObject layers={{ process: 0.7, memory: 0.6, filesystem: 0.4, network: 0.3, device: 0.4, environment: 0.2 }} active size="md" />
          ) : outcome.kind === "needs-operator" ? (
            <span className="tlabel text-status-error">may hold partial state — verify</span>
          ) : (
            <span className="tlabel">awaiting state</span>
          )}
        </MachineView>
      </div>

      {/* Stage sequence */}
      <div className="border-y border-border-subtle divide-y divide-border-subtle mb-10">
        {MIGRATION_STAGE_GROUPS.map((s, i) => {
          const state = rowState(i);
          return (
            <div key={s.key} className="grid grid-cols-[1fr_auto_auto] gap-4 items-center py-3 px-2">
              <span
                className={cn(
                  "font-mono text-[12px] tracking-[0.14em]",
                  state === "complete" || state === "failed" || state === "rolled"
                    ? "text-text-secondary"
                    : state === "active"
                    ? "text-text"
                    : "text-text-muted"
                )}
              >
                {s.label}
              </span>
              <span className="font-mono text-[10px] text-text-faint">
                {s.key === "transfer" && (bytesTotal > 0 || readouts.transferredBytes !== undefined)
                  ? `${formatBytes(bytesDone)} / ${bytesTotal > 0 ? formatBytes(bytesTotal) : "?"}`
                  : ""}
              </span>
              <span
                className={cn(
                  "font-mono text-[10px] tracking-[0.14em]",
                  state === "complete"
                    ? "text-status-healthy"
                    : state === "active"
                    ? "text-accent"
                    : state === "rolled"
                    ? "text-status-warning"
                    : state === "failed"
                    ? "text-status-error"
                    : "text-text-faint"
                )}
              >
                {state === "complete"
                  ? "COMPLETE"
                  : state === "active"
                  ? "ACTIVE"
                  : state === "rolled"
                  ? "ROLLED BACK"
                  : state === "failed"
                  ? migration.status === "cancelled"
                    ? "CANCELLED"
                    : "FAILED"
                  : "—"}
              </span>
            </div>
          );
        })}
      </div>

      {/* Readouts */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-px bg-border-subtle border border-border-subtle mb-10">
        <Readout label="TRANSFERRED" value={formatBytes(bytesDone)} />
        <Readout
          label="SPEED"
          value={readouts.transferSpeed ? `${formatBytes(readouts.transferSpeed)}/s` : "—"}
        />
        <Readout label="DOWNTIME" value={readouts.downtimeMs !== undefined ? formatDuration(readouts.downtimeMs) : "—"} />
        <Readout label="DEDUPLICATED" value={readouts.deduplicatedBytes !== undefined ? formatBytes(readouts.deduplicatedBytes) : "—"} />
      </div>

      {/* Terminal outcome — each kind says only what is true for it */}
      {outcome.kind === "rolled-back" && (
        <div className="border border-status-healthy/40 bg-status-healthy/5 p-5 mb-10">
          <p className="font-mono text-[11px] tracking-[0.16em] text-status-healthy mb-2">MIGRATION ROLLED BACK</p>
          <p className="text-sm text-text-secondary mb-1">
            {migration.error_message || "The migration rolled back and the workload was preserved on the source."}
          </p>
          <p className="text-xs text-text-muted mb-4">
            The workload is still running on the source. Nothing was lost — a retry is safe once the cause is understood.
          </p>
          <RetryActions
            canRetry={canOperate(role)}
            pending={retry.isPending}
            onRetry={() => retry.mutate(migration)}
            retryError={retryError}
          />
        </div>
      )}

      {outcome.kind === "needs-operator" && (
        <div className="border border-status-error/40 bg-status-error/5 p-5 mb-10">
          <p className="font-mono text-[11px] tracking-[0.16em] text-status-error mb-2">
            MIGRATION FAILED{outcome.rollbackIncomplete ? " — OPERATOR INTERVENTION REQUIRED" : ""}
          </p>
          <p className="text-sm text-text-secondary mb-1">{migration.error_message || "The migration did not complete."}</p>
          {outcome.rollbackIncomplete ? (
            <p className="text-xs text-text-muted mb-4">
              Rollback did not complete. The source may still be frozen and the destination may hold
              partial state — verify both machines before retrying or restarting anything.
            </p>
          ) : (
            <p className="text-xs text-text-muted mb-4">
              Do not assume the source is running — check the workload&apos;s state on the source machine
              before retrying.
            </p>
          )}
          <RetryActions
            canRetry={canOperate(role)}
            pending={retry.isPending}
            onRetry={() => retry.mutate(migration)}
            retryError={retryError}
          />
        </div>
      )}

      {outcome.kind === "completed-with-warning" && (
        <div className="border border-status-warning/40 bg-status-warning/5 p-5 mb-10">
          <p className="font-mono text-[11px] tracking-[0.16em] text-status-warning mb-2">
            COMPLETED WITH CLEANUP FAILURE
          </p>
          <p className="text-sm text-text-secondary mb-1">{migration.error_message}</p>
          <p className="text-xs text-text-muted">
            The destination is live, but source cleanup failed — the workload may still be running on
            the source too. Stop one of the copies before relying on this move.
          </p>
        </div>
      )}

      {outcome.kind === "cancelled" && (
        <div className="border border-status-warning/40 bg-status-warning/5 p-5 mb-10">
          <p className="font-mono text-[11px] tracking-[0.16em] text-status-warning mb-2">CANCELLATION REQUESTED</p>
          <p className="text-sm text-text-secondary mb-1">
            {migration.error_message ||
              "The agent is settling the cancellation: a rollback may still be in flight."}
          </p>
          <p className="text-xs text-text-muted">
            {currentStage ? `Last reported stage: ${currentStage.toUpperCase()}. ` : ""}
            Verify the workload on both machines before starting anything new.
          </p>
        </div>
      )}

      {/* Event log */}
      <div>
        <span className="tlabel block mb-4">event log</span>
        {eventsQ.isError ? (
          <p className="text-xs text-status-error">
            Could not load the event log — the events query failed. It retries while the migration runs.
          </p>
        ) : eventsQ.isLoading ? (
          <p className="font-mono text-[11px] text-text-muted">READING EVENTS…</p>
        ) : events.length === 0 ? (
          <p className="text-xs text-text-muted">
            No events recorded yet. Events appear as the agent reports stages.
          </p>
        ) : (
          <div className="border-t border-border-subtle">
            {[...events].sort((a, b) => a.sequence - b.sequence).map((e) => (
              <div key={e.id} className="grid grid-cols-[70px_110px_1fr_auto] gap-4 py-2 border-b border-border-subtle items-baseline">
                <span className="font-mono text-[10px] text-text-faint tabular-nums">{String(e.sequence).padStart(3, "0")}</span>
                <span className="font-mono text-[10px] text-accent-dim tracking-[0.1em]">{e.stage}</span>
                <span className="text-xs text-text-secondary">{e.message}</span>
                <span
                  className="font-mono text-[10px] text-text-faint whitespace-nowrap"
                  title={e.created_at ? formatDateTime(e.created_at) : undefined}
                >
                  {e.created_at ? timeAgoLabel(e.created_at) : ""}
                </span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function timeAgoLabel(date: string): string {
  const then = new Date(date).getTime();
  if (!Number.isFinite(then)) return "";
  const seconds = Math.floor((Date.now() - then) / 1000);
  if (seconds < 60) return "just now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

function RetryActions({
  canRetry,
  pending,
  onRetry,
  retryError,
}: {
  canRetry: boolean;
  pending: boolean;
  onRetry: () => void;
  retryError: string | null;
}) {
  return (
    <div>
      <div className="flex gap-3">
        <button
          onClick={onRetry}
          disabled={!canRetry || pending}
          className="font-mono text-[11px] tracking-[0.14em] text-accent border border-accent/40 px-4 py-2 hover:bg-accent hover:text-bg transition-colors disabled:opacity-50"
        >
          {pending ? "RETRYING…" : "RETRY"}
        </button>
        <Link href="/app/machines" className="font-mono text-[11px] tracking-[0.14em] text-text-secondary border border-border px-4 py-2 hover:text-text transition-colors">TRY ANOTHER MACHINE</Link>
      </div>
      {!canRetry && (
        <p className="text-xs text-text-muted mt-2">Starting a migration requires the operator role.</p>
      )}
      {retryError && <p className="text-xs text-status-error mt-3">{retryError}</p>}
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
