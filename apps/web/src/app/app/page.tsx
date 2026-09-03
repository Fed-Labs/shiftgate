"use client";

import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { MachineView, StateObject } from "@/components/state";
import { formatBytes, timeAgo } from "@/lib/utils";
import Link from "next/link";
import type { Machine, Workload } from "@/lib/types";

function workloadState(w: Workload): string {
  return ((w.status as { state?: string })?.state ?? "registered").toLowerCase();
}

export default function DashboardPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";

  const machinesQuery = useQuery({ queryKey: ["machines", orgId], queryFn: () => api.listMachines(orgId), enabled: !!orgId });
  const workloadsQuery = useQuery({ queryKey: ["workloads", orgId], queryFn: () => api.listWorkloads(orgId), enabled: !!orgId });
  const migrationsQuery = useQuery({ queryKey: ["migrations", orgId], queryFn: () => api.listMigrations(orgId), enabled: !!orgId });
  const checkpointsQuery = useQuery({ queryKey: ["checkpoints", orgId], queryFn: () => api.listCheckpoints(orgId), enabled: !!orgId });

  const machines = machinesQuery.data || [];
  const workloads = workloadsQuery.data || [];
  const migrations = migrationsQuery.data || [];
  const checkpoints = checkpointsQuery.data || [];

  const online = machines.filter((m) => m.status === "online").length;
  const running = workloads.filter((w) => workloadState(w) === "running").length;
  const active = migrations.filter((m) => m.status === "running" || m.status === "queued").length;
  const storage = checkpoints.reduce((s, c) => s + c.stored_bytes, 0);

  const loading = machinesQuery.isLoading || workloadsQuery.isLoading;

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      {/* Header row */}
      <div className="flex items-baseline justify-between mb-10">
        <div>
          <span className="tlabel block mb-2">fleet</span>
          <h1 className="font-display text-3xl font-medium text-text">Overview</h1>
        </div>
        <span className="font-mono text-[10px] text-text-faint">{organization?.name}</span>
      </div>

      {/* Instrument strip */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-px bg-border-subtle border border-border-subtle mb-12">
        <Cell label="MACHINES ONLINE" value={`${online}/${machines.length}`} />
        <Cell label="WORKLOADS RUNNING" value={`${running}`} />
        <Cell label="ACTIVE MIGRATIONS" value={`${active}`} accent={active > 0} />
        <Cell label="STATE STORED" value={formatBytes(storage)} />
      </div>

      {loading ? (
        <p className="font-mono text-[11px] text-text-muted">READING FLEET…</p>
      ) : machines.length === 0 ? (
        <EmptyFleet />
      ) : (
        <>
          {/* Machines as enclosures */}
          <div className="mb-12">
            <div className="flex items-baseline justify-between mb-4">
              <span className="tlabel">machines</span>
              <Link href="/app/machines" className="font-mono text-[10px] text-text-muted hover:text-text">ALL →</Link>
            </div>
            <div className="grid md:grid-cols-2 lg:grid-cols-3 gap-4">
              {machines.slice(0, 6).map((m) => (
                <Link key={m.id} href={`/app/machines/${m.id}`} className="block group">
                  <MachineView
                    name={m.name.toUpperCase()}
                    specs={machineSpecs(m)}
                    status={m.status === "online" ? "online" : "offline"}
                    active={m.status === "online"}
                  >
                    <span className="font-mono text-[11px] text-text-muted">
                      {workloadsOn(m.id).length} workload{workloadsOn(m.id).length === 1 ? "" : "s"}
                    </span>
                  </MachineView>
                </Link>
              ))}
            </div>
          </div>

          {/* Workloads */}
          <div>
            <div className="flex items-baseline justify-between mb-4">
              <span className="tlabel">workloads</span>
              <Link href="/app/workloads" className="font-mono text-[10px] text-text-muted hover:text-text">ALL →</Link>
            </div>
            <div className="border-t border-border-subtle">
              {workloads.slice(0, 6).map((w) => {
                const st = workloadState(w);
                return (
                  <Link
                    key={w.id}
                    href={`/app/workloads/${w.id}`}
                    className="grid grid-cols-[1fr_auto_auto] md:grid-cols-[1.4fr_1fr_auto_auto] gap-4 items-center py-3 border-b border-border-subtle hover:bg-bg-elevated transition-colors px-2"
                  >
                    <div className="flex items-center gap-3 min-w-0">
                      <StateObject layers={{ process: 0.6, memory: 0.5, filesystem: 0.3, network: 0.2, device: 0.3, environment: 0.1 }} active={st === "running"} size="sm" className="w-10 shrink-0" />
                      <span className="font-mono text-[12px] text-text truncate">{w.name}</span>
                    </div>
                    <span className="hidden md:block font-mono text-[10px] text-text-muted">{w.machine_id ? w.machine_id.slice(0, 8) : "—"}</span>
                    <span className="font-mono text-[10px] text-text-muted">{timeAgo(w.updated_at)}</span>
                    <span className={`font-mono text-[10px] tracking-[0.12em] ${st === "running" ? "text-status-healthy" : "text-text-muted"}`}>
                      {st.toUpperCase()}
                    </span>
                  </Link>
                );
              })}
            </div>
          </div>
        </>
      )}
    </div>
  );

  function workloadsOn(machineId: string) {
    return workloads.filter((w) => w.machine_id === machineId);
  }
}

function machineSpecs(m: Machine): string[] {
  const caps = m.capabilities;
  if (!caps || !caps.memory_bytes) return [m.agent_url ? "AGENT" : "—"];
  const out = [`${caps.cpus ?? "?"} CPU`, formatBytes(caps.memory_bytes ?? 0, 0)];
  if (caps.gpus?.length) out.push(caps.gpus[0].model?.split(" ").pop() ?? "GPU");
  return out;
}

function Cell({ label, value, accent = false }: { label: string; value: string; accent?: boolean }) {
  return (
    <div className="bg-bg px-5 py-4">
      <div className="tlabel mb-2">{label}</div>
      <div className={`readout text-2xl ${accent ? "text-accent" : "text-text"}`}>{value}</div>
    </div>
  );
}

function EmptyFleet() {
  return (
    <div className="border border-border-subtle py-20 text-center">
      <p className="font-mono text-[12px] text-text-secondary mb-2">NO MACHINES CONNECTED</p>
      <p className="text-text-muted text-sm mb-6">Connect a machine to start moving computation.</p>
      <Link
        href="/docs/quickstart"
        className="inline-block font-mono text-[11px] tracking-[0.16em] text-accent border border-accent/40 px-5 py-2.5 hover:bg-accent hover:text-bg transition-colors"
      >
        CONNECT A MACHINE
      </Link>
    </div>
  );
}
