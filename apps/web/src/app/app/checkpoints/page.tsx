"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, EmptyState, Button } from "@/components/ui";
import { LoadingState, ErrorState, MessageLine } from "@/components/state";
import { errorMessage, formatBytes, formatDateTime, timeAgo, canOperate } from "@/lib/utils";
import { migrationIsActive } from "@/lib/migration-view";
import { Save, Play, Eye, X, Loader2 } from "lucide-react";
import type { Checkpoint } from "@/lib/types";

export default function CheckpointsPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";
  const role = organization?.role;
  const queryClient = useQueryClient();
  const [message, setMessage] = useState<{ text: string; kind: "success" | "error" } | null>(null);
  const [viewing, setViewing] = useState<Checkpoint | null>(null);
  const [confirmingRestore, setConfirmingRestore] = useState<string | null>(null);
  const manifestDialogRef = useRef<HTMLDivElement>(null);
  const restoreFocusRef = useRef<HTMLElement | null>(null);

  // Focus management for the manifest dialog: move focus in on open, trap Tab,
  // close on Escape, and restore focus to the trigger on close (the same
  // contract MigrateDialog implements).
  useEffect(() => {
    if (!viewing) return;
    restoreFocusRef.current = (document.activeElement as HTMLElement) || null;
    manifestDialogRef.current?.focus();
    return () => restoreFocusRef.current?.focus?.();
  }, [viewing]);

  useEffect(() => {
    if (!viewing) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setViewing(null);
      if (event.key === "Tab" && manifestDialogRef.current) {
        const focusable = manifestDialogRef.current.querySelectorAll<HTMLElement>(
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
    const onFocusIn = (event: FocusEvent) => {
      const dialog = manifestDialogRef.current;
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
  }, [viewing]);

  const checkpointsQuery = useQuery({
    queryKey: ["checkpoints", orgId],
    queryFn: () => api.listCheckpoints(orgId),
    enabled: !!orgId,
  });

  const { data: workloads } = useQuery({
    queryKey: ["workloads", orgId],
    queryFn: () => api.listWorkloads(orgId),
    enabled: !!orgId,
  });

  // Restore targets the checkpoint's own machine — but if the workload has
  // moved (or is moving), restoring there resurrects a second copy beside the
  // live one. Both situations must warn before dispatch.
  const migrationsQuery = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
  });

  // Restore is dispatched to the checkpoint's source agent through the
  // control plane's command queue — the same path the CLI uses.
  const restore = useMutation({
    mutationFn: (checkpoint: Checkpoint) =>
      api.agentCommand(orgId, checkpoint.machine_id, {
        action: "restore",
        checkpoint_id: checkpoint.id,
        workload_id: checkpoint.workload_id,
        timeout_seconds: 120,
      }),
    onSuccess: () => {
      setMessage({ text: "Restore command dispatched to the source agent", kind: "success" });
      queryClient.invalidateQueries({ queryKey: ["workloads", orgId] });
    },
    onError: (error: unknown) =>
      setMessage({ text: errorMessage(error, "Restore failed"), kind: "error" }),
    onSettled: () => setConfirmingRestore(null),
  });

  if (checkpointsQuery.isLoading) {
    return <LoadingState label="READING CHECKPOINTS…" />;
  }

  if (checkpointsQuery.isError && !checkpointsQuery.data) {
    return (
      <div className="p-8">
        <ErrorState
          title="COULD NOT LOAD CHECKPOINTS"
          message={errorMessage(checkpointsQuery.error, "The control plane did not answer the query.")}
          onRetry={() => void checkpointsQuery.refetch()}
        />
      </div>
    );
  }

  const checkpoints = checkpointsQuery.data;
  if (!checkpoints || checkpoints.length === 0) {
    return (
      <div className="p-8">
        <EmptyState
          title="No checkpoints yet"
          description="Checkpoints capture the state of running workloads so they can be restored later."
        />
      </div>
    );
  }

  const workloadMap = new Map((workloads || []).map((w) => [w.id, w]));
  const movingWorkloads = new Set(
    (migrationsQuery.data || []).filter(migrationIsActive).map((m) => m.workload_id)
  );
  const operator = canOperate(role);

  // Group checkpoints by workload
  const grouped = checkpoints.reduce((acc, cp) => {
    if (!acc[cp.workload_id]) acc[cp.workload_id] = [];
    acc[cp.workload_id].push(cp);
    return acc;
  }, {} as Record<string, Checkpoint[]>);

  return (
    <div className="p-8 space-y-6">
      <div>
        <h1 className="text-2xl font-bold text-text mb-2">Checkpoints</h1>
        <p className="text-text-secondary">{checkpoints.length} checkpoint{checkpoints.length !== 1 ? "s" : ""}</p>
      </div>

      {message && (
        <MessageLine
          pendingLabel="DISPATCHING RESTORE…"
          message={message.text}
          pending={restore.isPending}
          success={message.kind === "success"}
        />
      )}

      <div className="space-y-6">
        {Object.entries(grouped).map(([workloadId, cps]) => {
          const workloadName = workloadMap.get(workloadId)?.name || workloadId.slice(0, 8);
          return (
            <div key={workloadId}>
              <h2 className="text-lg font-semibold text-text mb-3">{workloadName}</h2>
              <div className="space-y-2">
                {cps.map((checkpoint) => {
                  const wl = workloadMap.get(checkpoint.workload_id);
                  const moving = movingWorkloads.has(checkpoint.workload_id);
                  const moved = !!wl?.machine_id && wl.machine_id !== checkpoint.machine_id;
                  const confirmRequired = moving || moved;
                  const confirming = confirmingRestore === checkpoint.id;
                  const restoreBlockedReason = !operator
                    ? "Restoring requires the operator role"
                    : moving
                    ? "A migration is in flight for this workload — restoring now can corrupt the move"
                    : moved
                    ? "This workload now runs on another machine — restoring here creates a duplicate copy there"
                    : undefined;
                  return (
                  <Card key={checkpoint.id} className="p-4">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-4 flex-1">
                        <Save size={18} className="text-text-muted flex-shrink-0" />
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center gap-3 mb-1">
                            <span className="text-sm font-medium text-text">
                              {workloadName} · {timeAgo(checkpoint.created_at)}
                            </span>
                            <Badge variant={checkpoint.kind === "full" ? "default" : "info"}>
                              {checkpoint.kind}
                            </Badge>
                            <Badge
                              variant={
                                checkpoint.status === "available"
                                  ? "success"
                                  : checkpoint.status === "creating"
                                  ? "accent"
                                  : checkpoint.status === "corrupt"
                                  ? "error"
                                  : "default"
                              }
                            >
                              {checkpoint.status}
                            </Badge>
                          </div>
                          <div className="flex items-center gap-4 text-xs text-text-secondary">
                            <span>{formatBytes(checkpoint.stored_bytes)}</span>
                            <span>{checkpoint.chunk_count} chunks</span>
                            <span className="font-mono text-text-muted">{checkpoint.id.slice(0, 12)}</span>
                          </div>
                        </div>
                      </div>

                      <div className="flex items-center gap-2">
                        <Button
                          size="sm"
                          variant={confirming ? "danger" : "secondary"}
                          disabled={
                            checkpoint.status !== "available" ||
                            restore.isPending ||
                            !operator ||
                            (confirmRequired && !confirming)
                          }
                          title={restoreBlockedReason}
                          onClick={() => {
                            if (!operator) return;
                            if (confirmRequired && !confirming) {
                              setConfirmingRestore(checkpoint.id);
                              return;
                            }
                            restore.mutate(checkpoint);
                          }}
                        >
                          {restore.isPending && restore.variables?.id === checkpoint.id ? (
                            <Loader2 size={14} className="mr-1 animate-spin" />
                          ) : (
                            <Play size={14} className="mr-1" />
                          )}
                          {confirming ? "Confirm restore" : "Restore"}
                        </Button>
                        <Button
                          size="sm"
                          variant="ghost"
                          aria-label="View checkpoint manifest"
                          onClick={() => setViewing(checkpoint)}
                        >
                          <Eye size={14} />
                        </Button>
                      </div>
                    </div>
                  </Card>
                  );
                })}
              </div>
            </div>
          );
        })}
      </div>

      <p className="text-xs text-text-muted">
        Checkpoint deletion is not available from the dashboard: the control-plane API
        exposes only registration and listing, and checkpoint reclamation runs under the
        workload retention policy on the agent. Clone sets are likewise agent-side today
        (manage them with <span className="font-mono">shiftgate clone list</span> and
        <span className="font-mono">shiftgate clone rollback</span> against the owning
        machine) — the control plane publishes no clone-set endpoints for this view to read.
      </p>

      {viewing && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-bg/80 backdrop-blur-sm p-4"
          onClick={() => setViewing(null)}
        >
          <div
            ref={manifestDialogRef}
            tabIndex={-1}
            className="w-full max-w-lg border border-border bg-bg-surface shadow-2xl outline-none"
            onClick={(event) => event.stopPropagation()}
            role="dialog"
            aria-modal="true"
            aria-label="Checkpoint manifest"
          >
            <div className="flex items-center justify-between px-5 py-4 border-b border-border-subtle">
              <div>
                <p className="text-xs text-text-muted uppercase tracking-wider">Checkpoint</p>
                <p className="font-mono text-[13px] text-text">{viewing.id}</p>
              </div>
              <button
                onClick={() => setViewing(null)}
                className="text-text-muted hover:text-text transition-colors"
                aria-label="Close"
              >
                <X size={16} />
              </button>
            </div>
            <div className="px-5 py-4 space-y-3">
              <Row label="Workload" value={workloadMap.get(viewing.workload_id)?.name || viewing.workload_id} />
              <Row label="Machine" value={viewing.machine_id} />
              <Row label="Kind" value={viewing.kind} />
              <Row label="Status" value={viewing.status} />
              <Row label="Plain bytes" value={formatBytes(viewing.plain_bytes)} />
              <Row label="Stored bytes" value={formatBytes(viewing.stored_bytes)} />
              <Row label="Chunks" value={String(viewing.chunk_count)} />
              <Row label="Created" value={formatDateTime(viewing.created_at)} />
              {viewing.parent_id && <Row label="Parent" value={viewing.parent_id} />}
              <div>
                <p className="text-xs text-text-muted uppercase tracking-wider mb-1.5">Manifest</p>
                <pre className="text-[11px] text-text-secondary bg-bg rounded-lg border border-border p-3 overflow-x-auto">
                  {JSON.stringify(viewing.manifest, null, 2)}
                </pre>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-4">
      <span className="text-xs text-text-muted uppercase tracking-wider">{label}</span>
      <span className="font-mono text-xs text-text truncate">{value}</span>
    </div>
  );
}
