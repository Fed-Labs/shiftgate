// Agent and control-plane types, mirroring the Go structs the services
// actually serialize (internal/model, internal/checkpoint, internal/agent,
// internal/controlplane). Field names follow the JSON tags verbatim.

/* ------------------------------------------------------------------ */
/*  Agent — machine, health                                            */
/* ------------------------------------------------------------------ */

export type WorkloadStatus =
  | "registered" | "starting" | "running" | "paused"
  | "checkpointed" | "restoring" | "stopped" | "failed";

export type MigrationStage =
  | "CREATED" | "DISCOVER" | "VALIDATE" | "SNAPSHOT" | "PREPARE"
  | "TRANSFER" | "VERIFY" | "RESTORE" | "POST_VALIDATE" | "SWITCH"
  | "COMMIT" | "CLEANUP" | "COMPLETED" | "FAILED"
  | "ROLLING_BACK" | "ROLLED_BACK" | "CANCELLED";

export type MigrationMode = "cold" | "live";

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

export interface Health {
  status: string;
  version: string;
  machine_id: string;
  started_at: string;
  uptime: string;
}

export interface DoctorResponse {
  healthy: boolean;
  checks: Array<{ name: string; ok: boolean; message: string }>;
}

export interface MachineIdentity {
  id: string;
  name: string;
  public_key_pem: string;
}

/* ------------------------------------------------------------------ */
/*  Agent — workloads                                                  */
/* ------------------------------------------------------------------ */

export interface PathSpec {
  path: string;
  mode: "read_only" | "read_write";
  one_filesystem: boolean;
  exclusions?: string[];
}

export interface PortSpec {
  protocol: string;
  container_port: number;
  host_port?: number;
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
  paths: PathSpec[];
  ports?: PortSpec[];
  device_policy: "reject_incompatible" | "warn_incompatible";
  network_policy: "preserve" | "reconnect" | "drain";
}

export interface ProcessState {
  pid?: number;
  [key: string]: unknown;
}

export interface Workload {
  spec: WorkloadSpec;
  status: WorkloadStatus;
  process?: ProcessState | null;
  latest_checkpoint_id?: string;
  generation: number;
  last_error?: string;
}

/* ------------------------------------------------------------------ */
/*  Agent — checkpoints, restores, forks                               */
/* ------------------------------------------------------------------ */

export type CheckpointKind = "full" | "incremental";

export interface CheckpointSummary {
  id: string;
  workload_id: string;
  workload_name: string;
  parent_id?: string;
  kind: CheckpointKind;
  created_at: string;
  plain_bytes: number;
  stored_bytes: number;
  key_version: number;
  manifest_hash: string;
}

export interface RestoreRecord {
  id: string;
  checkpoint_id: string;
  workload_id: string;
  state: string;
  target_root: string;
  staging_root: string;
  restore_directory: string;
  pid?: number;
  created_workload: boolean;
  created_at: string;
  updated_at: string;
  error?: string;
}

export interface ForkRecord {
  id: string;
  [key: string]: unknown;
}

/* ------------------------------------------------------------------ */
/*  Agent — migrations                                                 */
/* ------------------------------------------------------------------ */

export interface Destination {
  machine_id: string;
  agent_url: string;
  server_name?: string;
}

export interface CompatibilityIssue {
  code: string;
  severity: string;
  resource: string;
  description: string;
  adaptation?: string;
}

export interface CompatibilityReport {
  compatible: boolean;
  issues: CompatibilityIssue[];
  checked_at: string;
}

export interface MigrationMetrics {
  total_state_bytes: number;
  transferred_bytes: number;
  deduplicated_bytes: number;
  checkpoint_duration: number;
  restore_duration: number;
  downtime: number;
  transfer_duration: number;
  transfer_bytes_per_second: number;
}

export interface MigrationEvent {
  sequence: number;
  stage: MigrationStage;
  message: string;
  timestamp: string;
  progress: number;
  bytes_done?: number;
  bytes_total?: number;
}

export interface Migration {
  id: string;
  workload_id: string;
  checkpoint_id?: string;
  source_machine_id: string;
  destination: Destination;
  mode: MigrationMode;
  stage: MigrationStage;
  compatibility: CompatibilityReport;
  metrics: MigrationMetrics;
  events: MigrationEvent[];
  created_at: string;
  updated_at: string;
  completed_at?: string | null;
  failure_code?: string;
  failure_reason?: string;
  source_preserved: boolean;
  revision: number;
}

