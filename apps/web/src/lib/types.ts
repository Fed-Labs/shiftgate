// SHIFT API types — hand-maintained to match the control plane OpenAPI spec
// (api/openapi/shift.yaml). scripts/sync-openapi.mjs copies the spec into
// public/ for clients; it does not generate these types, so keep them in sync
// with the spec and the Go response shapes by hand.

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

/**
 * Entitlement plan keys as the server reports them (Entitlement.plan).
 * "paid" is the legacy/negotiated-tier label (billing_routes.go); the public
 * catalog never sells it — catalog keys are CatalogPlan / Plan.key. Defined as
 * a named union with a Plan alias so the types-drift gate does not confuse it
 * with the Plan schema (which is the catalog entry, keyed by CatalogPlan).
 */
export type EntitlementPlan = "free" | "pro" | "business" | "enterprise" | "paid";
export type Plan = EntitlementPlan;

/** Plan keys the server-side catalog can contain. */
export type CatalogPlan = "free" | "pro" | "business" | "enterprise";

/** One tier of the server-side plan catalog (GET /v1/plans). */
export interface PlanCatalogEntry {
  key: CatalogPlan;
  name: string;
  price_cents: number; // -1 = custom, 0 = free
  /** Prepay price for a full year (two months free). 0 = no annual offer, -1 = custom. */
  price_cents_annual?: number;
  /** Legacy per-seat billing shape; Business is flat with included seats. */
  per_seat?: boolean;
  /** Seats the flat price covers before seat_overage_cents applies per extra seat. */
  included_seats?: number;
  seat_overage_cents?: number;
  seat_derived_storage?: boolean;
  storage_per_seat_bytes?: number;
  storage_cap_bytes?: number;
  description: string;
  features: string[];
  max_machines: number; // -1 = unlimited
  max_storage_bytes: number; // -1 = unlimited
  live_migration: boolean; // false on the free (cold-only) tier
  team_members?: boolean;
  audit_logging?: boolean;
  sso_and_scim?: boolean;
  periodic_policies?: boolean;
  failover?: boolean;
  sourceguard?: boolean;
  sourceguard_credits_months?: number;
  storage_reclaim?: boolean;
  relay_allowance_bytes?: number;
  overage_storage_cents_per_gb?: number;
  overage_transfer_cents_per_gb?: number;
  overage_cap_cents_default?: number;
  max_api_requests_per_day?: number; // -1 = unlimited
  max_api_requests_per_hour?: number; // -1 = unlimited
  max_migrations_per_day?: number; // -1 = unlimited
  history_retention_days?: number;
  trial_eligible?: boolean;
}

export type APIKeyScope =
  | "read" | "operate" | "admin"
  | "machines" | "workloads" | "migrations" | "checkpoints"
  | "compute" | "scim";

export type UsageKind = "checkpoint_storage" | "transfer_bytes" | "machine_hours" | "api_requests";

export type WorkloadStatus =
  | "registered" | "starting" | "running" | "paused"
  | "checkpointed" | "restoring" | "stopped" | "failed";

export interface User {
  id: string;
  email: string;
  display_name: string;
  disabled_at?: string | null;
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
  // Optional/writeOnly per the spec: cookie-based browser sessions never see
  // the rotating refresh token — it lives only in the httpOnly `shift_rt`
  // cookie — so responses legitimately omit it.
  refresh_token?: string;
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
  local_agent_url?: string;
  capabilities: MachineCapabilitiesPayload;
  status: MachineStatus;
  last_seen_at: string | null;
  created_at: string;
  updated_at: string;
}

/** Heartbeat response (POST /machines/{id}/heartbeat): Machine plus the gate. */
export interface MachineHeartbeatResponse extends Machine {
  live_migration_allowed: boolean;
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
  route?: "direct" | "relay";
  relay_endpoint?: string;
  relay_ticket?: string;
  status: MigrationStatus;
  progress: MigrationProgress;
  error_message?: string;
  idempotency_key?: string;
  attempt_count: number;
  lease_owner?: string;
  lease_expires_at?: string | null;
  agent_migration_id?: string;
  managed: boolean;
  cancel_requested: boolean;
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
  /** True while a 14-day no-card Business trial is running (expires_at in the future). */
  trial_active?: boolean;
}

