// Dashboard — one instrument-panel view of everything: this machine's health
// and capability, the registered fleet, live workloads, migration activity,
// checkpoint footprint, and storage.

import { useQuery } from "@tanstack/react-query";
import { backend, formatBytes, formatRelative, shortID } from "@/lib/backend";
import { control } from "@/lib/control";
import { useAuth } from "@/lib/store";
import { Badge, Card, Dot, EmptyState, ErrorBanner, KeyValue, SectionLabel, Spinner, Stat } from "@/components/ui";
import type { Migration, MigrationStage } from "@/lib/types";

const ACTIVE_STAGES: MigrationStage[] = ["CREATED", "DISCOVER", "VALIDATE", "SNAPSHOT", "PREPARE", "TRANSFER", "VERIFY", "RESTORE", "POST_VALIDATE", "SWITCH", "COMMIT", "CLEANUP", "ROLLING_BACK"];

export function stageTone(stage: MigrationStage): "accent" | "healthy" | "error" | "warning" | "offline" {
  if (stage === "COMPLETED") return "healthy";
  if (stage === "FAILED") return "error";
  if (stage === "CANCELLED") return "offline";
  if (stage === "ROLLING_BACK" || stage === "ROLLED_BACK") return "warning";
  return "accent";
}

export function DashboardScreen() {
  const { organization, tokens } = useAuth();

  const health = useQuery({
    queryKey: ["agent-health"],
    queryFn: backend.health,
    refetchInterval: 10_000,
    retry: false,
  });
  const machine = useQuery({ queryKey: ["agent-machine"], queryFn: backend.machine, retry: false });
  const doctor = useQuery({ queryKey: ["agent-doctor"], queryFn: backend.doctor, refetchInterval: 30_000, retry: false });
  const workloads = useQuery({ queryKey: ["agent-workloads"], queryFn: backend.workloads, refetchInterval: 5_000, retry: false });
  const migrations = useQuery({ queryKey: ["agent-migrations"], queryFn: backend.migrations, refetchInterval: 3_000, retry: false });
  const checkpoints = useQuery({ queryKey: ["agent-checkpoints"], queryFn: () => backend.checkpoints(), refetchInterval: 10_000, retry: false });
  const machines = useQuery({
    queryKey: ["control-machines", organization?.id],
    queryFn: () => control.machines(organization!.id),
    enabled: !!organization && !!tokens,
    refetchInterval: 15_000,
    retry: false,
  });

  if (health.isLoading || machine.isLoading) {
    return (
      <div className="flex items-center justify-center py-32">
        <Spinner className="w-6 h-6" />
      </div>
    );
  }

  if (health.isError || machine.isError) {
    return (
      <div className="max-w-md mx-auto py-24">
        <EmptyState
          title="The local agent is unreachable"
          description={
            health.error instanceof Error
              ? health.error.message
              : "Start the agent (shift-agent) and confirm its socket path in Settings."
          }
        />
      </div>
    );
  }

  const capabilities = machine.data!;
  const running = (workloads.data ?? []).filter((w) => w.status === "running");
  const active = (migrations.data ?? []).filter((m) => ACTIVE_STAGES.includes(m.stage));
  const recent = [...(migrations.data ?? [])]
    .sort((a, b) => b.created_at.localeCompare(a.created_at))
    .slice(0, 5);
  const storedBytes = (checkpoints.data ?? []).reduce((sum, c) => sum + c.stored_bytes, 0);
  const onlineMachines = (machines.data ?? []).filter((m) => m.status === "online");
  const primaryStorage = capabilities.storage.find((s) => s.mountpoint === "/") ?? capabilities.storage[0];
  const gpuSummary = capabilities.gpus.length > 0 ? `${capabilities.gpus.length}× GPU` : "None";

  return (
    <div className="space-y-6">
      {/* Machine header */}
      <div className="flex items-start justify-between">
        <div>
          <h1 className="text-xl font-semibold">{capabilities.hostname}</h1>
          <div className="text-[13px] text-text-muted mt-0.5">
            {capabilities.distribution} · {capabilities.architecture} · kernel {capabilities.kernel}
          </div>
        </div>
        <div className="flex items-center gap-2">
          <Badge tone={doctor.data?.healthy ? "healthy" : "warning"}>
            <Dot tone={doctor.data?.healthy ? "healthy" : "warning"} />
            {doctor.data?.healthy ? "Healthy" : "Attention"}
          </Badge>
          <Badge tone="neutral">agent {capabilities.agent_version}</Badge>
        </div>
      </div>

      <ErrorBanner message={migrations.error ? String(migrations.error) : null} />

      {/* Instrument row */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
        <Stat
          label="Machines"
          value={machines.data ? `${onlineMachines.length}/${machines.data.length}` : "—"}
          sub={machines.data ? "online / registered" : "control plane not connected"}
        />
        <Stat
          label="Workloads"
          value={`${running.length}/${workloads.data?.length ?? 0}`}
          sub="running / total"
        />
        <Stat
          label="Migrations"
          value={active.length > 0 ? `${active.length} active` : "Idle"}
          sub={active.length > 0 ? active.map((m) => m.stage).join(", ") : "none in flight"}
          tone={active.length > 0 ? "accent" : undefined}
        />
        <Stat label="Checkpoints" value={formatBytes(storedBytes)} sub={`${checkpoints.data?.length ?? 0} stored`} />
      </div>

      {/* Health checks */}
      {doctor.data && (
        <Card className="p-4">
          <SectionLabel>Health · doctor</SectionLabel>
          <div className="grid grid-cols-2 md:grid-cols-3 gap-x-6">
            {doctor.data.checks.map((check) => (
              <div key={check.name} className="flex items-center gap-2 py-1.5">
                <Dot tone={check.ok ? "healthy" : "error"} />
                <span className="text-[13px] text-text-secondary">{check.name}</span>
                <span className="readout text-[12px] text-text-muted ml-auto truncate max-w-32">
                  {check.message || (check.ok ? "ok" : "failed")}
                </span>
              </div>
            ))}
          </div>
        </Card>
      )}

      <div className="grid md:grid-cols-2 gap-3">
        {/* This machine */}
        <Card className="p-4">
          <SectionLabel>This machine</SectionLabel>
          <KeyValue label="Machine ID" value={shortID(capabilities.machine_id)} />
          <KeyValue label="CPU" value={`${capabilities.cpus} cores`} />
          <KeyValue label="Memory" value={formatBytes(capabilities.memory_bytes)} />
          <KeyValue
            label="Storage"
            value={
              primaryStorage
                ? `${formatBytes(primaryStorage.available_bytes)} free of ${formatBytes(primaryStorage.total_bytes)}`
                : "—"
            }
          />
          <KeyValue label="GPU" value={`${gpuSummary}${capabilities.gpus.some((g) => g.checkpoint_restore) ? " · checkpointable" : ""}`} />
          <KeyValue
            label="CRIU"
            value={capabilities.criu.healthy ? capabilities.criu.version || "healthy" : "unavailable"}
          />
          <KeyValue label="Uptime" value={health.data?.uptime ?? "—"} />
        </Card>

        {/* Device compatibility */}
        <Card className="p-4">
          <SectionLabel>Device compatibility</SectionLabel>
          {capabilities.gpus.length === 0 && (
            <p className="text-[13px] text-text-muted leading-relaxed">
              No accelerators on this machine. Workloads without GPU requirements migrate freely;
              GPU-dependent workloads report this machine as incompatible.
            </p>
          )}
          {capabilities.gpus.map((gpu, index) => (
            <div key={index} className="py-1.5 space-y-1">
              <div className="flex items-center gap-2">
                <Dot tone={gpu.checkpoint_restore ? "healthy" : "warning"} />
                <span className="text-[13px]">
                  {gpu.vendor} {gpu.model}
                </span>
                {gpu.checkpoint_restore && <Badge tone="healthy">checkpoint/restore</Badge>}
              </div>
              <KeyValue label="Driver" value={gpu.driver_version || "—"} />
              <KeyValue label="Memory" value={formatBytes(gpu.memory_bytes)} />
            </div>
          ))}
          <div className="hline my-3" />
          <div className="flex flex-wrap gap-1.5">
            {Object.entries(capabilities.features)
              .filter(([, enabled]) => enabled)
              .map(([feature]) => (
                <Badge key={feature} tone="neutral">
                  {feature}
                </Badge>
              ))}
          </div>
        </Card>
      </div>

      {/* Recent migrations */}
      <Card className="p-4">
        <SectionLabel>Recent migrations</SectionLabel>
        {recent.length === 0 ? (
          <p className="text-[13px] text-text-muted py-4">No migrations have run on this machine yet.</p>
        ) : (
          <div className="divide-y divide-border-subtle">
            {recent.map((migration) => (
              <MigrationRow key={migration.id} migration={migration} />
            ))}
          </div>
        )}
      </Card>
    </div>
  );
}

export function MigrationRow({ migration }: { migration: Migration }) {
  return (
    <div className="flex items-center gap-4 py-2.5">
      <Dot tone={stageTone(migration.stage) === "healthy" ? "healthy" : stageTone(migration.stage) === "error" ? "error" : "accent"} />
      <div className="min-w-0">
        <div className="text-[13px]">
          <span className="readout">{shortID(migration.workload_id)}</span>
          <span className="text-text-muted"> → {migration.destination.server_name || shortID(migration.destination.machine_id)}</span>
        </div>
        <div className="text-[11px] text-text-faint">
          {migration.mode} · {formatRelative(migration.created_at)}
          {migration.failure_code ? ` · failed: ${migration.failure_code}` : ""}
        </div>
      </div>
      <div className="ml-auto flex items-center gap-2">
        {migration.metrics.downtime > 0 && (
          <span className="text-[11px] text-text-muted">{formatBytes(migration.metrics.transferred_bytes)}</span>
        )}
        <Badge tone={stageTone(migration.stage)}>{migration.stage}</Badge>
      </div>
    </div>
  );
}
