import type {
  User, Organization, SessionTokens, Principal,
  Machine, MachineHeartbeatResponse, Workload, MigrationJob, MigrationEvent,
  Checkpoint, Entitlement, AuditEvent, APIKey, APIKeyCreated, AgentCommandResponse,
  UsageSummary, Health, ApiError, PlanCatalogEntry,
  ComputeInventory, ComputeOffer, ComputeReservation, StorageStatus, ErasureReport,
} from "./types";

// The control plane every dashboard client talks to — the same address agents
// are told to use. The platform hosts the control plane, so the address is
// part of the product (mirroring DefaultControlPlaneURL in
// internal/config/config.go); NEXT_PUBLIC_API_URL overrides it when a
// deployment serves the API from another origin.
export const API_BASE = process.env.NEXT_PUBLIC_API_URL || "https://shiftgate.dev";

const BASE = API_BASE;

// The migration-create response carries a one-use relay ticket — an agent
// credential the dashboard never needs (the server hands it to the agent over
// the command channel). Drop it at the client boundary so it never lands in
// the react-query cache or any rendered surface.
function clientMigration(job: MigrationJob): MigrationJob {
  if (!job || !("relay_ticket" in job)) return job;
  const rest: MigrationJob = { ...job };
  delete rest.relay_ticket;
  return rest;
}

// Every request gets a deadline: a black-holed connection (dropped packets,
// captive portal, dead control plane) must settle as a typed error instead of
// leaving loading/mutation states pending forever.
const REQUEST_TIMEOUT_MS = 30_000;

// Refresh and expiry handling live in the auth store (single-flight rotation);
// the client only needs the seam so api.ts stays free of store imports.
export type SessionRefresher = () => Promise<boolean>;
export type SessionExpired = () => void;

class ApiClient {
  private accessToken: string | null = null;
  private refresher: SessionRefresher | null = null;
  private onSessionExpired: SessionExpired | null = null;

  setToken(token: string | null) {
    this.accessToken = token;
  }

  setSessionHandlers(refresh: SessionRefresher, expired: SessionExpired) {
    this.refresher = refresh;
    this.onSessionExpired = expired;
  }

