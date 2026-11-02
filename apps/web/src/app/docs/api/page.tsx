import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "API Reference",
  description: "SHIFT REST API reference — endpoints, authentication, request and response formats.",
};

interface Endpoint {
  method: "GET" | "POST" | "PUT" | "DELETE" | "PATCH";
  path: string;
  description: string;
}

const ENDPOINT_GROUPS: { title: string; endpoints: Endpoint[] }[] = [
  {
    title: "Authentication and account",
    endpoints: [
      { method: "POST", path: "/v1/auth/register", description: "Register a user and their initial organization. Returns the user, organization, and a first session." },
      { method: "POST", path: "/v1/auth/login", description: "Authenticate with email and password; returns access and refresh session tokens. Set \"remember\": true for the full 30-day refresh lifetime; without it the refresh token expires after 24 hours, so the session ends with the browser session. Set \"session_cookie\": true (or send a browser CORS fetch) and the rotating refresh token travels only in the httpOnly shift_rt cookie, never the response body." },
      { method: "POST", path: "/v1/auth/refresh", description: "Exchange a refresh token for a new access token. Optional when the shift_rt cookie is present; for cookie sessions the response omits the rotating token entirely." },
      { method: "POST", path: "/v1/auth/logout", description: "Revoke the presented session tokens and clear the refresh cookie." },
      { method: "GET", path: "/v1/me", description: "Return the authenticated user's profile, organization memberships, and roles." },
      { method: "DELETE", path: "/v1/users/me", description: "GDPR erasure: irreversibly anonymize the calling account in place (email, display name, password hash, session tokens, audit IP/user-agent metadata) and revoke every session. The pseudonymous user row survives so audit trails, organization membership, and billing history keep valid references. Returns a counts-only erasure report — {user_id, email_anonymized, sessions_removed, audit_ips_scrubbed} — never the destroyed data." },
      { method: "POST", path: "/v1/auth/sso/authorize", description: "Begin an OIDC single sign-on flow (authorization code with PKCE S256, loopback redirect); returns the authorization URL to open in the browser." },
      { method: "POST", path: "/v1/auth/sso/token", description: "Redeem the SSO callback code for session tokens." },
      { method: "GET", path: "/v1/auth/sso/required", description: "Report whether the control plane requires single sign-on for the given email's domain." },
    ],
  },
  {
    title: "Organizations",
    endpoints: [
      { method: "GET", path: "/v1/organizations", description: "List the authenticated user's organizations and roles." },
      { method: "POST", path: "/v1/organizations/{organizationID}/members", description: "Invite or add a member to the organization with a role (admin or owner; owner cannot be assigned by this endpoint). Membership changes take effect immediately — no invitation email is sent. Note: there is no member-list endpoint yet; use SCIM (Business plan) for directory sync." },
      { method: "GET", path: "/v1/organizations/{organizationID}/entitlement", description: "Return the organization's plan entitlements: plan, status, machine and storage limits with current usage, and whether live migration is allowed." },
      { method: "GET", path: "/v1/organizations/{organizationID}/usage", description: "Return metered usage for the organization. Usage queries accept inclusive from and exclusive to ISO-8601 dates." },
      { method: "GET", path: "/v1/organizations/{organizationID}/audit", description: "Return the organization's audit log events." },
      { method: "POST", path: "/v1/organizations/{organizationID}/reconcile", description: "Reconcile migration job state against the agents involved (operator)." },
    ],
  },
  {
    title: "Machines",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/machines", description: "List all machines in the organization with identity, capabilities, and status." },
      { method: "POST", path: "/v1/organizations/{organizationID}/machines", description: "Register a machine. Accepts the machine identity public key and capability report." },
      { method: "GET", path: "/v1/organizations/{organizationID}/machines/capability", description: "Fleet capability query (?capability=NAME&kind=number|text|boolean[&minimum=N]) through the relational projection of each machine's capability document. kind is required; minimum applies only to numbers. Returns full machine records." },
      { method: "POST", path: "/v1/organizations/{organizationID}/machines/{machineID}/heartbeat", description: "Record a machine heartbeat; agents call this on their report interval." },
      { method: "POST", path: "/v1/organizations/{organizationID}/machines/{machineID}/commands", description: "Dispatch a command (workload action, checkpoint, migration) to a machine's agent through its explicitly configured local_agent_url Unix socket." },
    ],
  },
  {
    title: "Workloads",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/workloads", description: "List all workloads. Supports filtering by machine and status." },
      { method: "POST", path: "/v1/organizations/{organizationID}/workloads", description: "Create a workload. Accepts command, resource requirements, device policy, and network policy." },
      { method: "POST", path: "/v1/organizations/{organizationID}/workloads/{workloadID}/status", description: "Update a workload's status from an agent's report." },
    ],
  },
  {
    title: "Workload ownership and fenced failover",
    endpoints: [
      { method: "POST", path: "/v1/organizations/{organizationID}/ownership/acquire", description: "Return the next monotone ownership epoch for a workload on a machine, plus an opaque token that is returned exactly once and never persisted in plaintext. A second, unexpired owner holding the workload is a 409 conflict." },
      { method: "POST", path: "/v1/organizations/{organizationID}/ownership/fence", description: "Confirm a fenced failover. Body: {workload_id, machine_id, epoch, token} with the epoch and token from acquire. Success is 200 {state: \"fenced\", epoch}; a stale epoch or wrong token is a 409 ownership conflict. This is the fencing path automatic standby restore requires — lease expiry or source silence alone never substitutes for it." },
    ],
  },
  {
    title: "Migrations",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/migrations", description: "List migration history for the organization." },
      { method: "POST", path: "/v1/organizations/{organizationID}/migrations", description: "Initiate a durable migration job. Send Idempotency-Key to make retries safe; the same key returns the original job, while a different request conflicts. Requires workload, source, destination machine ids, and a cold or live mode; optional route (direct or relay) and timeout_seconds. Live mode requires a plan whose tier allows it (402 LIVE_MIGRATION_PLAN_REQUIRED otherwise)." },
      { method: "GET", path: "/v1/organizations/{organizationID}/migrations/{migrationID}/events", description: "Return the migration's ordered event log: stage transitions, progress, byte counts, failures." },
      { method: "POST", path: "/v1/organizations/{organizationID}/migrations/{migrationID}/cancel", description: "Cancel a running migration and roll it back at the agent." },
      { method: "POST", path: "/v1/organizations/{organizationID}/relay/tickets", description: "Issue a short-lived, one-use HMAC relay ticket bound to organization, source machine, destination machine, migration, nonce, and expiry (never logged). Both agents connect outbound to the relay with it; the relay pairs exactly the two legs and the peer mTLS handshake remains the machine-identity authority." },
    ],
  },
  {
    title: "Checkpoints",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/checkpoints", description: "List checkpoints. Supports filtering by workload and kind (full or incremental)." },
      { method: "POST", path: "/v1/organizations/{organizationID}/checkpoints", description: "Create a checkpoint record for a workload (agent-driven)." },
    ],
  },
  {
    title: "Hosted storage",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/storage", description: "Return the organization's hosted-storage status: enabled, endpoint, bucket, prefix, reconciled usage, quota, and over-quota flag." },
      { method: "GET", path: "/v1/organizations/{organizationID}/storage/credentials", description: "Issue short-lived, org-prefix-scoped object-storage credentials to an agent (machines scope). Refused with 403 STORAGE_QUOTA_EXCEEDED when the organization is over quota, or 503 STORAGE_NOT_CONFIGURED when hosting is off." },
      { method: "POST", path: "/v1/organizations/{organizationID}/storage/reconcile", description: "Force a usage recount from the actual bucket contents (admin)." },
    ],
  },
  {
    title: "Compute marketplace",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/compute/offers", description: "List compute offers published by other organizations." },
      { method: "POST", path: "/v1/organizations/{organizationID}/compute/offers", description: "Publish this organization's spare machine capacity as an offer." },
      { method: "DELETE", path: "/v1/organizations/{organizationID}/compute/offers/{offerID}", description: "Withdraw a published offer." },
      { method: "GET", path: "/v1/organizations/{organizationID}/compute/inventory", description: "List the machine capacity this organization can offer." },
      { method: "POST", path: "/v1/organizations/{organizationID}/compute/placements", description: "Match a workload's requirements against open offers. Ranks destinations and explains every rejection without holding anything." },
      { method: "GET", path: "/v1/organizations/{organizationID}/compute/reservations", description: "List this organization's reservations, both as buyer and as seller." },
      { method: "POST", path: "/v1/organizations/{organizationID}/compute/reservations", description: "Reserve capacity from another organization's offer. Runs the real scheduler server-side inside the store's locked transaction and expires or is released rather than pinning capacity forever." },
      { method: "POST", path: "/v1/organizations/{organizationID}/compute/reservations/{reservationID}/state", description: "Advance a reservation's lifecycle: commit, release, or fail." },
    ],
  },
  {
    title: "Billing and plans",
    endpoints: [
      { method: "GET", path: "/v1/plans", description: "Return the public plan catalog: prices, seat model, machine and storage limits, and features per tier (including live_migration — false on the free tier). The catalog is the single source of plan truth." },
      { method: "POST", path: "/v1/organizations/{organizationID}/billing/checkout", description: "Create a Stripe checkout session for a plan change (admin). Includes a seat count for per-seat plans; plans without a self-service price are rejected, and unconfigured Stripe billing answers 503 BILLING_NOT_CONFIGURED." },
      { method: "POST", path: "/v1/organizations/{organizationID}/billing/portal", description: "Create a Stripe billing-portal session for subscription management (admin). An organization that never subscribed gets 400 PORTAL_NO_CUSTOMER." },
      { method: "POST", path: "/v1/webhooks/stripe", description: "Stripe webhook receiver; verifies signatures and applies subscription state using the catalog's limits, never the numbers the subscription metadata claims." },
    ],
  },
  {
    title: "Retention",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/retention", description: "Return the organization's retention windows in days (admin). Defaults when no policy was configured: audit 365, checkpoints 90, deleted storage 30." },
      { method: "PUT", path: "/v1/organizations/{organizationID}/retention", description: "Set retention windows (admin): audit_retention_days, checkpoint_retention_days, deleted_storage_retention_days. Omitted fields keep their value, 0 is the deliberate keep-forever policy, values run 0–36500 and are plan-capped (402 RETENTION_PLAN_LIMIT). Changes are audited as retention.update." },
    ],
  },
  {
    title: "Single sign-on configuration",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/sso", description: "Return the organization's single sign-on enforcement state." },
      { method: "PUT", path: "/v1/organizations/{organizationID}/sso", description: "Claim or release an email domain for single sign-on enforcement (admin). One organization per domain; while enforced, password login and self-service registration are refused for the domain." },
    ],
  },
  {
    title: "SCIM provisioning",
    endpoints: [
      { method: "GET", path: "/v1/scim/v2/Users", description: "List provisioned users; supports SCIM filters on userName, displayName, and active. Requires an API key with the scim scope — session tokens cannot provision." },
      { method: "POST", path: "/v1/scim/v2/Users", description: "Provision a user from the identity provider." },
      { method: "GET", path: "/v1/scim/v2/Users/{userID}", description: "Return one provisioned user." },
      { method: "PATCH", path: "/v1/scim/v2/Users/{userID}", description: "Update a provisioned user, including activate/deactivate (replace operation only)." },
      { method: "DELETE", path: "/v1/scim/v2/Users/{userID}", description: "Deprovision a user. SCIM delete deactivates: sessions are revoked in the same transaction and the row survives for audit integrity; the account's personal data is anonymized after a 30-day grace window." },
      { method: "GET", path: "/v1/scim/v2/ResourceTypes", description: "SCIM resource-type discovery." },
      { method: "GET", path: "/v1/scim/v2/Schemas", description: "SCIM schema discovery." },
      { method: "GET", path: "/v1/scim/v2/ServiceProviderConfig", description: "SCIM service-provider configuration." },
    ],
  },
  {
    title: "API keys",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{organizationID}/api-keys", description: "List API keys for the organization: key id, name, scopes, and last used. Metadata never contains the secret." },
      { method: "POST", path: "/v1/organizations/{organizationID}/api-keys", description: "Create an API key with specified scopes and expiration. The response contains the secret exactly once; only an HMAC digest is persisted." },
      { method: "DELETE", path: "/v1/organizations/{organizationID}/api-keys/{keyID}", description: "Revoke an API key." },
    ],
  },
  {
    title: "Service",
    endpoints: [
      { method: "GET", path: "/health", description: "Liveness: uptime and version. Unauthenticated." },
      { method: "GET", path: "/ready", description: "Readiness: database reachable. Unauthenticated." },
      { method: "GET", path: "/metrics", description: "Prometheus metrics." },
    ],
  },
];

