import type { MigrationJob, MigrationProgress } from "./types";

// Read view over the migration progress document. The control plane's
// contract (dispatcher + managed worker) emits:
//   current_stage (string)          — legacy alias: stage
//   percent (0..100)                — legacy: progress (0..1 fraction)
//   downtime_ms (int, ms)           — measured freeze window
//   transfer_speed (float, bytes/s)
//   deduplicated_bytes / transferred_bytes / total_state_bytes (int)
// The legacy keys are still read so rows written before the contract landed
// keep rendering.
export interface MigrationReadouts {
  stage: string;
  percent: number;
  downtimeMs?: number;
  transferSpeed?: number;
  deduplicatedBytes?: number;
  transferredBytes?: number;
  totalStateBytes?: number;
}

function numberField(source: Record<string, unknown>, key: string): number | undefined {
  const value = source[key];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

export function readMigrationProgress(progress: MigrationProgress | undefined): MigrationReadouts {
  const doc: Record<string, unknown> = (progress as Record<string, unknown>) ?? {};
  const stage =
    (typeof doc.current_stage === "string" && doc.current_stage) ||
    (typeof doc.stage === "string" && doc.stage) ||
    "";
  const percentField = numberField(doc, "percent");
  const fractionField = numberField(doc, "progress");
  const percent =
    percentField !== undefined
      ? Math.min(100, Math.max(0, percentField))
      : fractionField !== undefined
        ? Math.min(100, Math.max(0, fractionField * 100))
        : 0;
  return {
    stage,
    percent,
    downtimeMs: numberField(doc, "downtime_ms"),
    transferSpeed: numberField(doc, "transfer_speed"),
    deduplicatedBytes: numberField(doc, "deduplicated_bytes"),
    transferredBytes: numberField(doc, "transferred_bytes"),
    totalStateBytes: numberField(doc, "total_state_bytes"),
  };
}

// The stage tracker rows. The rollback row exists because ROLLING_BACK /
// ROLLED_BACK are real terminal paths an operator must see — a job that
// rolled back is not the same outcome as one that failed mid-flight.
export const MIGRATION_STAGE_GROUPS = [
  { key: "checkpoint", label: "CHECKPOINT", stages: ["CREATED", "DISCOVER", "VALIDATE", "SNAPSHOT", "PREPARE"] },
  { key: "transfer", label: "TRANSFER", stages: ["TRANSFER"] },
  { key: "restore", label: "RESTORE", stages: ["VERIFY", "RESTORE"] },
  { key: "validation", label: "VALIDATION", stages: ["POST_VALIDATE", "SWITCH", "COMMIT", "CLEANUP"] },
  { key: "rollback", label: "ROLLBACK", stages: ["ROLLING_BACK", "ROLLED_BACK"] },
] as const;

export function stageGroupIndex(stage: string): number {
  for (let i = 0; i < MIGRATION_STAGE_GROUPS.length; i++) {
    if ((MIGRATION_STAGE_GROUPS[i].stages as readonly string[]).includes(stage)) return i;
  }
  return -1;
}

export function migrationIsActive(job: Pick<MigrationJob, "status">): boolean {
  return job.status === "running" || job.status === "queued";
}

// How a terminal job must be described. A clean ROLLED_BACK really did
// preserve the source; a FAILED whose rollback failed did not ("rollback
// incomplete: …" in the failure reason). The job status alone collapses both
// to "failed", so the stage in the progress document is the discriminator.
export type MigrationOutcome =
  | { kind: "active" }
  | { kind: "completed" }
  | { kind: "completed-with-warning"; sourceMayRemain: true }
  | { kind: "rolled-back"; sourcePreserved: true }
  | { kind: "needs-operator"; rollbackIncomplete: boolean }
  | { kind: "cancelled"; sourceMayRemain: boolean };

export function migrationOutcome(job: MigrationJob): MigrationOutcome {
  const readouts = readMigrationProgress(job.progress);
  const stage = readouts.stage.toUpperCase();
  const error = job.error_message || "";
  if (migrationIsActive(job)) return { kind: "active" };
  if (job.status === "completed") {
    // Post-commit source-cleanup failures complete with an error message:
    // the destination is live and a stale copy may still run on the source.
    return error ? { kind: "completed-with-warning", sourceMayRemain: true } : { kind: "completed" };
  }
  if (job.status === "cancelled") {
    return {
      kind: "cancelled",
      sourceMayRemain: stage !== "ROLLED_BACK" && !/rolled back/i.test(error),
    };
  }
  if (job.status === "failed") {
    if (stage === "ROLLED_BACK" || (!stage && /rolled back/i.test(error) && !/rollback incomplete/i.test(error))) {
      return { kind: "rolled-back", sourcePreserved: true };
    }
    return {
      kind: "needs-operator",
      rollbackIncomplete: stage === "FAILED" || /rollback incomplete/i.test(error),
    };
  }
  return { kind: "active" };
}

// `route` is not yet returned on list/detail records (only create sets it), so
// the creating browser remembers the route it used and RETRY reproduces it
// instead of silently downgrading a relay move to direct.
const ROUTE_STORE_KEY = "shiftgate-migration-routes";

type RouteMemory = Record<string, { route: "direct" | "relay"; relayEndpoint?: string }>;

function readRouteMemory(): RouteMemory {
  if (typeof window === "undefined") return {};
  try {
    return JSON.parse(window.sessionStorage.getItem(ROUTE_STORE_KEY) || "{}") as RouteMemory;
  } catch {
    return {};
  }
}

export function rememberMigrationRoute(
  migrationId: string,
  route: "direct" | "relay" | undefined,
  relayEndpoint?: string
): void {
  if (!migrationId || !route || typeof window === "undefined") return;
  try {
    const memory = readRouteMemory();
    memory[migrationId] = { route, relayEndpoint };
    window.sessionStorage.setItem(ROUTE_STORE_KEY, JSON.stringify(memory));
  } catch {
    /* storage unavailable: the server default applies */
  }
}

export function resolveMigrationRoute(job: MigrationJob): "direct" | "relay" | undefined {
  if (job.route === "direct" || job.route === "relay") return job.route;
  const remembered = readRouteMemory()[job.id];
  if (remembered?.route) return remembered.route;
  if (job.relay_endpoint) return "relay";
  return undefined;
}

export function resolveRelayEndpoint(job: MigrationJob): string | undefined {
  return job.relay_endpoint || readRouteMemory()[job.id]?.relayEndpoint;
}
