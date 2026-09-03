"use client";

import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, EmptyState, Button } from "@/components/ui";
import { formatBytes, timeAgo } from "@/lib/utils";
import { Save, Play, Eye, Trash2 } from "lucide-react";
import type { Checkpoint } from "@/lib/types";

export default function CheckpointsPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";

  const { data: checkpoints, isLoading, error } = useQuery({
    queryKey: ["checkpoints", orgId],
    queryFn: () => api.listCheckpoints(orgId),
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
          Failed to load checkpoints
        </div>
      </div>
    );
  }

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

  const workloadMap = new Map((workloads || []).map((w) => [w.id, w.name]));

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

      <div className="space-y-6">
        {Object.entries(grouped).map(([workloadId, cps]) => {
          const workloadName = workloadMap.get(workloadId) || workloadId.slice(0, 8);
          return (
            <div key={workloadId}>
              <h2 className="text-lg font-semibold text-text mb-3">{workloadName}</h2>
              <div className="space-y-2">
                {cps.map((checkpoint) => (
                  <Card key={checkpoint.id} className="p-4">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-4 flex-1">
                        <Save size={18} className="text-text-muted flex-shrink-0" />
                        <div className="flex-1 min-w-0">
                          <div className="flex items-center gap-3 mb-1">
                            <span className="text-sm font-medium text-text">
                              {checkpoint.id.slice(0, 12)}
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
                            <span>{timeAgo(checkpoint.created_at)}</span>
                          </div>
                        </div>
                      </div>

                      <div className="flex items-center gap-2">
                        <Button size="sm" variant="secondary" disabled={checkpoint.status !== "available"}>
                          <Play size={14} className="mr-1" />
                          Restore
                        </Button>
                        <Button size="sm" variant="ghost">
                          <Eye size={14} />
                        </Button>
                        <Button size="sm" variant="ghost">
                          <Trash2 size={14} />
                        </Button>
                      </div>
                    </div>
                  </Card>
                ))}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