/** Trial state (GET /v1/organizations/{id}/trial). */
export interface TrialStatus {
  organization_id: string;
  /** True when a trial row exists — including an already-expired trial. */
  trial: boolean;
  eligible: boolean;
  started_at?: string;
  expires_at?: string;
  /** Floored whole days remaining per the API; derive display from expires_at. */
  days_left?: number;
  converted?: boolean;
}

/** Trial start response (POST /v1/organizations/{id}/trial). */
export interface TrialStarted {
  organization_id: string;
  plan: string;
  trial: boolean;
  started_at: string;
  expires_at: string;
}

/** Self-serve usage summary (GET /v1/organizations/{id}/usage/summary). */
export interface UsageSummaryResponse {
  period: { from: string; to: string };
  storage: { used_bytes: number; max_bytes: number; pct: number };
  transfer: {
    bytes_30d: number;
    limit_bytes: number;
    by_channel: Record<string, number>;
    throttle: "ok" | "warn" | "exceeded";
    resets_at: string;
  };
  machines: { online: number; total: number };
  migrations_7d: Record<string, number>;
  seats: { included: number; purchased: number; members: number };
  api: { requests_today: number; daily_budget: number };
}

/** Stripe checkout body (POST /v1/organizations/{id}/billing/checkout). */
export interface CheckoutOptions {
  plan: string;
  success_url: string;
  cancel_url: string;
  seats?: number;
  interval?: "month" | "year";
  addons?: string[];
  coupon?: string;
  spend_cap_cents?: number;
}

// Control-plane-hosted checkpoint storage status. When enabled is false the
// control plane does not host storage and location fields are absent.
export interface StorageStatus {
  enabled: boolean;
  endpoint?: string;
  bucket?: string;
  prefix?: string;
  used_storage_bytes: number;
  max_storage_bytes: number;
  over_quota: boolean;
  last_reconciled_at?: string;
}

export interface AuditEvent {
  id: string;
  organization_id?: string;
  actor_user_id?: string;
  action: string;
  resource_type: string;
  resource_id?: string;
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
  expires_at?: string | null;
  last_used_at?: string | null;
  revoked_at?: string | null;
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

/** Counts-only GDPR erasure summary (DELETE /v1/users/me). */
export interface ErasureReport {
  user_id: string;
  email_anonymized: boolean;
  sessions_removed: number;
  api_keys_removed: number;
  audit_ips_scrubbed: number;
}

/* ------------------------------------------------------------------ */
/*  Compute marketplace                                                */
/* ------------------------------------------------------------------ */

export interface ComputeResources {
  cpu_count?: number;
  memory_bytes?: number;
  storage_bytes?: number;
  network_mbps?: number;
  gpus?: GPUDevice[];
}

export interface ComputePricing {
  currency: string;
  cpu_hour_micros?: number;
  memory_gib_hour_micros?: number;
  storage_gib_hour_micros?: number;
  gpu_hour_micros?: number;
  egress_gib_micros?: number;
  minimum_charge_micros?: number;
}

export interface ComputeOffer {
  id: string;
  organization_id: string;
  machine_id: string;
  machine_name?: string;
  agent_url?: string;
  exposed: ComputeResources;
  committed: ComputeResources;
  available: ComputeResources;
  pricing: ComputePricing;
  geography: { region?: string; country?: string; zone?: string };
  policy: { visibility: string; [key: string]: unknown };
  availability: { status: string; last_seen_at?: string };
  trust: string;
  identity_verified: boolean;
  /** Hardware and CRIU capabilities from the machine registry, not the publisher. */
  capabilities: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  withdrawn_at?: string;
}

export interface ComputeInventory {
  trading_enabled: boolean;
  offers: ComputeOffer[];
}

export interface ComputeReservation {
  id: string;
  offer_id: string;
  organization_id: string;
  machine_id: string;
  workload_id?: string;
  migration_id?: string;
  requested: ComputeResources;
  state: "PENDING" | "ACTIVE" | "RELEASED" | "EXPIRED" | "FAILED";
  hourly_micros: number;
  currency?: string;
  expires_at: string;
  created_at: string;
  updated_at: string;
  released_at?: string;
  error?: string;
}
