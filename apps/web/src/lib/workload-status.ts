import type { AgentAction, WorkloadRuntimeStatus } from "./types";

// The normalized workload status enum the control plane writes (F9), plus the
// terminal "deleted" tombstone the delete dispatch leaves behind. The fleet
// list and the detail page both normalize through here so they can never
// disagree about which state a workload is in or which actions are legal.
export const WORKLOAD_STATES = [
  "registered",
  "starting",
  "running",
  "paused",
  "checkpointed",
  "restoring",
  "stopped",
  "failed",
  "deleted",
] as const;

// Legacy drift values older writers put into `status.state`: the restore
// dispatch used to store the checkpoint restore transaction state
// (RestoreState: PREPARING…COMMITTED, ROLLING_BACK…FAILED), and the create
// default used the key `status` instead of `state`. Normalize them to the
// enum so one bad write cannot blank out every action button.
function fromRestoreRecordState(value: string): string | null {
  switch (value) {
    case "preparing":
    case "filesystem_switched":
    case "process_running":
    case "validated":
    case "rolling_back":
      return "restoring";
    case "committed":
      return "running";
    case "rolled_back":
    case "failed":
      return "failed";
    default:
      return null;
  }
}

export function normalizeWorkloadState(status: WorkloadRuntimeStatus | undefined): string {
  const doc = (status ?? {}) as Record<string, unknown>;
  const raw =
    (typeof doc.state === "string" && doc.state) ||
    (typeof doc.status === "string" && doc.status) ||
    "";
  const value = raw.trim().toLowerCase();
  if (!value) return "unknown";
  if ((WORKLOAD_STATES as readonly string[]).includes(value)) return value;
  return fromRestoreRecordState(value) ?? value;
}

export function isDeletedWorkload(status: WorkloadRuntimeStatus | undefined): boolean {
  return normalizeWorkloadState(status) === "deleted";
}

export interface WorkloadActionSpec {
  key: AgentAction;
  label: string;
  states: string[];
  danger?: boolean;
  confirm?: string;
  timeoutSeconds: number;
}

// One per-state action table for every workload surface. Dispatching an
// action the agent only accepts for another state comes back as
// AGENT_COMMAND_FAILED, so the button must not exist in the first place.
// In-flight states (starting, restoring) get no lifecycle action at all, and
// the terminal tombstone (deleted) gets none either.
export const WORKLOAD_ACTIONS: WorkloadActionSpec[] = [
  { key: "start", label: "START", states: ["registered", "stopped", "checkpointed"], timeoutSeconds: 30 },
  { key: "pause", label: "PAUSE", states: ["running"], timeoutSeconds: 30 },
  { key: "resume", label: "RESUME", states: ["paused"], timeoutSeconds: 30 },
  { key: "checkpoint", label: "CHECKPOINT", states: ["running"], timeoutSeconds: 120 },
  {
    key: "stop",
    label: "STOP",
    states: ["running", "paused"],
    danger: true,
    confirm: "CONFIRM STOP",
    timeoutSeconds: 30,
  },
  {
    key: "delete",
    label: "DELETE",
    states: ["registered", "stopped", "failed"],
    danger: true,
    confirm: "CONFIRM DELETE",
    timeoutSeconds: 30,
  },
];

export function actionsForState(state: string): WorkloadActionSpec[] {
  return WORKLOAD_ACTIONS.filter((a) => a.states.includes(state));
}

export function timeoutForAction(action: AgentAction): number {
  return WORKLOAD_ACTIONS.find((a) => a.key === action)?.timeoutSeconds ?? 30;
}

// The three-state control on the detail page: idle workloads start, running
// workloads pause, paused workloads resume — and states with no legal
// lifecycle command get no button at all.
export function primaryWorkloadAction(state: string): WorkloadActionSpec | null {
  return (
    actionsForState(state).find((a) => a.key === "start" || a.key === "pause" || a.key === "resume") ?? null
  );
}
