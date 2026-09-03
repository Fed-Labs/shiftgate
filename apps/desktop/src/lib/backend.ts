// Bridge to the Rust side. The Rust process owns both transports: the agent
// is reached over its Unix socket by a hand-written HTTP/1.1 client
// (src-tauri/src/http.rs — std only, no agent-specific client library), and
// the control plane over HTTPS through the official Tauri HTTP plugin. The
// WebView itself makes no direct network connections.
//
// This module is the typed surface over the generic `agent_request` command:
// every method names a real agent route and a real response type, so the
// TypeScript model of the agent API lives in exactly one place — here and in
// types.ts.

import type {
  Health,
  MachineCapabilities,
  MachineIdentity,
  DoctorResponse,
  Workload,
  CheckpointSummary,
  RestoreRecord,
  ForkRecord,
  Migration,
  Destination,
  MigrationMode,
} from "./types";

export interface CreateWorkloadInput {
  name: string;
  command: string[];
  root_path: string;
  working_dir?: string;
  environment?: Record<string, string>;
  ports?: Array<{ protocol: string; container_port: number; host_port?: number }>;
}

export interface CreateCheckpointInput {
  workload_id: string;
  kind?: "full" | "incremental";
  leave_running?: boolean;
}

export interface CreateMigrationInput {
  workload_id: string;
  destination: Destination;
  mode?: MigrationMode;
}

export interface BackendConfig {
  control_plane_url: string;
  agent_socket: string;
}

export class AgentError extends Error {
  code: string;
  status: number;
  constructor(code: string, message: string, status: number) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

declare global {
  interface Window {
    __TAURI_INTERNALS__?: {
      invoke: (cmd: string, args?: Record<string, unknown>) => Promise<unknown>;
    };
  }
}

const tauriInvoke = window.__TAURI_INTERNALS__?.invoke?.bind(window.__TAURI_INTERNALS__);

async function invoke<T>(command: string, args?: Record<string, unknown>): Promise<T> {
  if (!tauriInvoke) {
    throw new Error(
      "Desktop shell unavailable — start the app with `npm run tauri dev`, not `npm run dev`."
    );
  }
  return tauriInvoke(command, args) as Promise<T>;
}

/**
 * One request to the local agent. The Rust side performs it over the Unix
 * socket and surfaces non-2xx responses as Err("<CODE>: <message>").
 */
async function agentRequest<T>(
  method: string,
  path: string,
  body?: unknown
): Promise<T> {
  const raw = await invoke<string>("agent_request", { method, path, body });
  if (raw === "") return undefined as T;
  try {
    return JSON.parse(raw) as T;
  } catch {
    throw new AgentError("AGENT_PROTOCOL", "The agent returned a non-JSON body.", 0);
  }
}

/** A text response from the agent (logs are text/plain, not JSON). */
async function agentText(
  method: string,
  path: string,
  body?: unknown
): Promise<string> {
  return invoke<string>("agent_request", { method, path, body });
}

export const backend = {
  /* Configuration -------------------------------------------------- */
  config: () => invoke<BackendConfig>("get_config"),
  saveEndpoints: (controlPlaneURL: string, agentSocket: string) =>
    invoke<BackendConfig>("save_endpoints", { controlPlaneURL, agentSocket }),

  /* Agent — machine and health -------------------------------------- */
  health: () => agentRequest<Health>("GET", "/v1/health"),
  machine: () => agentRequest<MachineCapabilities>("GET", "/v1/machine"),
  identity: () => agentRequest<MachineIdentity>("GET", "/v1/identity"),
  doctor: () => agentRequest<DoctorResponse>("GET", "/v1/doctor"),

  /* Agent — workloads ------------------------------------------------ */
  workloads: () => agentRequest<Workload[]>("GET", "/v1/workloads"),
  workload: (id: string) => agentRequest<Workload>("GET", `/v1/workloads/${id}`),
  createWorkload: (input: CreateWorkloadInput) =>
    agentRequest<Workload>("POST", "/v1/workloads", input),
  workloadAction: (id: string, action: string) =>
    agentRequest<Workload>("POST", `/v1/workloads/${id}/${action}`),
  deleteWorkload: (id: string) =>
    agentRequest<void>("DELETE", `/v1/workloads/${id}`),
  workloadLogs: (id: string, tail: number) =>
    agentText("GET", `/v1/workloads/${id}/logs?tail=${tail}`),

  /* Agent — checkpoints, restores, forks ------------------------------ */
  checkpoints: (workloadId?: string) =>
    agentRequest<CheckpointSummary[]>(
      "GET",
      workloadId ? `/v1/checkpoints?workload_id=${encodeURIComponent(workloadId)}` : "/v1/checkpoints"
    ),
  createCheckpoint: (input: CreateCheckpointInput) =>
    agentRequest<CheckpointSummary>("POST", "/v1/checkpoints", input),
  restoreCheckpoint: (id: string, timeoutSeconds: number) =>
    agentRequest<RestoreRecord>("POST", `/v1/checkpoints/${id}/restore`, {
      timeout_seconds: timeoutSeconds,
    }),
  restores: () => agentRequest<RestoreRecord[]>("GET", "/v1/restores"),
  forks: () => agentRequest<ForkRecord[]>("GET", "/v1/forks"),
  forkWorkload: (id: string, name?: string) =>
    agentRequest<ForkRecord>("POST", `/v1/workloads/${id}/fork`, { name }),

  /* Agent — migrations ------------------------------------------------ */
  migrations: () => agentRequest<Migration[]>("GET", "/v1/migrations"),
  migration: (id: string) => agentRequest<Migration>("GET", `/v1/migrations/${id}`),
  createMigration: (input: CreateMigrationInput) =>
    agentRequest<Migration>("POST", "/v1/migrations", input),
  cancelMigration: (id: string) =>
    agentRequest<void>("POST", `/v1/migrations/${id}/cancel`),
};

/* ------------------------------------------------------------------ */
/*  Formatting helpers                                                 */
/* ------------------------------------------------------------------ */

export function formatBytes(bytes: number | undefined | null): string {
  if (bytes === undefined || bytes === null || Number.isNaN(bytes)) return "—";
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
  let value = bytes;
  let unit = "B";
  for (const next of units) {
    if (value < 1024) break;
    value /= 1024;
    unit = next;
  }
  return `${value >= 100 ? value.toFixed(0) : value.toFixed(1)} ${unit}`;
}

export function formatDuration(seconds: number | undefined | null): string {
  if (seconds === undefined || seconds === null || seconds <= 0) return "—";
  if (seconds < 1) return `${(seconds * 1000).toFixed(0)} ms`;
  if (seconds < 60) return `${seconds.toFixed(1)} s`;
  const minutes = Math.floor(seconds / 60);
  const rest = Math.round(seconds % 60);
  if (minutes < 60) return `${minutes}m ${rest}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

export function formatRelative(iso: string | undefined | null): string {
  if (!iso) return "—";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";
  const seconds = Math.round((Date.now() - then) / 1000);
  if (seconds < 5) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
}

export function shortID(id: string | undefined): string {
  if (!id) return "—";
  return id.length > 12 ? id.slice(0, 12) : id;
}
