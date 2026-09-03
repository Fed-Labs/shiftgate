// SHIFTGATE API types — generated from the control plane OpenAPI spec.

export type Role = "owner" | "admin" | "operator" | "viewer";

export type MachineStatus = "online" | "offline" | "draining" | "disabled";

export type MigrationStatus = "queued" | "running" | "completed" | "failed" | "cancelled";
export type MigrationMode = "cold" | "live";

export type MigrationStage =
  | "CREATED" | "DISCOVER" | "VALIDATE" | "SNAPSHOT" | "PREPARE"
  | "TRANSFER" | "VERIFY" | "RESTORE" | "POST_VALIDATE" | "SWITCH"
  | "COMMIT" | "CLEANUP" | "COMPLETED" | "FAILED"
  | "ROLLING_BACK" | "ROLLED_BACK" | "CANCELLED";

export type CheckpointKind = "full" | "incremental";
export type CheckpointStatus = "creating" | "available" | "corrupt" | "deleted";

export type Plan = "free" | "pro" | "business" | "enterprise" | "paid";

/** Plan keys the server-side catalog can contain. */
export type CatalogPlan = "free" | "pro" | "business" | "enterprise";

/** One tier of the server-side plan catalog (GET /v1/plans). */
export interface PlanCatalogEntry {
  key: CatalogPlan;
  name: string;
  price_cents: number; // -1 = custom, 0 = free
  per_seat?: boolean;
  description: string;
  features: string[];
  max_machines: number; // -1 = unlimited
  max_storage_bytes: number; // -1 = unlimited
}

export type APIKeyScope =
  | "read" | "operate" | "admin"
  | "machines" | "workloads" | "migrations" | "checkpoints";

export type UsageKind = "checkpoint_storage" | "transfer_bytes" | "machine_hours" | "api_requests";

export type WorkloadStatus =
  | "registered" | "starting" | "running" | "paused"
  | "checkpointed" | "restoring" | "stopped" | "failed";

export interface User {
  id: string;
  email: string;
  display_name: string;
  disabled_at: string | null;
  created_at: string;
}

export interface Organization {
  id: string;
  name: string;
  role: Role;
  created_at: string;
}

export interface SessionTokens {
  session_id: string;
  access_token: string;
  refresh_token: string;
  token_type: "Bearer";
  expires_at: string;
}

export interface Principal {
  session_id: string;
  user_id: string;
  email: string;
  display_name: string;
  organization_id?: string;
  role?: Role;
  api_key_id?: string;
  scopes?: APIKeyScope[];
}

export interface GPUDevice {
  vendor: string;
  model: string;
  driver_version?: string;
  memory_bytes?: number;
  compute_capability?: string;
  runtime?: string;
  checkpoint_restore: boolean;
}

export interface StorageDevice {
  mountpoint: string;
  filesystem: string;
  total_bytes: number;
  available_bytes: number;
}

export interface CRIUCapabilities {
  installed: boolean;
  version?: string;
  healthy: boolean;
  features?: string[];
  errors?: string[];
}

export interface MachineCapabilities {
  machine_id: string;
  hostname: string;
  os: string;
  distribution: string;
  kernel: string;
  architecture: string;
  cpus: number;
  memory_bytes: number;
  storage: StorageDevice[];
  gpus: GPUDevice[];
  criu: CRIUCapabilities;
  features: Record<string, boolean>;
  agent_version: string;
  observed_at: string;
}

export interface MachineCapabilitiesPayload extends Partial<MachineCapabilities> {
  [key: string]: unknown;
}

export interface Machine {
  id: string;
  organization_id: string;
  machine_id: string;
  name: string;
  agent_url: string;
  capabilities: MachineCapabilitiesPayload;
  status: MachineStatus;
  last_seen_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface WorkloadSpec {
  id: string;
  name: string;
  command: string[];
  root_path: string;
  working_dir: string;
  environment?: Record<string, string>;
  uid: number;
  gid: number;
  paths: Array<{ path: string; mode: "read_only" | "read_write"; one_filesystem: boolean; exclusions?: string[] }>;
  ports?: Array<{ protocol: string; container_port: number; host_port?: number }>;
  resources: {
    cpu_count?: number;
    memory_bytes?: number;
    storage_bytes?: number;
    pids?: number;
    gpus?: GPUDevice[];
  };
  device_policy: "reject_incompatible" | "warn_incompatible";
  network_policy: "preserve" | "reconnect" | "drain";
}

export interface Workload {
  id: string;
  organization_id: string;
  machine_id?: string;
  name: string;
  spec: Record<string, unknown>;
  status: WorkloadRuntimeStatus;
  created_at: string;
  updated_at: string;
}

export interface WorkloadRuntimeStatus {
  [key: string]: unknown;
  state?: WorkloadStatus;
  cpu_percent?: number;
  memory_bytes?: number;
}

export interface MigrationProgress {
  [key: string]: unknown;
  percent?: number;
}

export interface MigrationJob {
  id: string;
  organization_id: string;
  workload_id: string;
  source_machine_id: string;
  destination_machine_id: string;
  mode: MigrationMode;
  status: MigrationStatus;
  progress: MigrationProgress;
  error_message?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
  completed_at: string | null;
}

export interface MigrationEvent {
  id: string;
  sequence: number;
  stage: string;
  message: string;
  progress: number;
  bytes_done: number;
  bytes_total: number;
  created_at: string;
}

export type AgentAction =
  | "start"
  | "pause"
  | "resume"
  | "stop"
  | "delete"
  | "checkpoint"
  | "restore"
  | "migrate";

export interface AgentCommandResponse {
  machine: string;
  action: AgentAction;
  result: unknown;
  workload?: Workload;
  checkpoint?: Checkpoint;
  migration?: MigrationJob;
}

export interface Checkpoint {
  id: string;
  organization_id: string;
  workload_id: string;
  machine_id: string;
  kind: CheckpointKind;
  parent_id?: string;
  manifest: Record<string, unknown>;
  plain_bytes: number;
  stored_bytes: number;
  chunk_count: number;
  status: CheckpointStatus;
  created_at: string;
  deleted_at: string | null;
}

export interface Entitlement {
  organization_id: string;
  plan: Plan;
  status: string;
  max_storage_bytes: number;
  used_storage_bytes: number;
  max_machines: number;
  stripe_customer_id?: string;
}

export interface AuditEvent {
  id: string;
  organization_id?: string;
  actor_user_id?: string;
  action: string;
  resource_type: string;
  resource_id: string;
  metadata: Record<string, unknown>;
  request_id?: string;
  remote_addr?: string;
  created_at: string;
}

export interface APIKey {
  id: string;
  organization_id: string;
  name: string;
  prefix: string;
  scopes: APIKeyScope[];
  expires_at: string | null;
  last_used_at: string | null;
  revoked_at: string | null;
  created_at: string;
}

export interface APIKeyCreated extends APIKey {
  secret: string;
}

export interface UsageSummary {
  kind: UsageKind;
  quantity: number;
  period_start: string;
  period_end: string;
}

export interface Health {
  status: "ok";
  version: string;
  started_at: string;
  uptime: string;
}

export interface ApiError {
  code: string;
  message: string;
  request_id?: string;
}
