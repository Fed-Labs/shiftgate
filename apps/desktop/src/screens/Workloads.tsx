// Workloads — the operating surface: create, start, pause/resume, stop,
// checkpoint, migrate, fork, delete, inspect, logs, and the periodic
// checkpoint policy. Every action is a real agent call; failures surface the
// agent's own error code and message, never a generic "something went wrong".

import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ChevronDown,
  Copy,
  Pause,
  Play,
  Plus,
  Save,
  Square,
  Trash2,
  Waypoints,
} from "lucide-react";
import clsx from "clsx";
import { backend, errorMessage, formatBytes, formatRelative, shortID } from "@/lib/backend";
import { control } from "@/lib/control";
import { useAuth } from "@/lib/store";
import { Badge, Button, Card, Dot, EmptyState, ErrorBanner, KeyValue, SectionLabel, Spinner } from "@/components/ui";
import type { Workload, WorkloadStatus } from "@/lib/types";

const WORKLOAD_TONES: Record<WorkloadStatus, "accent" | "healthy" | "warning" | "error" | "offline" | "neutral" | "info"> = {
  running: "healthy",
  starting: "accent",
  restoring: "accent",
  paused: "warning",
  checkpointed: "info",
  registered: "neutral",
  stopped: "offline",
  failed: "error",
};

function statusTone(status: WorkloadStatus) {
  return WORKLOAD_TONES[status] ?? "neutral";
}

