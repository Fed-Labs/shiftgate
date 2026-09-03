import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "API Reference",
  description: "SHIFTGATE REST API reference — endpoints, authentication, request and response formats.",
};

interface Endpoint {
  method: "GET" | "POST" | "PUT" | "DELETE" | "PATCH";
  path: string;
  description: string;
}

const ENDPOINT_GROUPS: { title: string; endpoints: Endpoint[] }[] = [
  {
    title: "Authentication",
    endpoints: [
      { method: "POST", path: "/v1/auth/register", description: "Register a new organization account. Returns organization ID and initial API key." },
      { method: "POST", path: "/v1/auth/login", description: "Authenticate and receive a session token or API key." },
      { method: "GET", path: "/v1/me", description: "Return the authenticated user's profile, organization membership, and role." },
    ],
  },
  {
    title: "Machines",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{id}/machines", description: "List all machines in the organization. Supports filtering by status and capability." },
      { method: "POST", path: "/v1/organizations/{id}/machines", description: "Register a new machine. Accepts a machine identity public key and capability report." },
    ],
  },
  {
    title: "Workloads",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{id}/workloads", description: "List all workloads. Supports filtering by machine, status, and resource requirements." },
      { method: "POST", path: "/v1/organizations/{id}/workloads", description: "Create a new workload. Accepts command, resource requirements, device policy, and network policy." },
    ],
  },
  {
    title: "Migrations",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{id}/migrations", description: "List migration history. Supports filtering by workload, source, destination, and status." },
      { method: "POST", path: "/v1/organizations/{id}/migrations", description: "Initiate a migration. Specifies workload, target machine, and migration mode (live or cold)." },
    ],
  },
  {
    title: "Checkpoints",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{id}/checkpoints", description: "List checkpoints. Supports filtering by workload and type (full or incremental)." },
    ],
  },
  {
    title: "API Keys",
    endpoints: [
      { method: "GET", path: "/v1/organizations/{id}/api-keys", description: "List API keys for the organization. Shows key ID, name, scopes, and last used timestamp." },
      { method: "POST", path: "/v1/organizations/{id}/api-keys", description: "Create a new API key with specified scopes and expiration." },
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
            The SHIFTGATE REST API. All endpoints return JSON and require authentication
            via Bearer token.
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
Content-Type: application/json

{
  "workload_id": "wl_k8m2p4q7",
  "target_machine_id": "mach_9e4d8c2a",
  "mode": "live"
}`}</code>
                </pre>
              </div>
            </div>

            <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
              <div className="border-b border-border-subtle px-4 py-2">
                <span className="text-xs font-mono text-text-muted">Response — 201 Created</span>
              </div>
              <div className="p-4">
                <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                  <code>{`{
  "id": "mig_x7y8z9",
  "workload_id": "wl_k8m2p4q7",
  "source_machine_id": "mach_7f3a2b1c",
  "target_machine_id": "mach_9e4d8c2a",
  "mode": "live",
  "status": "in_progress",
  "stages": [
    { "name": "snapshot", "status": "completed" },
    { "name": "transfer", "status": "in_progress" },
    { "name": "restore",  "status": "pending" },
    { "name": "validate", "status": "pending" },
    { "name": "switch",   "status": "pending" }
  ],
  "created_at": "2026-08-20T14:30:00Z"
}`}</code>
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
