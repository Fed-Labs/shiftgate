"use client";

import React from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Cpu,
  MemoryStick,
  HardDrive,
  ChevronRight,
  Loader2,
  MoreVertical,
  Box,
  Activity,
} from "lucide-react";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { MachineCapabilities } from "@/lib/types";
import { Container, Card, Badge, Mono, Button, StatusDot, EmptyState } from "@/components/ui";
import { formatBytes, timeAgo, formatMemory } from "@/lib/utils";
import Link from "next/link";

/* ------------------------------------------------------------------ */
/*  Helpers                                                            */
/* ------------------------------------------------------------------ */

function machineStatusVariant(status: string): "default" | "success" | "warning" | "error" | "info" {
  switch (status) {
    case "online": return "success";
    case "offline": return "error";
    case "draining": return "warning";
    case "disabled": return "default";
    default: return "default";
  }
}

function statusDotStatus(status: string): "online" | "offline" | "running" | "error" | "warning" {
  switch (status) {
    case "online": return "online";
    case "offline": return "offline";
    case "draining": return "warning";
    case "disabled": return "error";
    default: return "offline";
  }
}

function workloadStatusVariant(status: string): "default" | "success" | "warning" | "error" | "info" | "accent" {
  switch (status) {
    case "running": return "success";
    case "starting":
    case "restoring": return "info";
    case "paused":
    case "checkpointed": return "warning";
    case "stopped":
    case "failed": return "error";
    default: return "default";
  }
}

function migrationStatusVariant(status: string): "default" | "success" | "warning" | "error" | "info" | "accent" {
  switch (status) {
    case "completed": return "success";
    case "running": return "accent";
    case "queued": return "info";
    case "failed": return "error";
    case "cancelled": return "warning";
    default: return "default";
  }
}

function isMachineCaps(c: unknown): c is MachineCapabilities {
  if (!c || typeof c !== "object" || !("hostname" in c)) return false;
  const candidate = c as { gpus?: unknown; storage?: unknown; criu?: { healthy?: unknown } };
  return (
    Array.isArray(candidate.gpus) &&
    Array.isArray(candidate.storage) &&
    !!candidate.criu &&
    typeof candidate.criu === "object" &&
    typeof candidate.criu.healthy === "boolean"
  );
}

function getWorkloadStatus(w: { status: Record<string, unknown> }): string {
  const s = w.status;
  if (typeof s === "string") return s;
  return String(s?.state || "registered");
}

/* ------------------------------------------------------------------ */
/*  Info Row                                                           */
/* ------------------------------------------------------------------ */

function InfoRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between py-2.5">
      <span className="text-sm text-text-muted">{label}</span>
      <div className="text-sm text-text">{children}</div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function MachineDetailPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = React.use(params);
  const orgId = useAuth((s) => s.organization?.id || "");
  const [showActions, setShowActions] = React.useState(false);

  const { data: machines, isLoading, error, refetch } = useQuery({
    queryKey: ["machines", orgId],
    queryFn: () => api.listMachines(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  });

  const { data: workloads } = useQuery({
    queryKey: ["workloads", orgId],
    queryFn: () => api.listWorkloads(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  });

  const { data: migrations } = useQuery({
    queryKey: ["migrations", orgId],
    queryFn: () => api.listMigrations(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  });

  if (isLoading) {
    return (
      <Container>
        <div className="flex items-center justify-center py-32">
          <Loader2 className="w-6 h-6 text-accent animate-spin" />
        </div>
      </Container>
    );
  }

  if (error && !machines) {
    return (
      <Container>
        <EmptyState
          title="Failed to load machine"
          description="The control plane did not answer the query. This is a connection or session problem — not a missing machine."
        />
        <div className="flex justify-center mt-4">
          <Button variant="secondary" onClick={() => void refetch()}>Retry</Button>
        </div>
      </Container>
    );
  }

  // The route param may be the row UUID (list links) or the agent's
  // machine_id slug (migrations and workloads reference machines that way) —
  // accept both, and key every child lookup by the resolved machine's
  // machine_id, which is what workloads and migrations carry.
  const machine = machines?.find((m) => m.id === id || m.machine_id === id);
  if (!machine) {
    return (
      <Container>
        <EmptyState title="Machine not found" description="This machine does not exist or has been removed." />
      </Container>
    );
  }

  const caps = isMachineCaps(machine.capabilities) ? machine.capabilities : null;
  const machineWorkloads = workloads?.filter((w) => w.machine_id === machine.machine_id) || [];
  const machineMigrations = migrations?.filter(
    (m) => m.source_machine_id === machine.machine_id || m.destination_machine_id === machine.machine_id
  ) || [];

  return (
    <Container>
      {/* Header */}
      <div className="flex items-start justify-between gap-4 mb-8">
        <div>
          <div className="flex items-center gap-3 mb-2">
            <Link href="/app/machines" className="text-text-muted hover:text-text-secondary text-sm transition-colors">
              Machines
            </Link>
            <ChevronRight className="w-3 h-3 text-text-muted" />
            <Mono className="text-sm text-text-muted">{machine.machine_id.slice(0, 8)}</Mono>
          </div>
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-semibold text-text">{machine.name}</h1>
            <div className="flex items-center gap-1.5">
              <StatusDot status={statusDotStatus(machine.status)} />
              <Badge variant={machineStatusVariant(machine.status)}>{machine.status}</Badge>
            </div>
          </div>
          <p className="text-sm text-text-muted mt-1">
            Last seen {machine.last_seen_at ? timeAgo(machine.last_seen_at) : "never"}
          </p>
        </div>

        <div
          className="relative"
          onKeyDown={(e) => {
            if (e.key === "Escape") setShowActions(false);
          }}
        >
          <Button
            variant="secondary"
            size="sm"
            aria-label="Machine actions"
            aria-haspopup="true"
            aria-expanded={showActions}
            onClick={() => setShowActions(!showActions)}
          >
            <MoreVertical className="w-4 h-4" />
          </Button>
          {showActions && (
            <>
              <div className="fixed inset-0 z-40" onClick={() => setShowActions(false)} />
              <div className="absolute right-0 top-full mt-1 z-50 w-72 bg-bg-elevated border border-border rounded-lg shadow-xl p-3">
                <p className="text-xs text-text-secondary leading-relaxed">
                  Draining and removing machines is done from the machine itself — the agent
                  reports <Mono className="text-[10px]">draining</Mono> status via its heartbeat and deregistration
                  runs on the host. The control-plane API has no machine mutation endpoints, so
                  these actions are not offered here.
                </p>
              </div>
            </>
          )}
        </div>
      </div>

      {/* System Info + Capabilities */}
      <div className="grid md:grid-cols-2 gap-4 mb-6">
        <Card className="p-5">
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">System</h2>
          <div className="divide-y divide-border">
            <InfoRow label="Machine ID"><Mono className="text-xs">{machine.machine_id}</Mono></InfoRow>
            {caps && (
              <>
                <InfoRow label="Hostname"><Mono>{caps.hostname}</Mono></InfoRow>
                <InfoRow label="OS">
                  {caps.distribution || caps.os}
                  {caps.kernel ? <span className="text-text-muted"> ({caps.kernel})</span> : null}
                </InfoRow>
                <InfoRow label="Architecture"><Mono>{caps.architecture}</Mono></InfoRow>
              </>
            )}
            <InfoRow label="Agent URL"><Mono className="text-xs">{machine.agent_url}</Mono></InfoRow>
            <InfoRow label="Created">{timeAgo(machine.created_at)}</InfoRow>
          </div>
        </Card>

        <Card className="p-5">
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">Hardware</h2>
          {caps ? (
            <div className="space-y-3">
              <div className="flex items-center gap-3">
                <div className="flex items-center justify-center w-8 h-8 rounded-lg bg-bg-surface border border-border">
                  <Cpu className="w-4 h-4 text-text-muted" />
                </div>
                <div>
                  <p className="text-sm text-text">{caps.cpus} CPU{caps.cpus !== 1 ? "s" : ""}</p>
                  <p className="text-xs text-text-muted">{caps.architecture}</p>
                </div>
              </div>

              <div className="flex items-center gap-3">
                <div className="flex items-center justify-center w-8 h-8 rounded-lg bg-bg-surface border border-border">
                  <MemoryStick className="w-4 h-4 text-text-muted" />
                </div>
                <div>
                  <p className="text-sm text-text">{formatMemory(caps.memory_bytes)}</p>
                  <p className="text-xs text-text-muted">RAM</p>
                </div>
              </div>

              {caps.gpus.length > 0 && (
                <div className="flex items-center gap-3">
                  <div className="flex items-center justify-center w-8 h-8 rounded-lg bg-bg-surface border border-border">
                    <Activity className="w-4 h-4 text-text-muted" />
                  </div>
                  <div>
                    <p className="text-sm text-text">{caps.gpus.length} GPU{caps.gpus.length !== 1 ? "s" : ""}</p>
                    <p className="text-xs text-text-muted">
                      {caps.gpus.map((g) => `${g.vendor} ${g.model}`).join(", ")}
                    </p>
                  </div>
                </div>
              )}

              {caps.storage.length > 0 && (
                <div className="flex items-center gap-3">
                  <div className="flex items-center justify-center w-8 h-8 rounded-lg bg-bg-surface border border-border">
                    <HardDrive className="w-4 h-4 text-text-muted" />
                  </div>
                  <div>
                    <p className="text-sm text-text">
                      {caps.storage.map((s) => formatBytes(s.total_bytes)).join(", ")}
                    </p>
                    <p className="text-xs text-text-muted">
                      {caps.storage.map((s) => s.mountpoint).join(", ")}
                    </p>
                  </div>
                </div>
              )}

              {caps.agent_version && (
                <div className="flex items-center gap-3">
                  <div className="flex items-center justify-center w-8 h-8 rounded-lg bg-bg-surface border border-border">
                    <Box className="w-4 h-4 text-text-muted" />
                  </div>
                  <div>
                    <p className="text-sm text-text">SHIFT v{caps.agent_version}</p>
                    <p className="text-xs text-text-muted">Agent</p>
                  </div>
                </div>
              )}
            </div>
          ) : (
            <EmptyState title="No capabilities" description="Machine has not reported capabilities yet." />
          )}
        </Card>
      </div>

      {/* GPU Details */}
      {caps && caps.gpus.length > 0 && (
        <Card className="p-5 mb-6">
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">GPU Details</h2>
          <div className="grid gap-3">
            {caps.gpus.map((gpu, i) => (
              <div key={i} className="flex items-center justify-between p-3 bg-bg-surface rounded-lg border border-border">
                <div>
                  <p className="text-sm font-medium text-text">{gpu.vendor} {gpu.model}</p>
                  <div className="flex items-center gap-3 mt-1">
                    {gpu.compute_capability && (
                      <span className="text-xs text-text-muted">Compute {gpu.compute_capability}</span>
                    )}
                    {gpu.runtime && (
                      <span className="text-xs text-text-muted">{gpu.runtime}</span>
                    )}
                    {gpu.driver_version && (
                      <span className="text-xs text-text-muted">Driver {gpu.driver_version}</span>
                    )}
                  </div>
                </div>
                <div className="text-right">
                  <Mono className="text-sm text-text">{gpu.memory_bytes != null ? formatBytes(gpu.memory_bytes) : "—"}</Mono>
                  <p className="text-xs text-text-muted">Memory</p>
                </div>
              </div>
            ))}
          </div>
        </Card>
      )}

      {/* CRIU */}
      {caps && (
        <Card className="p-5 mb-6">
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">CRIU</h2>
          <div className="flex items-center gap-3">
            <Badge variant={caps.criu.healthy ? "success" : "error"}>
              {caps.criu.healthy ? "Healthy" : "Unhealthy"}
            </Badge>
            {caps.criu.version ? <Mono className="text-sm text-text-muted">v{caps.criu.version}</Mono> : null}
            {caps.criu.features ? (
              <div className="flex gap-1.5 flex-wrap">
                {caps.criu.features.map((f) => (
                  <Mono key={f} className="text-xs bg-bg-surface px-2 py-0.5 rounded border border-border">{f}</Mono>
                ))}
              </div>
            ) : null}
          </div>
          {caps.criu.errors && caps.criu.errors.length > 0 && (
            <div className="mt-3 p-3 bg-status-error/5 border border-status-error/20 rounded-lg">
              {caps.criu.errors.map((e, i) => (
                <p key={i} className="text-xs text-status-error">{e}</p>
              ))}
            </div>
          )}
        </Card>
      )}

      {/* Workloads */}
      <Card className="p-5 mb-6">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">
          Workloads ({machineWorkloads.length})
        </h2>
        {machineWorkloads.length === 0 ? (
          <EmptyState title="No workloads" description="No workloads are running on this machine." />
        ) : (
          <div className="space-y-1">
            {machineWorkloads.map((w) => {
              const wStatus = getWorkloadStatus(w);
              return (
                <Link key={w.id} href={`/app/workloads/${w.id}`}>
                  <div className="flex items-center justify-between py-2.5 px-3 rounded-lg hover:bg-bg-hover transition-colors cursor-pointer">
                    <div className="flex items-center gap-3">
                      <StatusDot status={wStatus === "running" ? "running" : wStatus === "stopped" ? "offline" : "warning"} />
                      <span className="text-sm font-medium text-text">{w.name}</span>
                      <Badge variant={workloadStatusVariant(wStatus)}>{wStatus}</Badge>
                    </div>
                    <Mono className="text-xs text-text-muted">{w.id.slice(0, 8)}</Mono>
                  </div>
                </Link>
              );
            })}
          </div>
        )}
      </Card>

      {/* Recent Migrations */}
      <Card className="p-5 mb-12">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">
          Recent Migrations ({machineMigrations.length})
        </h2>
        {machineMigrations.length === 0 ? (
          <EmptyState title="No migrations" description="No migrations involve this machine." />
        ) : (
          <div className="space-y-1">
            {machineMigrations.slice(0, 10).map((m) => {
              const isSource = m.source_machine_id === machine.machine_id;
              return (
                <Link key={m.id} href={`/app/migrations/${m.id}`}>
                  <div className="flex items-center justify-between py-2.5 px-3 rounded-lg hover:bg-bg-hover transition-colors cursor-pointer">
                    <div className="flex items-center gap-3">
                      <Badge variant={migrationStatusVariant(m.status)}>{m.status}</Badge>
                      <span className="text-xs text-text-muted">
                        {isSource ? "From this machine" : "To this machine"}
                      </span>
                      <Mono className="text-xs text-text-muted">{m.id.slice(0, 8)}</Mono>
                    </div>
                    <span className="text-xs text-text-muted">{timeAgo(m.created_at)}</span>
                  </div>
                </Link>
              );
            })}
          </div>
        )}
      </Card>
    </Container>
  );
}
