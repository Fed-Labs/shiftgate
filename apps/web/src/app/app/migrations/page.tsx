"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, EmptyState, Button } from "@/components/ui";
import { timeAgo } from "@/lib/utils";
import { ArrowRightLeft, Loader2, XCircle } from "lucide-react";

export default function MigrationsPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";
  const queryClient = useQueryClient();
  const [message, setMessage] = useState<string | null>(null);

  const cancel = useMutation({
    mutationFn: (migrationId: string) => api.cancelMigration(orgId, migrationId),
    onSuccess: () => {
      setMessage("Migration cancellation requested");
      queryClient.invalidateQueries({ queryKey: ["migrations", orgId] });
    },
    onError: (error: unknown) => setMessage(error instanceof Error ? error.message : "Cancellation failed"),
  });


  const { data: migrations, isLoading, error } = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
  });

  const { data: machines } = useQuery({
    queryKey: ["machines", orgId],
    queryFn: () => api.listMachines(orgId),
    enabled: !!orgId,
  });

  const { data: workloads } = useQuery({
    queryKey: ["workloads", orgId],
    queryFn: () => api.listWorkloads(orgId),
    enabled: !!orgId,
  });

  if (isLoading) {
    return (
      <div className="p-8">
        <div className="text-text-secondary">Loading...</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="p-8">
        <div className="bg-status-error/10 border border-status-error/20 rounded-md px-4 py-3 text-sm text-status-error">
          Failed to load migrations
        </div>
      </div>
    );
  }

  if (!migrations || migrations.length === 0) {
    return (
      <div className="p-8">
        <EmptyState
          title="No migrations yet"
          description="Dashboard rows show reported intent. Run a migration with the CLI on its source machine."
        />
      </div>
    );
  }

  const machineMap = new Map((machines || []).map((m) => [m.id, m.name]));
  const workloadMap = new Map((workloads || []).map((w) => [w.id, w.name]));

  const sortedMigrations = [...migrations].sort(
    (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
  );

  return (
    <div className="p-8 space-y-6">
      <div>
        <h1 className="text-2xl font-bold text-text mb-2">Migrations</h1>
<p className="text-text-secondary">{migrations.length} migration{migrations.length !== 1 ? "s" : ""}</p>      </div>

      {message && (
        <div className="rounded-lg border border-status-error/20 bg-status-error/10 px-4 py-3 text-sm text-status-error">{message}</div>
      )}

      <div className="space-y-3">
        {sortedMigrations.map((migration) => {
          const sourceName = machineMap.get(migration.source_machine_id) || migration.source_machine_id.slice(0, 8);
          const destName = machineMap.get(migration.destination_machine_id) || migration.destination_machine_id.slice(0, 8);
          const workloadName = workloadMap.get(migration.workload_id) || migration.workload_id.slice(0, 8);
          const isActive = migration.status === "running" || migration.status === "queued";
          const progress = migration.progress.percent || 0;

          return (
            <Card
              key={migration.id}
              className={`p-6 cursor-pointer ${isActive ? "ring-2 ring-accent/30" : ""}`}
            >
              <div className="flex items-center justify-between">
                <div className="flex items-center gap-4 flex-1">
                  <ArrowRightLeft size={20} className="text-text-muted flex-shrink-0" />
                  <div className="flex-1 min-w-0">
                    <h3 className="text-lg font-semibold text-text mb-1">{workloadName}</h3>
                    <div className="flex items-center gap-2 text-sm text-text-secondary">
                      <span>{sourceName}</span>
                      <span className="text-text-muted">→</span>
                      <span>{destName}</span>
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
                          <span className="text-xs text-text-muted">{progress}%</span>
                        </div>
                      </div>
                    )}
                  </div>
                </div>

                <div className="flex items-center gap-3">
                  <Badge
                    variant={
                      migration.status === "completed"
                        ? "success"
                        : migration.status === "running"
                        ? "accent"
                        : migration.status === "failed"
                        ? "error"
                        : migration.status === "cancelled"
                        ? "warning"
                        : "default"
                    }
                  >
                    {migration.status}
                  </Badge>
                  {isActive && (
                    <Button size="sm" variant="danger" disabled={cancel.isPending} onClick={() => cancel.mutate(migration.id)}>
                      {cancel.isPending && cancel.variables === migration.id ? <Loader2 size={14} className="mr-1 animate-spin" /> : <XCircle size={14} className="mr-1" />}
                      Cancel
                    </Button>
                  )}
                  <span className="text-xs text-text-muted">{timeAgo(migration.created_at)}</span>
                </div>
              </div>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