const METHOD_COLORS: Record<string, string> = {
  GET: "text-status-info",
  POST: "text-status-healthy",
  PUT: "text-status-warning",
  DELETE: "text-status-error",
  PATCH: "text-status-warning",
};

export default function ApiPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Reference
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            API reference
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            The SHIFT REST API. All endpoints return JSON. Authentication is a
            Bearer credential — an opaque session access token or an opaque API
            key (no JSON Web Tokens are issued or accepted anywhere in the
            system). The dashboard&apos;s browser sessions ride an httpOnly,
            SameSite=Strict refresh cookie scoped to the auth paths instead of
            a body-carried refresh token.
          </p>
        </div>

        {/* Base URL and auth */}
        <div className="grid gap-4 sm:grid-cols-2 mb-12">
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-2">
              Base URL
            </p>
            <code className="text-sm font-mono text-accent">
              https://&lt;your-control-plane-host&gt;/v1
            </code>
          </div>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-2">
              Authentication
            </p>
            <code className="text-sm font-mono text-text-secondary">
              Authorization: Bearer &lt;token&gt;
            </code>
          </div>
        </div>

        {/* Pagination */}
        <div className="mb-12">
          <h2 className="text-lg font-semibold mb-4 pb-2 border-b border-border-subtle">
            Pagination and conventions
          </h2>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <ul className="text-sm text-text-secondary space-y-2">
              <li>
                • Control-plane list endpoints paginate with{" "}
                <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">limit</code>{" "}
                (1–500, default 100; values above the cap are clamped) and{" "}
                <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">offset</code>{" "}
                (records to skip) over a stable newest-first order.
              </li>
              <li>
                • Usage queries accept inclusive{" "}
                <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">from</code>{" "}
                and exclusive{" "}
                <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">to</code>{" "}
                ISO-8601 dates.
              </li>
              <li>
                • Migration events are append-only and returned in sequence
                order; checkpoint registration stores manifest and accounting
                metadata in PostgreSQL while encrypted content-addressed chunks
                remain on agents.
              </li>
              <li>
                • Treat <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">4xx</code>{" "}
                responses as actionable input or authorization errors and{" "}
                <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">5xx</code>{" "}
                responses as retryable only when the operation is idempotent.
                Every response carries an <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">X-Request-ID</code>.
              </li>
            </ul>
          </div>
        </div>

        {/* Endpoints */}
        <div className="space-y-10">
          {ENDPOINT_GROUPS.map((group) => (
            <div key={group.title}>
              <h2 className="text-lg font-semibold mb-4 pb-2 border-b border-border-subtle">
                {group.title}
              </h2>
              <div className="space-y-2">
                {group.endpoints.map((ep) => (
                  <div
                    key={`${ep.method}-${ep.path}`}
                    className="flex items-start gap-4 rounded-lg border border-border bg-bg-elevated px-5 py-4"
                  >
                    <span
                      className={`shrink-0 text-xs font-mono font-bold ${METHOD_COLORS[ep.method]}`}
                    >
                      {ep.method}
                    </span>
                    <div className="min-w-0">
                      <code className="text-sm font-mono text-text block mb-1">
                        {ep.path}
                      </code>
                      <p className="text-sm text-text-secondary">{ep.description}</p>
                    </div>
                  </div>
                ))}
              </div>
            </div>
          ))}
        </div>

        {/* Example request/response */}
        <div className="mt-16">
          <h2 className="text-lg font-semibold mb-6 pb-2 border-b border-border-subtle">
            Example
          </h2>

          <div className="grid gap-4 lg:grid-cols-2">
            <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
              <div className="border-b border-border-subtle px-4 py-2">
                <span className="text-xs font-mono text-text-muted">Request</span>
              </div>
              <div className="p-4">
                <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                  <code>{`POST /v1/organizations/org_a1b2c3/migrations
Host: api.shiftgate.dev
Authorization: Bearer sg_key_...
Idempotency-Key: 7f3a2b1c-retry-1
Content-Type: application/json

{
  "workload_id": "wl_k8m2p4q7",
  "source_machine_id": "mach_7f3a2b1c",
  "destination_machine_id": "mach_9e4d8c2a",
  "mode": "live",
  "route": "direct"
}`}</code>
                </pre>
              </div>
            </div>

            <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
              <div className="border-b border-border-subtle px-4 py-2">
                <span className="text-xs font-mono text-text-muted">Response — 202 Accepted</span>
              </div>
              <div className="p-4">
                <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                  <code>{`{
  "id": "mig_x7y8z9",
  "organization_id": "org_a1b2c3",
  "workload_id": "wl_k8m2p4q7",
  "source_machine_id": "mach_7f3a2b1c",
  "destination_machine_id": "mach_9e4d8c2a",
  "mode": "live",
  "route": "direct",
  "status": "queued",
  "progress": {},
  "created_by": "usr_2f4b6d",
  "created_at": "2026-09-13T14:30:00Z",
  "updated_at": "2026-09-13T14:30:00Z"
}

# relay-routed jobs additionally carry:
#   "relay_endpoint": "https://relay.example:9443"
#   "relay_ticket": "one-use ticket — never logged"`}</code>
                </pre>
              </div>
            </div>
          </div>
        </div>

        {/* OpenAPI link */}
        <div className="mt-12 rounded-lg border border-border bg-bg-elevated p-6">
          <h3 className="text-sm font-semibold mb-2">OpenAPI specification</h3>
          <p className="text-sm text-text-secondary mb-4">
            The full OpenAPI 3.1 specification is available for download. Use it to generate
            client libraries, validate requests, or explore the API in tools like Insomnia or Postman.
          </p>
          <a
            href="/api/openapi.yaml"
            className="inline-flex items-center gap-2 text-sm font-mono text-accent hover:underline"
          >
            openapi.yaml →
          </a>
        </div>
      </Container>
    </Section>
  );
}
