"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { MachineView, StateObject, LoadingState, ErrorState } from "@/components/state";
import { formatBytes, errorMessage } from "@/lib/utils";
import type { Machine } from "@/lib/types";

function specs(m: Machine): string[] {
  const caps = m.capabilities;
  if (!caps || !caps.memory_bytes) return ["—"];
  const out = [`${caps.cpus ?? "?"} CPU`, formatBytes(caps.memory_bytes ?? 0, 0)];
  if (caps.gpus?.length) out.push((caps.gpus[0].model ?? "GPU").split(" ").pop() ?? "GPU");
  if (caps.distribution) out.push(caps.distribution.toUpperCase());
  return out;
}

export default function MachinesPage() {
  const { organization } = useAuth();
  const orgId = organization?.id || "";

  const machinesQuery = useQuery({
    queryKey: ["machines", orgId],
    queryFn: () => api.listMachines(orgId),
    enabled: !!orgId,
    refetchInterval: 15_000,
    refetchOnWindowFocus: true,
  });
  const machines = machinesQuery.data;

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      <div className="flex items-baseline justify-between mb-10">
        <div>
          <span className="tlabel block mb-2">fleet</span>
          <h1 className="font-display text-3xl font-medium text-text">Machines</h1>
        </div>
        <span className="font-mono text-[11px] text-text-muted">
          {machinesQuery.isSuccess
            ? `${machines?.length ?? 0} registered · ${machines?.filter((m) => m.status === "online").length ?? 0} online`
            : `${machines?.length ?? 0} registered`}
        </span>
      </div>

      {machinesQuery.isLoading ? (
        <LoadingState label="READING MACHINES…" />
      ) : machinesQuery.isError ? (
        <ErrorState
          title="COULD NOT LOAD MACHINES"
          message={errorMessage(machinesQuery.error, "The control plane did not answer the query. This is a connection or session problem — not an empty fleet.")}
          onRetry={() => void machinesQuery.refetch()}
        />
      ) : !machines || machines.length === 0 ? (
        <div className="border border-border-subtle py-20 text-center">
          <p className="font-mono text-[12px] text-text-secondary mb-2">NO MACHINES REGISTERED</p>
          <p className="text-text-muted text-sm mb-6">Register a machine to start moving computation.</p>
          <Link
            href="/docs/quickstart"
            className="inline-block font-mono text-[11px] tracking-[0.16em] text-accent border border-accent/40 px-5 py-2.5 hover:bg-accent hover:text-bg transition-colors"
          >
            CONNECT A MACHINE
          </Link>
        </div>
      ) : (
        <div className="grid md:grid-cols-2 lg:grid-cols-3 gap-5">
          {machines.map((m) => {
            // The caption describes whether capabilities were ever reported —
            // a machine that reported and later went offline keeps its
            // capabilities, so key off the document, not the online flag.
            const hasCapabilities = !!m.capabilities && Object.keys(m.capabilities).length > 0;
            return (
              <Link key={m.id} href={`/app/machines/${m.id}`} className="block group">
                <MachineView
                  name={m.name.toUpperCase()}
                  specs={specs(m)}
                  status={m.status === "online" ? "online" : m.status === "draining" ? "idle" : "offline"}
                  active={m.status === "online"}
                >
                  <div className="flex items-center gap-3">
                    <StateObject
                      layers={{ process: 0.5, memory: 0.5, filesystem: 0.3, network: 0.2, device: 0.3, environment: 0.1 }}
                      active={m.status === "online"}
                      size="sm"
                      className="w-14"
                    />
                    <span className="font-mono text-[10px] text-text-muted text-left">
                      {hasCapabilities ? "capabilities reported" : "no capabilities yet"}
                    </span>
                  </div>
                </MachineView>
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}
