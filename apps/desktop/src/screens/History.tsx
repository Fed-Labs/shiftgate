// History — the durable record: migrations (with source/destination and
// failures), checkpoints, restores, and forks, straight from the agent's own
// records with real timestamps.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import { backend, formatBytes, formatRelative, shortID } from "@/lib/backend";
import { Badge, Card, EmptyState, Spinner } from "@/components/ui";
import { stageTone } from "@/screens/Dashboard";
import type { Migration } from "@/lib/types";

type Tab = "migrations" | "checkpoints" | "restores" | "forks";

const TABS: Array<{ id: Tab; label: string }> = [
  { id: "migrations", label: "Migrations" },
  { id: "checkpoints", label: "Checkpoints" },
  { id: "restores", label: "Restores" },
  { id: "forks", label: "Forks" },
];

export function HistoryScreen({ onOpenMigration }: { onOpenMigration: (id: string) => void }) {
  const [tab, setTab] = useState<Tab>("migrations");

  const migrations = useQuery({
    queryKey: ["agent-migrations"],
    queryFn: backend.migrations,
    refetchInterval: 5_000,
    retry: false,
  });
  const checkpoints = useQuery({
    queryKey: ["agent-checkpoints"],
    queryFn: () => backend.checkpoints(),
    refetchInterval: 10_000,
    retry: false,
  });
  const restores = useQuery({
    queryKey: ["agent-restores"],
    queryFn: () => backend.restores(),
    refetchInterval: 10_000,
    retry: false,
  });

  const loading = migrations.isLoading || checkpoints.isLoading || restores.isLoading;

  if (loading) {
    return (
      <div className="flex items-center justify-center py-32">
        <Spinner className="w-6 h-6" />
      </div>
    );
  }

  if (migrations.isError) {
    return (
      <EmptyState
        title="Cannot read history"
        description={migrations.error instanceof Error ? migrations.error.message : String(migrations.error)}
      />
    );
  }

  const migrationValues = [...(migrations.data ?? [])].sort((a, b) =>
    b.created_at.localeCompare(a.created_at)
  );
  const checkpointValues = [...(checkpoints.data ?? [])].sort((a, b) =>
    b.created_at.localeCompare(a.created_at)
  );
  const failures = migrationValues.filter(
    (m) => m.stage === "FAILED" || m.stage === "ROLLED_BACK"
  );

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold">History</h1>
        <p className="text-[13px] text-text-muted mt-0.5">
          {migrationValues.length} migrations · {checkpointValues.length} checkpoints
          {failures.length > 0 ? ` · ${failures.length} failures` : ""}
        </p>
      </div>

      {/* Tabs */}
      <div className="flex gap-1 border-b border-border">
        {TABS.map(({ id, label }) => (
          <button
            key={id}
            onClick={() => setTab(id)}
            className={clsx(
              "px-3 py-2 text-[13px] -mb-px border-b-2 transition-colors",
              tab === id
                ? "border-accent text-text"
                : "border-transparent text-text-muted hover:text-text-secondary"
            )}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "migrations" && (
        <Card className="overflow-hidden">
          {migrationValues.length === 0 ? (
            <EmptyState title="No migrations yet" description="Run one from the Workloads screen." />
          ) : (
            <div className="divide-y divide-border-subtle">
              {migrationValues.map((migration) => (
                <MigrationHistoryRow
                  key={migration.id}
                  migration={migration}
                  onOpen={() => onOpenMigration(migration.id)}
                />
              ))}
            </div>
          )}
        </Card>
      )}

      {tab === "checkpoints" && (
        <Card className="overflow-hidden">
          {checkpointValues.length === 0 ? (
            <EmptyState title="No checkpoints yet" description="Checkpoint a workload while it runs." />
          ) : (
            <div className="divide-y divide-border-subtle">
              {checkpointValues.map((checkpoint) => (
                <div key={checkpoint.id} className="flex items-center gap-4 px-4 py-3">
                  <div className="min-w-0">
                    <div className="text-[13px]">
                      <span className="readout">{shortID(checkpoint.id)}</span>
                      <span className="text-text-faint"> · {checkpoint.kind}</span>
                      {checkpoint.parent_id && (
                        <span className="text-text-faint"> · parent {shortID(checkpoint.parent_id)}</span>
                      )}
                    </div>
                    <div className="text-[11px] text-text-faint truncate">
                      {checkpoint.workload_name} · {new Date(checkpoint.created_at).toLocaleString()}
                    </div>
                  </div>
                  <div className="ml-auto text-right shrink-0">
                    <div className="readout text-[12px]">{formatBytes(checkpoint.stored_bytes)}</div>
                    <div className="text-[11px] text-text-faint">
                      {formatBytes(checkpoint.plain_bytes)} captured
                    </div>
                  </div>
                </div>
              ))}
            </div>
          )}
        </Card>
      )}

      {tab === "restores" && (
        <Card className="overflow-hidden">
          {(restores.data ?? []).length === 0 ? (
            <EmptyState
              title="No restores yet"
              description="Restore a checkpoint from a workload's inspect panel, or migrate in — the destination restores too."
            />
          ) : (
            <div className="divide-y divide-border-subtle">
              {[...(restores.data ?? [])]
                .sort((a, b) => b.created_at.localeCompare(a.created_at))
                .map((restore) => (
                  <div key={restore.id} className="flex items-center gap-4 px-4 py-3">
                    <div className="min-w-0 flex-1">
                      <div className="text-[13px]">
                        <span className="readout">{shortID(restore.id)}</span>
                        <span className="text-text-faint">
                          {" "}
                          · checkpoint {shortID(restore.checkpoint_id)}
                        </span>
                        {restore.created_workload && (
                          <span className="text-text-faint"> · created new workload</span>
                        )}
                      </div>
                      <div className="text-[11px] text-text-faint truncate">
                        {new Date(restore.created_at).toLocaleString()} · {restore.target_root}
                        {restore.pid ? ` · pid ${restore.pid}` : ""}
                        {restore.error ? ` · ${restore.error}` : ""}
                      </div>
                    </div>
                    <Badge
                      tone={
                        restore.state === "COMMITTED" || restore.state === "VALIDATED"
                          ? "healthy"
                          : restore.state === "FAILED"
                            ? "error"
                            : restore.state === "ROLLED_BACK"
                              ? "offline"
                              : "accent"
                      }
                    >
                      {restore.state}
                    </Badge>
                  </div>
                ))}
            </div>
          )}
        </Card>
      )}

      {tab === "forks" && <ForksTab />}
    </div>
  );
}

function MigrationHistoryRow({ migration, onOpen }: { migration: Migration; onOpen: () => void }) {
  return (
    <button
      onClick={onOpen}
      className="w-full flex items-center gap-4 px-4 py-3 text-left hover:bg-bg-hover transition-colors"
    >
      <div className="min-w-0 flex-1">
        <div className="text-[13px]">
          <span className="readout">{shortID(migration.workload_id)}</span>
          <span className="text-text-muted">
            {" → "}
            {migration.destination.server_name || shortID(migration.destination.machine_id)}
          </span>
        </div>
        <div className="text-[11px] text-text-faint">
          {new Date(migration.created_at).toLocaleString()} · {formatRelative(migration.created_at)} ·{" "}
          {migration.mode}
          {migration.metrics.transferred_bytes > 0 &&
            ` · ${formatBytes(migration.metrics.transferred_bytes)} transferred`}
          {migration.failure_code ? ` · ${migration.failure_code}` : ""}
        </div>
      </div>
      <Badge tone={stageTone(migration.stage)}>{migration.stage}</Badge>
    </button>
  );
}

function ForksTab() {
  const forks = useQuery({ queryKey: ["agent-forks"], queryFn: backend.forks, retry: false });

  if (forks.isLoading) {
    return (
      <Card className="p-4">
        <Spinner />
      </Card>
    );
  }

  const values = forks.data ?? [];
  if (values.length === 0) {
    return (
      <Card>
        <EmptyState
          title="No forks yet"
          description="Fork a workload to get an independent copy with its own filesystem and key namespace."
        />
      </Card>
    );
  }

  return (
    <Card className="overflow-hidden">
      <div className="divide-y divide-border-subtle">
        {values.map((fork) => {
          const record = fork as Record<string, unknown>;
          const id = typeof record.id === "string" ? record.id : "";
          const name = typeof record.name === "string" ? record.name : "fork";
          const createdAt = typeof record.created_at === "string" ? record.created_at : null;
          return (
            <div key={id} className="flex items-center gap-4 px-4 py-3">
              <div className="min-w-0">
                <div className="text-[13px]">{name}</div>
                <div className="text-[11px] text-text-faint">
                  {shortID(id)}
                  {createdAt ? ` · ${new Date(createdAt).toLocaleString()}` : ""}
                </div>
              </div>
            </div>
          );
        })}
      </div>
    </Card>
  );
}
