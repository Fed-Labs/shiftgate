import type {
  User, Organization, SessionTokens, Principal,
  Machine, Workload, MigrationJob, MigrationEvent,
  Checkpoint, Entitlement, AuditEvent, APIKey, APIKeyCreated, AgentCommandResponse,
  UsageSummary, Health, ApiError, PlanCatalogEntry,
} from "./types";

const BASE = process.env.NEXT_PUBLIC_API_URL || "http://127.0.0.1:8090";

class ApiClient {
  private accessToken: string | null = null;

  setToken(token: string | null) {
    this.accessToken = token;
  }

  private async request<T>(path: string, init?: RequestInit): Promise<T> {
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(init?.headers as Record<string, string> || {}),
    };
    if (this.accessToken) {
      headers["Authorization"] = `Bearer ${this.accessToken}`;
    }
    const res = await fetch(`${BASE}${path}`, { ...init, headers });
    if (!res.ok) {
      const body = await res.json().catch(() => ({ code: "unknown", message: res.statusText }));
      const err: ApiError & { status: number } = {
        code: body.code || "unknown",
        message: body.message || res.statusText,
        request_id: body.request_id,
        status: res.status,
      };
      throw err;
    }
    if (res.status === 204) return undefined as T;
    return res.json();
  }

  // Health
  health() { return this.request<Health>("/health"); }

  // Auth
  register(email: string, password: string, display_name: string, organization?: string) {
    return this.request<{ user: User; organization: Organization; tokens: SessionTokens }>(
      "/v1/auth/register",
      { method: "POST", body: JSON.stringify({ email, password, display_name, organization }) }
    );
  }

  login(email: string, password: string) {
    return this.request<{ user: User; tokens: SessionTokens }>(
      "/v1/auth/login",
      { method: "POST", body: JSON.stringify({ email, password }) }
    );
  }

  refresh(refresh_token: string) {
    return this.request<SessionTokens>(
      "/v1/auth/refresh",
      { method: "POST", body: JSON.stringify({ refresh_token }) }
    );
  }

  logout() {
    return this.request<void>("/v1/auth/logout", { method: "POST" });
  }

  me() { return this.request<Principal>("/v1/me"); }

  // Organizations
  listOrganizations() { return this.request<Organization[]>("/v1/organizations"); }

  upsertMember(orgId: string, email: string, role: string) {
    return this.request<void>(
      `/v1/organizations/${orgId}/members`,
      { method: "POST", body: JSON.stringify({ email, role }) }
    );
  }

  listAuditEvents(orgId: string, limit = 100) {
    return this.request<AuditEvent[]>(`/v1/organizations/${orgId}/audit?limit=${limit}`);
  }

  // Machines
  listMachines(orgId: string) {
    return this.request<Machine[]>(`/v1/organizations/${orgId}/machines`);
  }

  createMachine(orgId: string, data: { machine_id: string; name: string; agent_url: string; capabilities?: Record<string, unknown> }) {
    return this.request<Machine>(
      `/v1/organizations/${orgId}/machines`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  machineHeartbeat(orgId: string, machineId: string, data: Record<string, unknown>) {
    return this.request<Machine>(
      `/v1/organizations/${orgId}/machines/${machineId}/heartbeat`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  agentCommand(orgId: string, machineId: string, data: {
    action: AgentCommandResponse["action"];
    workload_id?: string;
    checkpoint_id?: string;
    destination_id?: string;
    mode?: "cold" | "live";
    timeout_seconds?: number;
  }) {
    return this.request<AgentCommandResponse>(
      `/v1/organizations/${orgId}/machines/${machineId}/commands`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  reconcile(orgId: string) {
    return this.request<{ reconciled_workloads: number; failed: Record<string, string> }>(
      `/v1/organizations/${orgId}/reconcile`,
      { method: "POST" }
    );
  }

  // Workloads
  listWorkloads(orgId: string) {
    return this.request<Workload[]>(`/v1/organizations/${orgId}/workloads`);
  }

  createWorkload(orgId: string, data: { name: string; machine_id?: string; spec: Record<string, unknown>; status?: Record<string, unknown> }) {
    return this.request<Workload>(
      `/v1/organizations/${orgId}/workloads`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  updateWorkloadStatus(orgId: string, workloadId: string, status: Record<string, unknown>) {
    return this.request<Workload>(
      `/v1/organizations/${orgId}/workloads/${workloadId}/status`,
      { method: "POST", body: JSON.stringify(status) }
    );
  }

  cancelMigration(orgId: string, migrationId: string) {
    return this.request<{ status: string }>(
      `/v1/organizations/${orgId}/migrations/${migrationId}/cancel`,
      { method: "POST", body: JSON.stringify({}) }
    );
  }

  // Migrations
  listMigrations(orgId: string) {
    return this.request<MigrationJob[]>(`/v1/organizations/${orgId}/migrations`);
  }

  createMigration(orgId: string, data: { workload_id: string; source_machine_id: string; destination_machine_id: string; mode?: string }) {
    return this.request<MigrationJob>(
      `/v1/organizations/${orgId}/migrations`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  listMigrationEvents(orgId: string, migrationId: string) {
    return this.request<MigrationEvent[]>(`/v1/organizations/${orgId}/migrations/${migrationId}/events`);
  }

  // Checkpoints
  listCheckpoints(orgId: string, workloadId?: string) {
    const q = workloadId ? `?workload_id=${workloadId}` : "";
    return this.request<Checkpoint[]>(`/v1/organizations/${orgId}/checkpoints${q}`);
  }

  registerCheckpoint(orgId: string, data: Record<string, unknown>) {
    return this.request<Checkpoint>(
      `/v1/organizations/${orgId}/checkpoints`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  // Entitlements
  getEntitlement(orgId: string) {
    return this.request<Entitlement>(`/v1/organizations/${orgId}/entitlement`);
  }

  // Billing
  listPlans() {
    return this.request<PlanCatalogEntry[]>("/v1/plans");
  }

  startCheckout(orgId: string, data: { plan: string; success_url: string; cancel_url: string; seats?: number }) {
    return this.request<{ url: string; session_id: string; plan: string }>(
      `/v1/organizations/${orgId}/billing/checkout`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  openBillingPortal(orgId: string, data: { return_url: string }) {
    return this.request<{ url: string }>(
      `/v1/organizations/${orgId}/billing/portal`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  // API Keys
  listAPIKeys(orgId: string) {
    return this.request<APIKey[]>(`/v1/organizations/${orgId}/api-keys`);
  }

  createAPIKey(orgId: string, data: { name: string; scopes: string[]; expires_at?: string | null }) {
    return this.request<APIKeyCreated>(
      `/v1/organizations/${orgId}/api-keys`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  revokeAPIKey(orgId: string, keyId: string) {
    return this.request<void>(`/v1/organizations/${orgId}/api-keys/${keyId}`, { method: "DELETE" });
  }

  // Usage
  listUsage(orgId: string, from?: string, to?: string) {
    const params = new URLSearchParams();
    if (from) params.set("from", from);
    if (to) params.set("to", to);
    return this.request<UsageSummary[]>(`/v1/organizations/${orgId}/usage?${params}`);
  }
}

export const api = new ApiClient();
export default api;
