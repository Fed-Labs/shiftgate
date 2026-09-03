// Migration view — the live pipeline the spec centers on:
//
//   Checkpointing → Transferring → Restoring → Validating → Completed
//
// The agent's stage machine is finer-grained than the five user-facing
// phases, so this screen folds the real stages into those phases and shows
// the technical detail (bytes, rates, per-stage events) beneath them. A
// failing migration shows the real failure code and the rollback state.

import React from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Loader2, X } from "lucide-react";
import clsx from "clsx";
import { backend, formatBytes, formatDuration, shortID } from "@/lib/backend";
import { Badge, Button, Card, EmptyState, ProgressBar, SectionLabel, Spinner, KeyValue } from "@/components/ui";
import type { MigrationStage } from "@/lib/types";

/** The five phases users see, in order. */
const PHASES = ["Checkpointing", "Transferring", "Restoring", "Validating", "Completed"] as const;
type Phase = (typeof PHASES)[number];

/** Map the agent's fine-grained stages onto the user-facing phases. */
function phaseOf(stage: MigrationStage): Phase {
  switch (stage) {
    case "CREATED":
    case "DISCOVER":
    case "VALIDATE":
    case "SNAPSHOT":
    case "PREPARE":
      return "Checkpointing";
    case "TRANSFER":
    case "VERIFY":
      return "Transferring";
    case "RESTORE":
    case "POST_VALIDATE":
      return "Restoring";
    case "SWITCH":
    case "COMMIT":
    case "CLEANUP":
      return "Validating";
    case "COMPLETED":
      return "Completed";
    default:
      // FAILED, ROLLING_BACK, ROLLED_BACK, CANCELLED — no phase; the banner
      // below carries the true state.
      return "Validating";
  }
}

function phaseIndex(stage: MigrationStage): number {
  return PHASES.indexOf(phaseOf(stage));
}

export function isTerminal(stage: MigrationStage): boolean {
  return stage === "COMPLETED" || stage === "FAILED" || stage === "ROLLED_BACK" || stage === "CANCELLED";
}