export function WorkloadsScreen({ onMigrate }: { onMigrate: (migrationID: string) => void }) {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [migratingFor, setMigratingFor] = useState<Workload | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  const workloads = useQuery({
    queryKey: ["agent-workloads"],
    queryFn: backend.workloads,
    refetchInterval: 3_000,
    retry: false,
  });

  interface Action {
    key: string;
    invoke: () => Promise<unknown>;
  }

  const run = useMutation({
    mutationFn: async (action: Action) => action.invoke(),
    onSuccess: () => {
      setActionError(null);
      void queryClient.invalidateQueries({ queryKey: ["agent-workloads"] });
      void queryClient.invalidateQueries({ queryKey: ["agent-checkpoints"] });
    },
    onError: (error) => setActionError(error instanceof Error ? error.message : String(error)),
  });

  const perform = (key: string, invoke: () => Promise<unknown>) =>
    run.mutate({ key, invoke });

  if (workloads.isLoading) {
    return (
      <div className="flex items-center justify-center py-32">
        <Spinner className="w-6 h-6" />
      </div>
    );
  }

  if (workloads.isError) {
    return (
      <EmptyState
        title="Cannot reach the agent"
        description={workloads.error instanceof Error ? workloads.error.message : String(workloads.error)}
      />
    );
  }

  const values = workloads.data ?? [];

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold">Workloads</h1>
          <p className="text-[13px] text-text-muted mt-0.5">
            {values.length} registered · {values.filter((w) => w.status === "running").length} running
          </p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}>
          <Plus className="w-4 h-4" /> New workload
        </Button>
      </div>

      <ErrorBanner message={actionError} />

      {values.length === 0 && !creating && (
        <EmptyState
          title="No workloads on this machine"
          description="Create one with a command and a root directory. SHIFT captures the process tree and everything under its root."
          action={
            <Button variant="primary" onClick={() => setCreating(true)}>
              <Plus className="w-4 h-4" /> New workload
            </Button>
          }
        />
      )}

      {creating && <CreateWorkloadCard onClose={() => setCreating(false)} />}

      <div className="space-y-2">
        {values.map((workload) => (
          <WorkloadCard
            key={workload.spec.id}
            workload={workload}
            busy={run.isPending}
            onAction={perform}
            onMigrate={() => setMigratingFor(workload)}
          />
        ))}
      </div>

      {migratingFor && (
        <MigrateDialog
          workload={migratingFor}
          onClose={() => setMigratingFor(null)}
          onStarted={(migrationID) => {
            setMigratingFor(null);
            onMigrate(migrationID);
          }}
        />
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  One workload — summary row, actions, expandable inspect + logs      */
/* ------------------------------------------------------------------ */

function WorkloadCard({
  workload,
  busy,
  onAction,
  onMigrate,
}: {
  workload: Workload;
  busy: boolean;
  onAction: (key: string, invoke: () => Promise<unknown>) => void;
  onMigrate: () => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const spec = workload.spec;
  const id = spec.id;
  const running = workload.status === "running";

  return (
    <Card className="overflow-hidden">
      <div className="flex items-center gap-3 px-4 py-3">
        <Dot tone={statusTone(workload.status) === "healthy" ? "healthy" : statusTone(workload.status) === "error" ? "error" : "accent"} />
        <button className="flex-1 min-w-0 text-left" onClick={() => setExpanded(!expanded)}>
          <div className="flex items-center gap-2">
            <span className="text-[13px] font-medium truncate">{spec.name}</span>
            <span className="readout text-[11px] text-text-faint">{shortID(id)}</span>
          </div>
          <div className="text-[11px] text-text-faint truncate">
            {spec.command.join(" ")} · {spec.root_path}
          </div>
        </button>
        {workload.last_error && (
          <Badge tone="error" className="max-w-48 truncate">
            {workload.last_error}
          </Badge>
        )}
        <Badge tone={statusTone(workload.status)}>{workload.status}</Badge>
        <ChevronDown
          className={clsx("w-4 h-4 text-text-muted transition-transform", expanded && "rotate-180")}
        />
      </div>

      {/* Action bar */}
      <div className="flex items-center gap-1.5 px-4 py-2 border-t border-border-subtle bg-bg-elevated flex-wrap">
        {running ? (
          <Button
            variant="secondary"
            disabled={busy}
            onClick={() => onAction("pause", () => backend.workloadAction(id, "pause"))}
          >
            <Pause className="w-3.5 h-3.5" /> Pause
          </Button>
        ) : (
          <Button
            variant="secondary"
            disabled={busy}
            onClick={() => onAction("start", () => backend.workloadAction(id, "start"))}
          >
            <Play className="w-3.5 h-3.5" /> Start
          </Button>
        )}
        {workload.status === "paused" && (
          <Button
            variant="secondary"
            disabled={busy}
            onClick={() => onAction("resume", () => backend.workloadAction(id, "resume"))}
          >
            <Play className="w-3.5 h-3.5" /> Resume
          </Button>
        )}
        <Button
          variant="secondary"
          disabled={busy || !running}
          onClick={() => onAction("checkpoint", () => backend.createCheckpoint({ workload_id: id }))}
        >
          <Save className="w-3.5 h-3.5" /> Checkpoint
        </Button>
        <Button variant="secondary" disabled={busy} onClick={onMigrate}>
          <Waypoints className="w-3.5 h-3.5" /> Migrate
        </Button>
        <Button
          variant="secondary"
          disabled={busy}
          onClick={() => onAction("fork", () => backend.forkWorkload(id))}
          title="Fork: an independent copy from this workload's latest state"
        >
          <Copy className="w-3.5 h-3.5" /> Fork <span className="text-text-faint">from latest state</span>
        </Button>
        <Button
          variant="ghost"
          disabled={busy || workload.status === "stopped"}
          onClick={() => onAction("stop", () => backend.workloadAction(id, "stop"))}
        >
          <Square className="w-3.5 h-3.5" /> Stop
        </Button>
        {confirmDelete ? (
          <div className="flex items-center gap-1.5">
            <Button
              variant="danger"
              disabled={busy}
              onClick={() => onAction("delete", () => backend.deleteWorkload(id))}
            >
              <Trash2 className="w-3.5 h-3.5" /> Confirm delete
            </Button>
            <Button variant="ghost" onClick={() => setConfirmDelete(false)}>
              Cancel
            </Button>
          </div>
        ) : (
          <Button variant="ghost" onClick={() => setConfirmDelete(true)}>
            <Trash2 className="w-3.5 h-3.5" /> Delete
          </Button>
        )}
        {workload.latest_checkpoint_id && (
          <span className="ml-auto text-[11px] text-text-faint">
            latest checkpoint {shortID(workload.latest_checkpoint_id)}
          </span>
        )}
      </div>

      {expanded && <WorkloadInspect workload={workload} />}
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/*  Inspect — full spec, process state, checkpoints, live logs          */
/* ------------------------------------------------------------------ */

function WorkloadInspect({ workload }: { workload: Workload }) {
  const spec = workload.spec;
  const [logs, setLogs] = useState<string | null>(null);
  const [restoring, setRestoring] = useState<string | null>(null);
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const [confirmRestore, setConfirmRestore] = useState<string | null>(null);

  const checkpoints = useQuery({
    queryKey: ["agent-checkpoints", spec.id],
    queryFn: () => backend.checkpoints(spec.id),
    refetchInterval: 10_000,
    retry: false,
  });

  const readLogs = async () => {
    try {
      const text = await backend.workloadLogs(spec.id, 200);
      setLogs(typeof text === "string" ? text : JSON.stringify(text, null, 2));
    } catch (error) {
      setLogs(error instanceof Error ? error.message : String(error));
    }
  };

  const restore = async (checkpointID: string) => {
    setRestoring(checkpointID);
    setRestoreError(null);
    try {
      await backend.restoreCheckpoint(checkpointID, 300);
    } catch (error) {
      setRestoreError(error instanceof Error ? error.message : String(error));
    } finally {
      setRestoring(null);
    }
  };

  return (
    <div className="border-t border-border-subtle px-4 py-3 bg-bg-elevated space-y-4">
      <div className="grid md:grid-cols-2 gap-x-8">
        <div>
          <SectionLabel>Specification</SectionLabel>
          <KeyValue label="ID" value={spec.id} />
          <KeyValue label="Command" value={spec.command.join(" ")} />
          <KeyValue label="Root" value={spec.root_path} />
          <KeyValue label="Working dir" value={spec.working_dir} />
          <KeyValue label="UID / GID" value={`${spec.uid} / ${spec.gid}`} />
          <KeyValue label="Device policy" value={spec.device_policy} />
          <KeyValue label="Network policy" value={spec.network_policy} />
          <KeyValue label="Generation" value={workload.generation} />
        </div>
        <div>
          <SectionLabel>Runtime</SectionLabel>
          <KeyValue label="Status" value={workload.status} />
          <KeyValue label="PID" value={workload.process?.pid != null ? String(workload.process.pid) : "—"} />
          <KeyValue
            label="Ports"
            value={
              spec.ports && spec.ports.length > 0
                ? spec.ports
                    .map((p) => `${p.protocol} ${p.container_port}${p.host_port ? `→${p.host_port}` : ""}`)
                    .join(", ")
                : "none declared"
            }
          />
          <KeyValue
            label="Environment"
            value={
              spec.environment && Object.keys(spec.environment).length > 0
                ? `${Object.keys(spec.environment).length} variables`
                : "none"
            }
          />
          <KeyValue label="Latest checkpoint" value={workload.latest_checkpoint_id || "—"} />
          {workload.last_error && <KeyValue label="Last error" value={workload.last_error} />}
        </div>
      </div>

      {/* Checkpoints */}
      <div>
        <SectionLabel>Checkpoints</SectionLabel>
        <ErrorBanner message={restoreError} />
        {checkpoints.isError ? (
          // A failed query is not an empty history: say what went wrong.
          <p className="text-[13px] text-status-error">
            Cannot read checkpoints: {errorMessage(checkpoints.error) ?? "the agent rejected the request"}
          </p>
        ) : (checkpoints.data ?? []).length === 0 ? (
          <p className="text-[13px] text-text-muted">No checkpoints yet — use Checkpoint while the workload runs.</p>
        ) : (
          <div className="divide-y divide-border-subtle rounded-lg border border-border overflow-hidden">
            {(checkpoints.data ?? []).map((checkpoint) => (
              <div key={checkpoint.id} className="flex items-center gap-3 px-3 py-2 bg-bg-surface">
                <div className="min-w-0">
                  <div className="text-[12px]">
                    <span className="readout">{shortID(checkpoint.id)}</span>
                    <span className="text-text-faint"> · {checkpoint.kind}</span>
                  </div>
                  <div className="text-[11px] text-text-faint">
                    {new Date(checkpoint.created_at).toLocaleString()} · {formatBytes(checkpoint.plain_bytes)} state ·{" "}
                    {formatBytes(checkpoint.stored_bytes)} stored
                  </div>
                </div>
                <div className="ml-auto flex items-center gap-2">
                  {confirmRestore === checkpoint.id ? (
                    <>
                      <Button
                        variant="danger"
                        disabled={restoring === checkpoint.id}
                        onClick={() => {
                          setConfirmRestore(null);
                          void restore(checkpoint.id);
                        }}
                      >
                        {restoring === checkpoint.id
                          ? "Restoring…"
                          : `Confirm — restore ${shortID(checkpoint.id)}`}
                      </Button>
                      <Button variant="ghost" onClick={() => setConfirmRestore(null)}>
                        Cancel
                      </Button>
                    </>
                  ) : (
                    <Button
                      variant="ghost"
                      disabled={restoring === checkpoint.id}
                      onClick={() => setConfirmRestore(checkpoint.id)}
                    >
                      {restoring === checkpoint.id ? "Restoring…" : "Restore here"}
                    </Button>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
        {confirmRestore && (
          <p className="text-[12px] text-status-warning leading-relaxed mt-2">
            Restoring replaces this workload's current root state with checkpoint {shortID(confirmRestore)} —
            everything written since that checkpoint is discarded. The previous state is backed up and
            rolled back automatically if the restore fails.
          </p>
        )}
      </div>

      {/* Periodic checkpoint policy — remounted when the saved policy
          changes so a stale draft can never be written back. */}
      <CheckpointPolicyCard
        key={`${workload.spec.checkpoint_policy?.interval_seconds ?? "off"}:${
          workload.spec.checkpoint_policy?.keep_last ?? 0
        }`}
        workload={workload}
      />

      {/* Logs */}
      <div>
        <div className="flex items-center justify-between mb-2">
          <SectionLabel>Logs</SectionLabel>
          <Button variant="ghost" onClick={readLogs}>
            Read last 200 lines
          </Button>
        </div>
        {logs === null ? (
          <p className="text-[13px] text-text-muted">Logs are read on demand from the agent.</p>
        ) : (
          <pre className="terminal rounded-lg border border-border p-3 max-h-64 overflow-y-auto whitespace-pre-wrap">
            {logs}
          </pre>
        )}
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  Checkpoint policy — periodic checkpoints while the workload runs    */
/* ------------------------------------------------------------------ */

/** "10m", "30s", "1h", or a bare number (minutes) → seconds. */
function parseInterval(value: string): number {
  const trimmed = value.trim().toLowerCase();
  const match = trimmed.match(/^(\d+(?:\.\d+)?)\s*(s|m|h)?$/);
  if (!match) throw new Error(`POLICY_INTERVAL_INVALID: "${value}" — use 10m, 30s, 1h, or minutes as a number`);
  const amount = Number(match[1]);
  const unit = match[2] ?? "m";
  const seconds = Math.round(amount * (unit === "s" ? 1 : unit === "m" ? 60 : 3600));
  if (seconds < 10) throw new Error("POLICY_INTERVAL_INVALID: the minimum interval is 10 seconds");
  return seconds;
}

function formatInterval(seconds: number): string {
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
}

function CheckpointPolicyCard({ workload }: { workload: Workload }) {
  const queryClient = useQueryClient();
  const policy = workload.spec.checkpoint_policy ?? null;
  const [interval, setInterval] = useState(policy ? formatInterval(policy.interval_seconds) : "");
  const [keep, setKeep] = useState(policy?.keep_last ? String(policy.keep_last) : "");
  const [error, setError] = useState<string | null>(null);

  const save = useMutation({
    mutationFn: (off: boolean) => {
      if (off) return backend.workloadPolicy(workload.spec.id, 0, 0);
      const seconds = parseInterval(interval);
      const keepLast = keep.trim() === "" ? 0 : Number(keep);
      if (!Number.isInteger(keepLast) || keepLast < 0) {
        throw new Error("POLICY_KEEP_INVALID: keep must be a non-negative integer (0 keeps all)");
      }
      return backend.workloadPolicy(workload.spec.id, seconds, keepLast);
    },
    onSuccess: () => {
      setError(null);
      void queryClient.invalidateQueries({ queryKey: ["agent-workloads"] });
    },
    onError: (mutationError) =>
      setError(mutationError instanceof Error ? mutationError.message : String(mutationError)),
  });

  return (
    <div>
      <SectionLabel>Checkpoint policy</SectionLabel>
      <ErrorBanner message={error} />
      <p className="text-[13px] text-text-muted mb-3">
        {policy
          ? `Periodic checkpointing on — every ${formatInterval(policy.interval_seconds)}, retaining ${
              policy.keep_last ? `the last ${policy.keep_last}` : "all"
            }.`
          : "Periodic checkpointing is off — checkpoints are taken only on demand."}
      </p>
      <div className="grid md:grid-cols-2 gap-3">
        <Field label="Interval" value={interval} onChange={setInterval} placeholder="10m, 30s, or 1h" />
        <Field label="Keep last (0 keeps all)" value={keep} onChange={setKeep} placeholder="0" />
      </div>
      <div className="flex gap-2 mt-3">
        <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate(false)}>
          {save.isPending ? "Saving…" : "Save policy"}
        </Button>
        <Button variant="ghost" disabled={save.isPending || !policy} onClick={() => save.mutate(true)}>
          Disable
        </Button>
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  Create workload                                                     */
/* ------------------------------------------------------------------ */

/** Split a command line into argv, honoring shell-style quoting so paths
 * with spaces can be expressed ("python3" '/opt/my app/run.py' --flag). */
function splitCommand(line: string): string[] {
  const argv: string[] = [];
  let current = "";
  let quote: '"' | "'" | null = null;
  let started = false;
  for (const char of line) {
    if (quote) {
      if (char === quote) quote = null;
      else current += char;
      continue;
    }
    if (char === '"' || char === "'") {
      quote = char;
      started = true;
      continue;
    }
    if (char === " " || char === "\t") {
      if (started) {
        argv.push(current);
        current = "";
        started = false;
      }
      continue;
    }
    current += char;
    started = true;
  }
  if (quote) throw new Error("COMMAND_UNTERMINATED_QUOTE: close the quoted argument");
  if (started) argv.push(current);
  return argv;
}

function CreateWorkloadCard({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [command, setCommand] = useState("");
  const [rootPath, setRootPath] = useState("");
  const [workingDir, setWorkingDir] = useState("");
  const [ports, setPorts] = useState("");
  const [error, setError] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: async () => {
      const commandParts = splitCommand(command);
      if (commandParts.length === 0) throw new Error("COMMAND_INVALID: the command is empty");
      const portSpecs = ports
        .split(",")
        .map((entry) => entry.trim())
        .filter(Boolean)
        .map((entry) => {
          // "8080" publishes 8080→8080; "8080:80" publishes host 8080 → container 80.
          const match = entry.match(/^(?:(\d+):)?(\d+)$/);
          if (!match) throw new Error(`PORT_INVALID: ${entry} — use 8080 or 8080:80`);
          const hostPort = match[1] !== undefined ? Number(match[1]) : undefined;
          const containerPort = Number(match[2]);
          for (const [label, port] of [["host", hostPort], ["container", containerPort]] as const) {
            if (port !== undefined && (port < 1 || port > 65535)) {
              throw new Error(`PORT_INVALID: ${entry} — the ${label} port must be 1–65535`);
            }
          }
          return { protocol: "tcp", container_port: containerPort, ...(hostPort !== undefined ? { host_port: hostPort } : {}) };
        });
      return backend.createWorkload({
        name: name.trim(),
        command: commandParts,
        root_path: rootPath.trim(),
        ...(workingDir.trim() ? { working_dir: workingDir.trim() } : {}),
        ...(portSpecs.length > 0 ? { ports: portSpecs } : {}),
      });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["agent-workloads"] });
      onClose();
    },
    onError: (mutationError) =>
      setError(mutationError instanceof Error ? mutationError.message : String(mutationError)),
  });

  const submit = () => {
    setError(null);
    if (!name.trim() || !command.trim() || !rootPath.trim()) {
      setError("Name, command, and root path are required.");
      return;
    }
    create.mutate();
  };

  return (
    <Card className="p-4 space-y-3">
      <SectionLabel>New workload</SectionLabel>
      <ErrorBanner message={error} />
      <div className="grid md:grid-cols-2 gap-3">
        <Field label="Name" value={name} onChange={setName} placeholder="web-server" />
        <Field label="Command" value={command} onChange={setCommand} placeholder="python3 -m http.server 8080" />
        <Field label="Root path" value={rootPath} onChange={setRootPath} placeholder="/home/user/workloads/web" />
        <Field label="Working dir (optional)" value={workingDir} onChange={setWorkingDir} placeholder="defaults to the root path" />
        <Field label="Ports (optional)" value={ports} onChange={setPorts} placeholder="8080 or 8080:80, comma-separated" />
      </div>
      <p className="text-[12px] text-text-muted leading-relaxed">
        The root must be an explicit directory owned by you — SHIFT never captures an unrestricted home
        directory. Only TCP ports can be declared; they are reserved before restore and published after the
        health check passes. The command is parsed like a shell line: quote arguments that contain spaces.
      </p>
      <div className="flex gap-2">
        <Button variant="primary" disabled={create.isPending} onClick={submit}>
          {create.isPending ? "Creating…" : "Create workload"}
        </Button>
        <Button variant="ghost" onClick={onClose}>
          Cancel
        </Button>
      </div>
    </Card>
  );
}

function Field({
  label,
  value,
  onChange,
  placeholder,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
}) {
  return (
    <label className="block">
      <span className="tlabel block mb-1.5">{label}</span>
      <input value={value} onChange={(event) => onChange(event.target.value)} placeholder={placeholder} />
    </label>
  );
}

/* ------------------------------------------------------------------ */
/*  Migrate — pick a destination and a mode, then watch it run          */
/* ------------------------------------------------------------------ */

function MigrateDialog({
  workload,
  onClose,
  onStarted,
}: {
  workload: Workload;
  onClose: () => void;
  onStarted: (migrationID: string) => void;
}) {
  const { organization, tokens } = useAuth();
  const [destinationURL, setDestinationURL] = useState("");
  const [mode, setMode] = useState<"cold" | "live">("cold");
  const [error, setError] = useState<string | null>(null);
  const [preflight, setPreflight] = useState<{
    result: import("../lib/backend").PreflightResult;
    target: string;
    mode: "cold" | "live";
  } | null>(null);
  const [confirmingStart, setConfirmingStart] = useState(false);

  const machines = useQuery({
    queryKey: ["control-machines", organization?.id],
    queryFn: () => control.machines(organization!.id),
    enabled: !!organization && !!tokens,
    retry: false,
  });

  // A preflight report is valid only for the destination and mode it was
  // computed for; drop it the moment either changes so a stale green badge
  // can never vouch for a different target.
  useEffect(() => {
    setPreflight(null);
    setConfirmingStart(false);
  }, [destinationURL, mode]);

  // Dry run: the same discover/validate stages a real migration performs,
  // without freezing or moving anything — the report comes back inline.
  const check = useMutation({
    mutationFn: async (input: { target: string; mode: "cold" | "live" }) => {
      if (!input.target) throw new Error("DESTINATION_REQUIRED: pick a registered machine or enter its agent endpoint.");
      const parsed = parseEndpoint(input.target, machines.data ?? []);
      return backend.preflightMigration({
        workload_id: workload.spec.id,
        destination: {
          machine_id: parsed.machineID,
          agent_url: parsed.agentURL,
          server_name: parsed.serverName,
        },
        mode: input.mode,
      });
    },
    onSuccess: (result, input) => {
      setError(null);
      setPreflight({ result, target: input.target, mode: input.mode });
    },
    onError: (mutationError) =>
      setError(mutationError instanceof Error ? mutationError.message : String(mutationError)),
  });

  const start = useMutation({
    mutationFn: async (input: { target: string; mode: "cold" | "live" }) => {
      if (!input.target) throw new Error("DESTINATION_REQUIRED: pick a registered machine or enter its agent endpoint.");
      const parsed = parseEndpoint(input.target, machines.data ?? []);
      return backend.createMigration({
        workload_id: workload.spec.id,
        destination: {
          machine_id: parsed.machineID,
          agent_url: parsed.agentURL,
          server_name: parsed.serverName,
        },
        mode: input.mode,
      });
    },
    onSuccess: (migration) => onStarted(migration.id),
    onError: (mutationError) =>
      setError(mutationError instanceof Error ? mutationError.message : String(mutationError)),
  });

  const preflightIssues = preflight?.result.report.issues ?? [];
  const errorIssues = preflightIssues.filter((issue) => issue.severity === "error");
  const startDisabled = start.isPending || check.isPending || !destinationURL.trim();
  const beginStart = () => {
    setError(null);
    setConfirmingStart(true);
  };
  const confirmStart = () => {
    setConfirmingStart(false);
    start.mutate({ target: destinationURL.trim(), mode });
  };

  return (
    <Card className="p-4 space-y-4 border-accent/30">
      <div>
        <SectionLabel>Migrate “{workload.spec.name}”</SectionLabel>
        <p className="text-[13px] text-text-muted">
          The workload is checkpointed, transferred to the destination, verified, and restored. The source is
          preserved unless the migration commits cleanly.
        </p>
      </div>
      <ErrorBanner message={error} />

      <div>
        <span className="tlabel block mb-1.5">Destination</span>
        <div className="space-y-1.5">
          {(machines.data ?? [])
            .filter((machine) => machine.status !== "disabled")
            .map((machine) => (
              <label
                key={machine.id}
                className={clsx(
                  "flex items-center gap-3 px-3 py-2 rounded-md border cursor-pointer transition-colors",
                  destinationURL === machine.agent_url
                    ? "border-accent/40 bg-accent/10"
                    : "border-border bg-bg hover:bg-bg-hover"
                )}
              >
                <input
                  type="radio"
                  name="destination"
                  className="w-auto"
                  checked={destinationURL === machine.agent_url}
                  onChange={() => setDestinationURL(machine.agent_url)}
                />
                <span className="text-[13px]">{machine.name}</span>
                <span className="readout text-[11px] text-text-faint ml-auto truncate max-w-56">
                  {machine.agent_url}
                </span>
              </label>
            ))}
          <label className="block">
            <span className="tlabel block mb-1.5">Or an agent endpoint directly</span>
            <input
              value={destinationURL}
              onChange={(event) => setDestinationURL(event.target.value)}
              placeholder="unix:///run/shift/agent.sock · https://machine.internal:8443 · tcp://10.0.0.4:8081"
            />
          </label>
        </div>
      </div>

      <div>
        <span className="tlabel block mb-1.5">Mode</span>
        <div className="flex gap-2">
          {(["cold", "live"] as const).map((value) => (
            <button
              key={value}
              onClick={() => setMode(value)}
              className={clsx(
                "px-3 py-2 rounded-md border text-[13px] text-left transition-colors flex-1",
                mode === value ? "border-accent/40 bg-accent/10 text-text" : "border-border bg-bg text-text-secondary hover:bg-bg-hover"
              )}
            >
              <span className="block font-medium">
                {value === "cold" ? "Cold migration" : "Live migration"}
              </span>
              <span className="block text-[11px] text-text-faint mt-0.5">
                {value === "cold"
                  ? "Stop, checkpoint once, transfer, restore."
                  : "Pre-dump rounds while running; stopped only for the final checkpoint."}
              </span>
            </button>
          ))}
        </div>
      </div>

      {preflight && (
        <div className="rounded-md border border-border bg-bg p-3 space-y-2">
          <div className="flex items-center gap-2">
            <span className="tlabel block">compatibility preflight</span>
            <Badge tone={preflight.result.report.compatible ? "healthy" : "error"}>
              {preflight.result.report.compatible ? "compatible" : "issues reported"}
            </Badge>
            <span className="text-[11px] text-text-faint ml-auto truncate max-w-64">
              {preflight.target} · {preflight.mode} · checked {formatRelative(preflight.result.report.checked_at)} ·
              nothing was moved or frozen
            </span>
          </div>
          {preflightIssues.length > 0 && (
            <div className="space-y-1.5">
              {preflightIssues.map((issue, index) => (
                <div key={index} className="text-[12px]">
                  <Badge tone={issue.severity === "error" ? "error" : issue.severity === "warning" ? "warning" : "neutral"}>
                    {issue.severity}
                  </Badge>{" "}
                  <span className="text-text-secondary">{issue.resource}: {issue.description}</span>
                  {issue.adaptation && (
                    <p className="text-text-muted text-[11px] mt-0.5">Adaptation: {issue.adaptation}</p>
                  )}
                </div>
              ))}
            </div>
          )}
          {preflight.result.report.compatible && preflightIssues.length === 0 && (
            <p className="text-[12px] text-text-muted">
              No issues found. An error-severity issue would reject the migration at validate time.
            </p>
          )}
        </div>
      )}

      {confirmingStart && (
        <div className="rounded-md border border-status-warning/40 bg-status-warning/10 p-3 space-y-2">
          <p className="text-[13px] text-text-secondary">
            {mode === "cold"
              ? `Cold migration stops “${workload.spec.name}”, checkpoints it, transfers it, and restores it on the destination.`
              : `Live migration pre-dumps “${workload.spec.name}” while it runs and stops it only for the final checkpoint.`}
            {" "}The source is preserved unless the migration commits cleanly.
          </p>
          <p className="text-[12px] text-text-muted">
            {preflight === null
              ? "No compatibility preflight has been run for this destination and mode — the agent validates first and may reject the migration."
              : errorIssues.length > 0
                ? `The last preflight reported ${errorIssues.length} error-severity issue${
                    errorIssues.length === 1 ? "" : "s"
                  }; the agent will reject this migration at validate time.`
                : "The last preflight reported no blocking issues for this destination and mode."}
          </p>
          <div className="flex gap-2">
            <Button variant="danger" disabled={start.isPending} onClick={confirmStart}>
              {start.isPending ? "Starting…" : "Yes, start migration"}
            </Button>
            <Button variant="ghost" onClick={() => setConfirmingStart(false)}>
              Back
            </Button>
          </div>
        </div>
      )}

      <div className="flex gap-2">
        <Button
          variant="secondary"
          disabled={check.isPending || start.isPending || !destinationURL.trim()}
          onClick={() => check.mutate({ target: destinationURL.trim(), mode })}
        >
          {check.isPending ? "Checking…" : "Check compatibility"}
        </Button>
        <Button variant="primary" disabled={startDisabled} onClick={beginStart}>
          Start migration
        </Button>
        <Button variant="ghost" onClick={onClose}>
          Cancel
        </Button>
      </div>
    </Card>
  );
}

/**
 * Turn a picked or typed endpoint into the Destination the agent expects.
 * `machine_id` is an identity pin: the agent refuses any run whose pin does
 * not match the destination's real machine ID, and an empty pin means "no
 * pinning". For a manually entered endpoint there is no known ID, so the pin
 * stays empty (the agent writes the resolved ID back) and the endpoint string
 * doubles as the display label in `server_name`.
 */
function parseEndpoint(
  value: string,
  known: Awaited<ReturnType<typeof control.machines>>
): {
  machineID: string;
  agentURL: string;
  serverName?: string;
} {
  const trimmed = value.trim();
  const match = known.find((machine) => machine.agent_url === trimmed);
  if (match) {
    return {
      machineID: match.machine_id,
      agentURL: match.agent_url,
      serverName: match.name,
    };
  }
  return { machineID: "", agentURL: trimmed, serverName: trimmed };
}