  private async request<T>(path: string, init?: RequestInit, retry = true): Promise<T> {
    const headers = new Headers(init?.headers);
    if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    if (this.accessToken) {
      headers.set("Authorization", `Bearer ${this.accessToken}`);
    }
    const timeoutSignal = AbortSignal.timeout(REQUEST_TIMEOUT_MS);
    const signal = init?.signal ? AbortSignal.any([init.signal, timeoutSignal]) : timeoutSignal;

    let res: Response;
    try {
      res = await fetch(`${BASE}${path}`, { ...init, headers, signal });
    } catch (fetchError) {
      // Timeout/abort and network failures surface as ApiError-shaped objects
      // so errorMessage/apiErrorCode keep working for every caller.
      const aborted = fetchError instanceof Error && fetchError.name === "AbortError";
      const timedOut = fetchError instanceof Error && fetchError.name === "TimeoutError";
      const err: ApiError & { status: number } = {
        code: timedOut ? "REQUEST_TIMEOUT" : aborted ? "REQUEST_ABORTED" : "NETWORK_ERROR",
        message: timedOut
          ? "The request timed out before the control plane answered."
          : aborted
            ? "The request was cancelled."
            : "Could not reach the control plane. Check your connection and retry.",
        status: 0,
      };
      throw err;
    }
    if (!res.ok) {
      // Error bodies may be non-object (literal null, empty, proxy HTML):
      // coerce before reading fields so callers always get an ApiError.
      const raw = await res.json().catch(() => null);
      const body: Record<string, unknown> =
        raw && typeof raw === "object" ? (raw as Record<string, unknown>) : {};
      const err: ApiError & { status: number } = {
        code: typeof body.code === "string" && body.code ? body.code : "unknown",
        message:
          typeof body.message === "string" && body.message ? body.message : res.statusText || "Request failed",
        request_id: typeof body.request_id === "string" ? body.request_id : undefined,
        status: res.status,
      };
      // An expired access token is a normal event (15-minute TTL), not a
      // failure: rotate the session once and replay the request. Auth
      // endpoints never trigger this — a 401 there is a real rejection.
      if (res.status === 401 && retry && this.refresher && !path.startsWith("/v1/auth/")) {
        const refreshed = await this.refresher();
        if (refreshed) return this.request<T>(path, init, false);
        this.onSessionExpired?.();
      }
      throw err;
    }
    if (res.status === 204) return undefined as T;
    // 2xx with an empty or non-JSON body is success, not an exception.
    const text = await res.text().catch(() => "");
    if (!text) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      return undefined as T;
    }
  }

  // Health
  health() { return this.request<Health>("/health"); }

  // The server caps every list at one page (500 rows max, 100 default) and
  // never reports a total. Walking limit/offset pages keeps counts honest and
  // deep links resolvable instead of silently truncating at the boundary.
  private async listAll<T>(path: string, params: URLSearchParams = new URLSearchParams()): Promise<T[]> {
    const pageSize = 500;
    const rows: T[] = [];
    for (let offset = 0; ; offset += pageSize) {
      params.set("limit", String(pageSize));
      params.set("offset", String(offset));
      const page = await this.request<T[]>(`${path}?${params.toString()}`);
      if (!Array.isArray(page) || page.length === 0) return rows;
      rows.push(...page);
      if (page.length < pageSize) return rows;
    }
  }

  // Auth
  register(email: string, password: string, display_name: string, organization?: string) {
    return this.request<{ user: User; organization: Organization; tokens: SessionTokens }>(
      "/v1/auth/register",
      { method: "POST", body: JSON.stringify({ email, password, display_name, organization }), credentials: "include" }
    );
  }

  login(email: string, password: string, remember = false) {
    return this.request<{ user: User; tokens: SessionTokens }>(
      "/v1/auth/login",
      { method: "POST", body: JSON.stringify({ email, password, remember }), credentials: "include" }
    );
  }

  // Whether the email's domain enforces SSO: enforced domains refuse password
  // auth (403 SSO_REQUIRED), so the login/signup UI routes them to the SSO
  // path before a password is ever submitted.
  ssoRequired(email: string) {
    return this.request<{ required: boolean }>(
      `/v1/auth/sso/required?${new URLSearchParams({ email })}`
    );
  }

  // Browser OIDC entry: authorize mints the issuer URL for a PKCE pair the
  // page generated, token redeems the code the redirect brought back.
  ssoAuthorize(data: { email: string; redirect_uri: string; code_challenge: string }) {
    return this.request<{ authorization_url: string; state: string }>(
      "/v1/auth/sso/authorize",
      { method: "POST", body: JSON.stringify(data), credentials: "include" }
    );
  }

  ssoToken(data: { code: string; state: string; code_verifier: string; redirect_uri: string }) {
    return this.request<{ user: User; tokens: SessionTokens }>(
      "/v1/auth/sso/token",
      { method: "POST", body: JSON.stringify(data), credentials: "include" }
    );
  }

  githubAuthorize(data: { redirect_uri: string }) {
    return this.request<{ authorize_url: string; state: string }>(
      "/v1/auth/github/authorize",
      { method: "POST", body: JSON.stringify(data), credentials: "include" }
    );
  }

  githubToken(data: { code: string }) {
    return this.request<{ user: User; tokens: SessionTokens }>(
      "/v1/auth/github/token",
      { method: "POST", body: JSON.stringify(data), credentials: "include" }
    );
  }

  refresh(refresh_token: string) {
    return this.request<SessionTokens>(
      "/v1/auth/refresh",
      { method: "POST", body: JSON.stringify({ refresh_token }), credentials: "include" }
    );
  }

  logout() {
    return this.request<void>("/v1/auth/logout", { method: "POST", credentials: "include" });
  }

  me() { return this.request<Principal>("/v1/me"); }

  // GDPR erasure of the calling account: destroys personal data in place and
  // revokes every session. There is no organization-delete endpoint.
  eraseUser() {
    return this.request<ErasureReport>(
      "/v1/users/me",
      { method: "DELETE", credentials: "include" }
    );
  }

  // Organizations
  listOrganizations() { return this.request<Organization[]>("/v1/organizations"); }

  upsertMember(orgId: string, email: string, role: string) {
    return this.request<void>(
      `/v1/organizations/${orgId}/members`,
      { method: "POST", body: JSON.stringify({ email, role }) }
    );
  }

  listAuditEvents(orgId: string, limit = 100, offset = 0) {
    return this.request<AuditEvent[]>(
      `/v1/organizations/${orgId}/audit?${new URLSearchParams({ limit: String(limit), offset: String(offset) })}`
    );
  }

  // Machines
  listMachines(orgId: string) {
    return this.listAll<Machine>(`/v1/organizations/${orgId}/machines`);
  }

  createMachine(orgId: string, data: { machine_id: string; name: string; agent_url: string; local_agent_url?: string; capabilities?: Record<string, unknown> }) {
    return this.request<Machine>(
      `/v1/organizations/${orgId}/machines`,
      { method: "POST", body: JSON.stringify(data) }
    );
  }

  machineHeartbeat(orgId: string, machineId: string, data: Record<string, unknown>) {
    return this.request<MachineHeartbeatResponse>(
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
    return this.listAll<Workload>(`/v1/organizations/${orgId}/workloads`);
  }

  cancelMigration(orgId: string, migrationId: string) {
    return this.request<MigrationJob>(
      `/v1/organizations/${orgId}/migrations/${migrationId}/cancel`,
      { method: "POST", body: JSON.stringify({}) }
    );
  }

  // Migrations
  listMigrations(orgId: string) {
    return this.listAll<MigrationJob>(`/v1/organizations/${orgId}/migrations`).then((jobs) =>
      jobs.map(clientMigration)
    );
  }

  createMigration(orgId: string, data: {
    workload_id: string;
    source_machine_id: string;
    destination_machine_id: string;
    mode?: string;
    route?: "direct" | "relay";
    idempotency_key?: string;
    timeout_seconds?: number;
  }) {
    return this.request<MigrationJob>(
      `/v1/organizations/${orgId}/migrations`,
      { method: "POST", body: JSON.stringify(data) }
    ).then(clientMigration);
  }

  listMigrationEvents(orgId: string, migrationId: string) {
    return this.listAll<MigrationEvent>(`/v1/organizations/${orgId}/migrations/${migrationId}/events`);
  }

  // Checkpoints
  listCheckpoints(orgId: string, workloadId?: string) {
    const params = new URLSearchParams();
    if (workloadId) params.set("workload_id", workloadId);
    return this.listAll<Checkpoint>(`/v1/organizations/${orgId}/checkpoints`, params);
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

  // Hosted checkpoint storage
  getStorageStatus(orgId: string) {
    return this.request<StorageStatus>(`/v1/organizations/${orgId}/storage`);
  }

  // Compute marketplace
  computeInventory(orgId: string) {
    return this.request<ComputeInventory>(`/v1/organizations/${orgId}/compute/inventory`);
  }

  listComputeOffers(orgId: string) {
    return this.request<ComputeOffer[]>(`/v1/organizations/${orgId}/compute/offers`);
  }

  listComputeReservations(orgId: string) {
    return this.request<ComputeReservation[]>(`/v1/organizations/${orgId}/compute/reservations`);
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
    return this.listAll<APIKey>(`/v1/organizations/${orgId}/api-keys`);
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
    return this.listAll<UsageSummary>(`/v1/organizations/${orgId}/usage`, params);
  }

  // Referral: stamp a share code (from a /r/<code> link) on the organization
  // at signup. The server refuses self-referral (same org or same email
  // domain), so a stray code can never reward an existing member.
  claimReferral(orgId: string, code: string) {
    return this.request<{ status: string; code: string }>(
      `/v1/organizations/${orgId}/referral/claim`,
      { method: "POST", body: JSON.stringify({ code }) }
    );
  }
}

export const api = new ApiClient();
export default api;
