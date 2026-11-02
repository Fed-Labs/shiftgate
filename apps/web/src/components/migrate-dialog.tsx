"use client";

import React, { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { Button } from "@/components/ui";
import { apiErrorCode, apiErrorStatus, cn, errorMessage, isSubscriptionActive } from "@/lib/utils";
import { rememberMigrationRoute } from "@/lib/migration-view";
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
  const [route, setRoute] = useState<"direct" | "relay">("direct");
  const [message, setMessage] = useState<string | null>(null);
  const [relayNote, setRelayNote] = useState<string | null>(null);
  const [agreed, setAgreed] = useState(false);
  const dialogRef = useRef<HTMLDivElement>(null);
  const restoreFocusRef = useRef<HTMLElement | null>(null);

  const orgId = workload.organization_id;
  const sourceMachine = machines.find((m) => m.machine_id === workload.machine_id);
  // A migration needs a reachable agent on both ends; the source itself and
  // anything not fully online — including draining/decommissioning machines —
  // are not candidates. Moving onto a machine that is going away guarantees
  // an immediate second move later.
  const candidates = machines.filter(
    (m) => m.machine_id !== workload.machine_id && m.agent_url && m.status === "online"
  );
  // The plan decides whether live mode is offered at all: the free tier is
  // cold-only and the control plane enforces it (LIVE_MIGRATION_PLAN_REQUIRED).
  // The gate mirrors LiveMigrationAllowed — live needs a plan with the feature
  // AND a currently active subscription; a lapsed/cancelled/past-due-less one
  // falls back to cold-only even on a paid plan. The entitlement fetch is
  // advisory — if it fails, the options stay open and the server's refusal is
  // the gate that shows. The effective mode is derived rather than reset in an
  // effect: a gated plan simply resolves to cold.
  const entitlementQuery = useQuery({
    queryKey: ["entitlement", orgId],
    queryFn: () => api.getEntitlement(orgId),
    staleTime: 60_000,
  });
  const liveGated = entitlementQuery.data
    ? entitlementQuery.data.plan === "free" || !isSubscriptionActive(entitlementQuery.data.status)
    : false;
  const effectiveMode = liveGated ? "cold" : mode;

  const migrate = useMutation({
    mutationFn: async () => {
      if (!sourceMachine) {
        throw new Error(
          "Source machine not found for this workload — its machine may be stale or removed. Refresh the machine list and try again."
        );
      }
      if (!destinationId) throw new Error("Pick a destination first");
      // Every dashboard move is a platform-mediated job: the managed
      // migration worker polls the agent and keeps the job's status, stage,
      // progress, and event log current until it settles. The old direct
      // agent-command path created an unmanaged row nothing ever updated — a
      // wedged "running" entry whose stale active state also blocked every
      // later move of the workload.
      return api.createMigration(orgId, {
        workload_id: workload.id,
        source_machine_id: sourceMachine.machine_id,
        destination_machine_id: destinationId,
        mode: effectiveMode,
        route,
        timeout_seconds: 600,
      });
    },
    onSuccess: (created) => {
      queryClient.invalidateQueries({ queryKey: ["migrations", orgId] });
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
      // The create response is the only place the record carries its route —
      // remember it so a later RETRY reproduces the route instead of silently
      // switching. The one-use relay ticket is stripped at the api boundary
      // and never reaches this layer or the query cache.
      rememberMigrationRoute(created.id, route, created.relay_endpoint);
      if (route === "relay" || created.relay_endpoint) {
        const endpoint = created.relay_endpoint || "the configured relay";
        setMessage(`Relay migration started — state will traverse ${endpoint}. Track it live on the Migrations page.`);
      } else {
        setMessage("Migration started. Track it live on the Migrations page.");
      }
      onMigrated?.();
    },
    onError: (error: unknown) => {
      const status = apiErrorStatus(error);
      const code = apiErrorCode(error) || "";
      if (status === 503 || code.startsWith("RELAY")) {
        // The relay leg is only armed where a relay is configured (the
        // default deployment has none). The server settles the job row as
        // failed when ticket issue fails, so nothing half-created is left —
        // say so and take the route off the table for this dialog.
        setRelayNote(
          `${errorMessage(error, "Relay is not configured on this deployment.")} The migration job was settled as failed — nothing is in flight. Use the direct route, or see the quickstart's relay section to enable one.`
        );
        setRoute("direct");
      }
      setMessage(errorMessage(error, "Migration failed"));
    },
  });

  const destination = machines.find((m) => m.machine_id === destinationId);

  useEffect(() => {
    restoreFocusRef.current = (document.activeElement as HTMLElement) || null;
    dialogRef.current?.focus();
    return () => restoreFocusRef.current?.focus?.();
  }, []);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !migrate.isPending) onClose();
      if (event.key === "Tab" && dialogRef.current) {
        // Keep keyboard focus inside the dialog while it is open.
        const focusable = dialogRef.current.querySelectorAll<HTMLElement>(
          'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), audio[controls], video[controls], [href], [tabindex]:not([tabindex="-1"])'
        );
        if (focusable.length === 0) return;
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        const active = document.activeElement;
        if (event.shiftKey && active === first) {
          event.preventDefault();
          last.focus();
        } else if (!event.shiftKey && active === last) {
          event.preventDefault();
          first.focus();
        }
      }
    };
    // If focus escapes the dialog (browser chrome, programmatic moves), pull
    // it back so the trap is not silently one-directional.
    const onFocusIn = (event: FocusEvent) => {
      const dialog = dialogRef.current;
      if (!dialog) return;
      if (event.target instanceof Node && !dialog.contains(event.target)) {
        dialog.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    document.addEventListener("focusin", onFocusIn);
    return () => {
      window.removeEventListener("keydown", onKey);
      document.removeEventListener("focusin", onFocusIn);
    };
  }, [migrate.isPending, onClose]);

  // After a successful dispatch the dialog may stay mounted (onMigrated is
  // optional): never re-arm MOVE — one extra click must not dispatch a second,
  // duplicate migration.
  const canConfirm = destinationId !== null && !migrate.isPending && !migrate.isSuccess;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-bg/80 backdrop-blur-sm p-4"
      onClick={() => !migrate.isPending && onClose()}
    >
      <div
        ref={dialogRef}
        tabIndex={-1}
        className="w-full max-w-md border border-border bg-bg-surface shadow-2xl outline-none"
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
            type="button"
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
            <div
              role="radiogroup"
              aria-label="Destination machine"
              className="border border-border-subtle divide-y divide-border-subtle max-h-52 overflow-y-auto"
            >
              {candidates.map((machine) => (
                <button
                  type="button"
                  key={machine.machine_id}
                  role="radio"
                  aria-checked={destinationId === machine.machine_id}
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
            <div role="radiogroup" aria-label="Migration mode" className="grid grid-cols-2 gap-2">
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
                note={
                  liveGated
                    ? "Paid plan feature — needs the free-tier upgrade or an active subscription"
                    : undefined
                }
              />
            </div>
          </div>

          {/* route */}
          <div>
            <p className="tlabel mb-2">route</p>
            <div className="grid grid-cols-2 gap-2">
              <ModeOption
                selected={route === "direct"}
                title="Direct"
                description="Agents transfer state directly to each other over mutually authenticated TLS"
                onClick={() => setRoute("direct")}
              />
              <ModeOption
                selected={route === "relay"}
                title="Relay"
                description="State traverses a control-plane-issued relay ticket when the agents cannot reach each other directly"
                onClick={() => setRoute("relay")}
                disabled={relayNote !== null}
                note={
                  relayNote
                    ? "Unavailable on this deployment"
                    : "Needs a configured relay — the default deployment has none"
                }
              />
            </div>
            {relayNote && (
              <p className="text-[10px] text-status-warning leading-snug mt-2">{relayNote}</p>
            )}
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
                {effectiveMode === "live"
                  ? `I understand the workload is frozen briefly on ${sourceMachine?.name ?? "the source"} for the final checkpoint and resumes on the destination only after its state is verified there. A failed migration preserves the source.`
                  : `I understand the workload will be stopped on ${sourceMachine?.name ?? "the source"} for the capture-and-transfer window and resumes on the destination only after its state is verified there. A failed migration preserves the source.`}
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
            {effectiveMode === "live" ? "MOVE (LIVE)" : route === "relay" ? "MOVE (RELAY)" : "MOVE"}
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
      type="button"
      role="radio"
      aria-checked={selected && !disabled}
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