export function MigrationView({ migrationID, onClose }: { migrationID: string; onClose: () => void }) {
  const queryClient = useQueryClient();
  const migration = useQuery({
    queryKey: ["agent-migration", migrationID],
    queryFn: () => backend.migration(migrationID),
    // Poll fast while running, slow to a stop once terminal.
    refetchInterval: (query) =>
      query.state.data && isTerminal(query.state.data.stage) ? false : 1_000,
    retry: false,
  });

  if (migration.isLoading) {
    return (
      <div className="flex items-center justify-center py-24">
        <Spinner className="w-6 h-6" />
      </div>
    );
  }

  if (migration.isError || !migration.data) {
    return (
      <EmptyState
        title="Migration not found"
        description={migration.error instanceof Error ? migration.error.message : String(migration.error)}
        action={<Button onClick={onClose}>Back</Button>}
      />
    );
  }

  const value = migration.data;
  const current = phaseIndex(value.stage);
  const failed = value.stage === "FAILED";
  const cancelled = value.stage === "CANCELLED";
  const rollingBack = value.stage === "ROLLING_BACK";
  const rolledBack = value.stage === "ROLLED_BACK";
  const terminal = isTerminal(value.stage);
  const latest = value.events[value.events.length - 1];
  const progress = latest?.progress ?? 0;
  const metrics = value.metrics;
  const transferRate = metrics.transfer_bytes_per_second;

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-xl font-semibold">
            Migration {shortID(value.id)}
          </h1>
          <p className="text-[13px] text-text-muted mt-0.5">
            {value.workload_id !== "" && (
              <>
                workload <span className="readout">{shortID(value.workload_id)}</span> ·{" "}
              </>
            )}
            {value.mode} · {new Date(value.created_at).toLocaleString()}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {value.source_preserved && <Badge tone="info">source preserved</Badge>}
          <Badge tone={failed ? "error" : cancelled || rolledBack ? "offline" : terminal ? "healthy" : "accent"}>
            {value.stage}
          </Badge>
          {!terminal && (
            <Button
              variant="ghost"
              onClick={() =>
                backend
                  .cancelMigration(value.id)
                  .catch(() => undefined)
                  .finally(() => void queryClient.invalidateQueries({ queryKey: ["agent-migration", value.id] }))
              }
            >
              Cancel migration
            </Button>
          )}
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
        </div>
      </div>

      {/* Failure / rollback banner — the real reason, never a euphemism */}
      {(failed || rollingBack || rolledBack || cancelled) && (
        <div
          className={clsx(
            "rounded-lg border px-4 py-3 text-[13px]",
            failed
              ? "border-status-error/40 bg-status-error/10 text-status-error"
              : rolledBack || cancelled
                ? "border-border bg-bg-surface text-text-secondary"
                : "border-status-warning/40 bg-status-warning/10 text-status-warning"
          )}
        >
          {failed && (
            <>
              <span className="readout">{value.failure_code || "FAILED"}</span>
              {value.failure_reason ? ` — ${value.failure_reason}` : ""}
              {value.stage !== "ROLLING_BACK" && (
                <div className="text-[12px] mt-1 text-text-muted">
                  The source is {value.source_preserved ? "preserved and still usable" : "not preserved"}.
                  {value.stage === "FAILED" && " The agent will roll the destination back; nothing is half-restored."}
                </div>
              )}
            </>
          )}
          {rollingBack && <span>Rolling the destination back — the restore is being undone cleanly.</span>}
          {rolledBack && <span>Rolled back. The destination is clean; the source remains authoritative.</span>}
          {cancelled && <span>Cancelled at the operator's request.</span>}
        </div>
      )}

      {/* The pipeline */}
      <Card className="p-6">
        <div className="flex items-center justify-between gap-1">
          {PHASES.map((phase, index) => {
            const done = terminal && !failed && !cancelled && !rolledBack
              ? index < PHASES.length - 1
              : index < current;
            const activeNow = !terminal && index === current;
            const isFailedHere = (failed || cancelled || rolledBack) && index === current;
            return (
              <React.Fragment key={phase}>
                {index > 0 && (
                  <div
                    className={clsx(
                      "flex-1 h-px mx-1",
                      done || activeNow ? "bg-accent" : "bg-border"
                    )}
                  />
                )}
                <div className="flex flex-col items-center gap-1.5 min-w-20">
                  <div
                    className={clsx(
                      "w-7 h-7 rounded-full flex items-center justify-center border",
                      done
                        ? "bg-accent/15 border-accent text-accent"
                        : activeNow
                          ? "bg-accent/10 border-accent text-accent"
                          : isFailedHere
                            ? "bg-status-error/10 border-status-error text-status-error"
                            : "border-border text-text-faint"
                    )}
                  >
                    {done ? (
                      <Check className="w-3.5 h-3.5" />
                    ) : activeNow ? (
                      <Loader2 className="w-3.5 h-3.5 animate-spin" />
                    ) : isFailedHere ? (
                      <X className="w-3.5 h-3.5" />
                    ) : (
                      <span className="text-[10px] readout">{index + 1}</span>
                    )}
                  </div>
                  <span
                    className={clsx(
                      "text-[11px]",
                      done || activeNow ? "text-text" : "text-text-faint"
                    )}
                  >
                    {phase}
                  </span>
                </div>
              </React.Fragment>
            );
          })}
        </div>

        {/* Overall progress bar */}
        <div className="mt-6">
          <div className="flex items-center justify-between mb-1.5">
            <span className="tlabel">
              {latest ? latest.message || latest.stage : value.stage}
            </span>
            <span className="readout text-[12px] text-text-secondary">
              {Math.round(progress)}%
            </span>
          </div>
          <ProgressBar value={progress} tone={failed ? "error" : rollingBack ? "warning" : "accent"} />
        </div>
      </Card>

      {/* Technical detail beneath — meaningful, not overwhelming */}
      <div className="grid md:grid-cols-2 gap-3">
        <Card className="p-4">
          <SectionLabel>Transfer</SectionLabel>
          <KeyValue
            label="State captured"
            value={formatBytes(metrics.total_state_bytes)}
          />
          <KeyValue label="Transferred" value={formatBytes(metrics.transferred_bytes)} />
          <KeyValue label="Deduplicated away" value={formatBytes(metrics.deduplicated_bytes)} />
          <KeyValue
            label="Rate"
            value={transferRate > 0 ? `${formatBytes(transferRate)}/s` : "—"}
          />
          <KeyValue label="Transfer time" value={formatDuration(metrics.transfer_duration / 1e9)} />
        </Card>
        <Card className="p-4">
          <SectionLabel>Timing</SectionLabel>
          <KeyValue label="Checkpoint" value={formatDuration(metrics.checkpoint_duration / 1e9)} />
          <KeyValue label="Restore" value={formatDuration(metrics.restore_duration / 1e9)} />
          <KeyValue label="Workload downtime" value={formatDuration(metrics.downtime / 1e9)} />
          <KeyValue
            label="Destination"
            value={value.destination.server_name || shortID(value.destination.machine_id)}
          />
          <KeyValue label="Compatibility" value={value.compatibility.compatible ? "compatible" : "issues reported"} />
        </Card>
      </div>

      {/* Compatibility issues, when the checker found any */}
      {value.compatibility.issues.length > 0 && (
        <Card className="p-4">
          <SectionLabel>Compatibility notes</SectionLabel>
          <div className="space-y-2">
            {value.compatibility.issues.map((issue, index) => (
              <div key={index} className="text-[13px]">
                <div className="flex items-baseline gap-2">
                  <Badge
                    tone={
                      issue.severity === "error" ? "error" : issue.severity === "warning" ? "warning" : "neutral"
                    }
                  >
                    {issue.severity}
                  </Badge>
                  <span className="readout text-[11px] text-text-faint">{issue.resource}</span>
                </div>
                <p className="text-text-secondary mt-1 leading-relaxed">{issue.description}</p>
                {issue.adaptation && (
                  <p className="text-text-muted text-[12px] mt-0.5">Adaptation: {issue.adaptation}</p>
                )}
              </div>
            ))}
          </div>
        </Card>
      )}

      {/* Event log — the migration's own words */}
      <Card className="p-4">
        <SectionLabel>Event log</SectionLabel>
        <div className="max-h-72 overflow-y-auto divide-y divide-border-subtle">
          {[...value.events].reverse().map((event) => (
            <div key={event.sequence} className="flex items-start gap-3 py-2">
              <span className="readout text-[11px] text-text-faint w-14 shrink-0">
                {new Date(event.timestamp).toLocaleTimeString()}
              </span>
              <span className="text-[11px] text-accent w-28 shrink-0 readout">{event.stage}</span>
              <span className="text-[12px] text-text-secondary flex-1">
                {event.message}
                {(event.bytes_total ?? 0) > 0 && (
                  <span className="text-text-faint">
                    {" "}
                    — {formatBytes(event.bytes_done ?? 0)} / {formatBytes(event.bytes_total)}
                  </span>
                )}
              </span>
              <span className="readout text-[11px] text-text-faint shrink-0">
                {Math.round(event.progress)}%
              </span>
            </div>
          ))}
        </div>
      </Card>
    </div>
  );
}
