"use client";

import React, { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Button } from "@/components/ui";
import { cn } from "@/lib/utils";
import type { Machine, Workload } from "@/lib/types";
import { X, ArrowRight, Loader2 } from "lucide-react";

interface MigrateDialogProps {
  workload: Workload;
  machines: Machine[];
  onClose: () => void;
  onMigrated?: () => void;
}

export function MigrateDialog({ workload, machines, onClose, onMigrated }: MigrateDialogProps) {
  const queryClient = useQueryClient();
  const [destinationId, setDestinationId] = useState<string | null>(null);
  const [mode, setMode] = useState<"cold" | "live">("cold");
  const [message, setMessage] = useState<string | null>(null);
  const [agreed, setAgreed] = useState(false);

  const orgId = workload.organization_id;
  const sourceMachine = machines.find((m) => m.machine_id === workload.machine_id);
  // A migration needs a reachable agent on both ends; offline machines and
  // the source itself are not candidates.
  const candidates = machines.filter(
    (m) => m.machine_id !== workload.machine_id && m.agent_url && m.status !== "disabled" && m.status !== "offline"
  );
  // The plan decides whether live mode is offered at all: the free tier is
  // cold-only and the control plane enforces it (LIVE_MIGRATION_PLAN_REQUIRED).
  // The entitlement fetch is advisory — if it fails, the options stay open and
  // the server's refusal is the gate that shows. The effective mode is derived
  // rather than reset in an effect: a gated plan simply resolves to cold.
  const entitlementQuery = useQuery({
    queryKey: ["entitlement", orgId],
    queryFn: () => api.getEntitlement(orgId),
    staleTime: 60_000,
  });
  const liveGated = entitlementQuery.data?.plan === "free";
  const effectiveMode = liveGated ? "cold" : mode;

  const migrate = useMutation({
    mutationFn: async () => {
      if (!sourceMachine || !destinationId) throw new Error("Pick a destination first");
      const result = await api.agentCommand(orgId, sourceMachine.machine_id, {
        action: "migrate",
        workload_id: workload.id,
        destination_id: destinationId,
        mode: effectiveMode,
        timeout_seconds: 600,
      });
      return result;
    },
    onSuccess: (result) => {
      queryClient.invalidateQueries({ queryKey: ["migrations", orgId] });
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
      const stage = (result.result as { stage?: string } | undefined)?.stage;
      setMessage(
        stage
          ? `Migration started — stage ${stage}. Track it live on the Migrations page.`
          : "Migration started. Track it live on the Migrations page."
      );
      onMigrated?.();
    },
    onError: (error: unknown) => setMessage(error instanceof Error ? error.message : "Migration failed"),
  });

  const destination = machines.find((m) => m.machine_id === destinationId);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !migrate.isPending) onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [migrate.isPending, onClose]);

  const canConfirm = destinationId !== null && !migrate.isPending;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-bg/80 backdrop-blur-sm p-4"
      onClick={() => !migrate.isPending && onClose()}
    >
      <div
        className="w-full max-w-md border border-border bg-bg-surface shadow-2xl"
        onClick={(event) => event.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label={`Migrate ${workload.name}`}
      >
        {/* header */}
        <div className="flex items-center justify-between px-5 py-4 border-b border-border-subtle">
          <div>
            <p className="tlabel">move workload</p>
            <p className="font-mono text-[13px] text-text">{workload.name}</p>
          </div>
          <button
            onClick={() => !migrate.isPending && onClose()}
            className="text-text-muted hover:text-text transition-colors"
            aria-label="Close"
          >
            <X size={16} />
          </button>
        </div>

        <div className="px-5 py-4 space-y-5">
          {/* source → destination */}
          <div className="flex items-center gap-3 font-mono text-[11px]">
            <div className="flex-1 min-w-0">
              <p className="tlabel mb-1">source</p>
              <p className="text-text-secondary truncate">{sourceMachine ? sourceMachine.name : "—"}</p>
            </div>
            <ArrowRight size={14} className="text-accent shrink-0" />
            <div className="flex-1 min-w-0">
              <p className="tlabel mb-1">destination</p>
              <p className={cn("truncate", destination ? "text-text" : "text-text-muted")}>
                {destination ? destination.name : "select below"}
              </p>
            </div>
          </div>

          {/* destination list */}
          {candidates.length === 0 ? (
            <p className="text-xs text-text-muted leading-relaxed">
              No other online machines with a reachable agent. Register another machine — or bring one
              back online — to move this workload.
            </p>
          ) : (
            <div className="border border-border-subtle divide-y divide-border-subtle max-h-52 overflow-y-auto">
              {candidates.map((machine) => (
                <button
                  key={machine.machine_id}
                  onClick={() => destinationId !== machine.machine_id && setDestinationId(machine.machine_id)}
                  className={cn(
                    "w-full flex items-center justify-between px-3 py-2.5 text-left transition-colors",
                    destinationId === machine.machine_id ? "bg-accent/10" : "hover:bg-bg-hover"
                  )}
                >
                  <div className="min-w-0">
                    <p className="font-mono text-[12px] text-text truncate">{machine.name}</p>
                    <p className="font-mono text-[10px] text-text-muted truncate">
                      {machine.capabilities?.os}/{machine.capabilities?.architecture}
                      {machine.capabilities?.memory_bytes ? ` · ${Math.round(machine.capabilities.memory_bytes / 1e9)}GB` : ""}
                    </p>
                  </div>
                  <span
                    className={cn(
                      "w-3 h-3 border shrink-0 ml-3",
                      destinationId === machine.machine_id ? "bg-accent border-accent" : "border-border-strong"
                    )}
                  />
                </button>
              ))}
            </div>
          )}

          {/* mode */}
          <div>
            <p className="tlabel mb-2">mode</p>
            <div className="grid grid-cols-2 gap-2">
              <ModeOption
                selected={effectiveMode === "cold"}
                title="Cold"
                description="Stops the workload for the whole capture-and-transfer window"
                onClick={() => setMode("cold")}
              />
              <ModeOption
                selected={effectiveMode === "live"}
                title="Live"
                description="Pre-copy passes while it runs; a short freeze for the final delta"
                onClick={() => setMode("live")}
                disabled={liveGated}
                note={liveGated ? "Paid plan feature — the free tier is cold-only" : undefined}
              />
            </div>
          </div>

          {candidates.length > 0 && (
            <label className="flex items-start gap-2.5 cursor-pointer select-none">
              <input
                type="checkbox"
                checked={agreed}
                onChange={(event) => setAgreed(event.target.checked)}
                className="mt-0.5 accent-[var(--accent)]"
              />
              <span className="text-[11px] text-text-muted leading-relaxed">
                I understand the workload will be stopped on {sourceMachine?.name ?? "the source"} and
                resumes on the destination only after its state is verified there. A failed migration
                preserves the source.
              </span>
            </label>
          )}

          {message && (
            <p
              className={cn(
                "font-mono text-[11px] leading-relaxed",
                migrate.isPending ? "text-accent" : migrate.isSuccess ? "text-status-healthy" : "text-status-error"
              )}
            >
              {migrate.isPending ? "DISPATCHING MIGRATION…" : message}
            </p>
          )}
        </div>

        {/* footer */}
        <div className="flex items-center justify-end gap-2 px-5 py-4 border-t border-border-subtle">
          <Button variant="ghost" size="sm" onClick={onClose} disabled={migrate.isPending}>
            {migrate.isSuccess ? "DONE" : "CANCEL"}
          </Button>
          <Button
            variant="primary"
            size="sm"
            disabled={!canConfirm || !agreed || migrate.isPending}
            onClick={() => migrate.mutate()}
          >
            {migrate.isPending && <Loader2 size={13} className="mr-1.5 animate-spin" />}
            {effectiveMode === "live" ? "MOVE (LIVE)" : "MOVE"}
          </Button>
        </div>
      </div>
    </div>
  );
}

function ModeOption({
  selected,
  title,
  description,
  onClick,
  disabled,
  note,
}: {
  selected: boolean;
  title: string;
  description: string;
  onClick: () => void;
  disabled?: boolean;
  note?: string;
}) {
  return (
    <button
      onClick={() => !disabled && onClick()}
      disabled={disabled}
      className={cn(
        "text-left px-3 py-2.5 border transition-colors",
        selected && !disabled ? "border-accent bg-accent/5" : "border-border-subtle",
        disabled ? "opacity-50 cursor-not-allowed" : "hover:border-border-strong"
      )}
    >
      <p className={cn("font-mono text-[11px] tracking-[0.12em] mb-1", selected && !disabled ? "text-accent" : "text-text-secondary")}>
        {title.toUpperCase()}
      </p>
      <p className="text-[10px] text-text-muted leading-snug">{description}</p>
      {note && <p className="text-[10px] text-status-warning leading-snug mt-1">{note}</p>}
    </button>
  );
}


