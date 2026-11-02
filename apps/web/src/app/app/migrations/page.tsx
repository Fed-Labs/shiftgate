"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import Link from "next/link";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, EmptyState, Button } from "@/components/ui";
import { LoadingState, ErrorState } from "@/components/state";
import { canOperate, errorMessage, timeAgo } from "@/lib/utils";
import {
  migrationIsActive,
  migrationOutcome,
  readMigrationProgress,
  resolveMigrationRoute,
} from "@/lib/migration-view";
import { ArrowRightLeft, Loader2, XCircle } from "lucide-react";

export default function MigrationsPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";
  const role = organization?.role;
  const queryClient = useQueryClient();
  const [message, setMessage] = useState<{ text: string; kind: "success" | "error" } | null>(null);
  // Cancelling is destructive-ish and mid-transfer: one explicit confirm step,
  // scoped to the row it belongs to.
  const [confirmingCancel, setConfirmingCancel] = useState<string | null>(null);

  const cancel = useMutation({
    mutationFn: (migrationId: string) => api.cancelMigration(orgId, migrationId),
    onSuccess: () => {
      setMessage({ text: "Migration cancellation requested", kind: "success" });
      queryClient.invalidateQueries({ queryKey: ["migrations", orgId] });
    },
    onError: (error: unknown) =>
      setMessage({ text: errorMessage(error, "Cancellation failed"), kind: "error" }),
    onSettled: () => setConfirmingCancel(null),
  });

  const migrationsQuery = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
    // Fleet state must not go stale while anything is moving.
    refetchInterval: (query) =>
      (query.state.data ?? []).some((m) => migrationIsActive(m)) ? 5_000 : false,
    refetchOnWindowFocus: true,
  });

  const { data: machines, isError: machinesError } = useQuery({
    queryKey: ["machines", orgId],
    queryFn: () => api.listMachines(orgId),
    enabled: !!orgId,
  });

  const { data: workloads, isError: workloadsError } = useQuery({
    queryKey: ["workloads", orgId],
    queryFn: () => api.listWorkloads(orgId),
    enabled: !!orgId,
  });

  if (migrationsQuery.isLoading) {
    return <LoadingState label="READING MIGRATIONS…" />;
  }

  // A refetch failure with cached rows must not destroy the live view — keep
  // the table, flag it stale. Only a load with no data is a full error page.
  if (migrationsQuery.isError && !migrationsQuery.data) {
    return (
      <div className="p-8">
        <ErrorState
          title="COULD NOT LOAD MIGRATIONS"
          message={errorMessage(migrationsQuery.error, "The control plane did not answer the query.")}
          onRetry={() => void migrationsQuery.refetch()}
        />
      </div>
    );
  }

  const migrations = migrationsQuery.data;
  if (!migrations || migrations.length === 0) {
    return (
      <div className="p-8">
        <EmptyState
          title="No migrations yet"
          description="Move a workload from its detail page or the Workloads fleet (the MOVE button), or run one from the CLI on its source machine."
        />
      </div>
    );
  }

  // Migrations reference machines by their agent-side machine_id slug, not the
  // machine row UUID — key the name map that way or names never resolve.
  const machineMap = new Map((machines || []).map((m) => [m.machine_id, m.name]));
  const workloadMap = new Map((workloads || []).map((w) => [w.id, w.name]));

  const sortedMigrations = [...migrations].sort(
    (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
  );

  return (
    <div className="p-8 space-y-6">
      <div>
        <h1 className="text-2xl font-bold text-text mb-2">Migrations</h1>
        <p className="text-text-secondary">
          {migrations.length} migration{migrations.length !== 1 ? "s" : ""}
          {migrationsQuery.dataUpdatedAt > 0 && (
            <span className="text-text-muted"> · updated {new Date(migrationsQuery.dataUpdatedAt).toLocaleTimeString()}</span>
          )}
        </p>
      </div>

      {(migrationsQuery.isRefetchError || machinesError || workloadsError) && (
        <div className="rounded-lg border border-status-warning/30 bg-status-warning/10 px-4 py-3 text-sm text-status-warning">
          Showing last known data — a background refresh failed
          {machinesError || workloadsError ? " and some names resolve to ids" : ""}. It retries automatically.
        </div>
      )}

      {message && (
        <div
          className={`rounded-lg border px-4 py-3 text-sm ${
            message.kind === "success"
              ? "border-status-healthy/30 bg-status-healthy/10 text-status-healthy"
              : "border-status-error/20 bg-status-error/10 text-status-error"
          }`}
        >
          {message.text}
        </div>
      )}

      <div className="space-y-3">
        {sortedMigrations.map((migration) => {
          const sourceName = machineMap.get(migration.source_machine_id) || migration.source_machine_id.slice(0, 8);
          const destName = machineMap.get(migration.destination_machine_id) || migration.destination_machine_id.slice(0, 8);
          const workloadName = workloadMap.get(migration.workload_id) || migration.workload_id.slice(0, 8);
          const isActive = migrationIsActive(migration);
          const readouts = readMigrationProgress(migration.progress);
          const progress = readouts.percent;
          const route = resolveMigrationRoute(migration);
          const outcome = migrationOutcome(migration);
          const statusLabel =
            outcome.kind === "rolled-back"
              ? "rolled back"
              : outcome.kind === "completed-with-warning"
                ? "completed · cleanup failed"
                : migration.status;
          const statusVariant =
            outcome.kind === "completed"
              ? "success"
              : outcome.kind === "rolled-back" || outcome.kind === "cancelled" || outcome.kind === "completed-with-warning"
              ? "warning"
              : outcome.kind === "needs-operator"
              ? "error"
              : migration.status === "running"
              ? "accent"
              : "default";
          const confirming = confirmingCancel === migration.id;

          return (
            <Link key={migration.id} href={`/app/migrations/${migration.id}`} className="block">
              <Card className={`p-6 cursor-pointer hover:border-border-strong transition-colors ${isActive ? "ring-2 ring-accent/30" : ""}`}>
                <div className="flex items-center justify-between">
                  <div className="flex items-center gap-4 flex-1">
                    <ArrowRightLeft size={20} className="text-text-muted flex-shrink-0" />
                    <div className="flex-1 min-w-0">
                      <h3 className="text-lg font-semibold text-text mb-1">
                        {workloadName}
                        {route === "relay" && (
                          <span className="ml-2 align-middle inline-block rounded border border-accent/30 bg-accent/5 px-1.5 py-0.5 text-[10px] font-mono tracking-[0.12em] text-accent">
                            VIA RELAY
                          </span>
                        )}
                      </h3>
                      <div className="flex items-center gap-2 text-sm text-text-secondary">
                        <span>{sourceName}</span>
                        <span className="text-text-muted">→</span>
                        <span>{destName}</span>
                        {isActive && readouts.stage && (
                          <span className="font-mono text-[10px] tracking-[0.12em] text-text-muted">
                            {readouts.stage.toUpperCase()}
                          </span>
                        )}
                      </div>
                      {isActive && progress > 0 && (
                        <div className="mt-2">
                          <div className="flex items-center gap-2">
                            <div className="flex-1 h-1.5 bg-bg-active rounded-full overflow-hidden">
                              <div
                                className="h-full bg-accent transition-all"
                                style={{ width: `${progress}%` }}
                              />
                            </div>
                            <span className="text-xs text-text-muted">{Math.round(progress)}%</span>
                          </div>
                        </div>
                      )}
                    </div>
                  </div>

                  <div className="flex items-center gap-3">
                    <Badge variant={statusVariant}>{statusLabel}</Badge>
                    {isActive && (
                      canOperate(role) ? (
                        confirming ? (
                          <>
                            <Button
                              size="sm"
                              variant="danger"
                              disabled={cancel.isPending && cancel.variables === migration.id}
                              onClick={(event) => {
                                event.preventDefault();
                                event.stopPropagation();
                                cancel.mutate(migration.id);
                              }}
                            >
                              {cancel.isPending && cancel.variables === migration.id ? (
                                <Loader2 size={14} className="mr-1 animate-spin" />
                              ) : (
                                <XCircle size={14} className="mr-1" />
                              )}
                              Confirm
                            </Button>
                            <Button
                              size="sm"
                              variant="ghost"
                              disabled={cancel.isPending && cancel.variables === migration.id}
                              onClick={(event) => {
                                event.preventDefault();
                                event.stopPropagation();
                                setConfirmingCancel(null);
                              }}
                            >
                              Keep
                            </Button>
                          </>
                        ) : (
                          <Button
                            size="sm"
                            variant="danger"
                            onClick={(event) => {
                              // Keep the cancel button from navigating to the
                              // detail page the card links to.
                              event.preventDefault();
                              event.stopPropagation();
                              setConfirmingCancel(migration.id);
                            }}
                          >
                            <XCircle size={14} className="mr-1" />
                            Cancel
                          </Button>
                        )
                      ) : (
                        <span className="font-mono text-[10px] text-text-muted" title="Cancelling a migration requires the operator role">
                          CANCEL (OPERATOR)
                        </span>
                      )
                    )}
                    <span className="text-xs text-text-muted">{timeAgo(migration.created_at)}</span>
                  </div>
                </div>
              </Card>
            </Link>
          );
        })}
      </div>
    </div>
  );
}
