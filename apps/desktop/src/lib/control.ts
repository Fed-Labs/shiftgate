// Control-plane client. Requests run through the Rust `control_request`
// command, which pins every target to the configured control plane (https,
// verified; plain http only for loopback) — the WebView itself makes no
// network connections and holds no HTTP permission. JSON shapes match the
// web dashboard.

import { invoke } from "./backend";
import { useAuth } from "./store";

export interface ControlMachine {
  id: string;
  organization_id: string;
  machine_id: string;
  name: string;
  agent_url: string;
  capabilities: Record<string, unknown>;
  status: "online" | "offline" | "draining" | "disabled";
  last_seen_at: string | null;
  created_at: string;
  updated_at: string;
}

export interface Organization {
  id: string;
  name: string;
  role: string;
  created_at: string;
}

export interface User {
  id: string;
  email: string;
  display_name: string;
}

export interface SessionTokens {
  session_id: string;
  access_token: string;
  refresh_token: string;
  token_type: "Bearer";
  expires_at: string;
}

export class ControlError extends Error {
  code: string;
  status: number;
  constructor(code: string, message: string, status: number) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

/** The raw control-plane exchange: status + body text, nothing interpreted. */
export interface ControlResponse {
  status: number;
  body: string;
}

/**
 * One request against the configured control plane. The Rust side owns the
 * target URL; this only names the path. Errors carry "<CODE>: <message>".
 */
export function requestRaw(
  method: string,
  path: string,
  body?: unknown,
  authenticated = true
): Promise<ControlResponse> {
  const { tokens } = useAuth.getState();
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (authenticated && tokens?.access_token) {
    headers.authorization = `Bearer ${tokens.access_token}`;
  }
  return invoke<ControlResponse>("control_request", {
    method,
    path,
    body: body === undefined ? null : body,
    headers,
  });
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  retry = true
): Promise<T> {
  const { tokens, refresh } = useAuth.getState();
  const response = await requestRaw(method, path, body);
  if (response.status === 401 && retry && tokens?.refresh_token) {
    // Access tokens are short-lived; one refresh, then replay the request.
    const refreshed = await refresh();
    if (refreshed) return request<T>(method, path, body, false);
  }
  if (response.status === 204) return undefined as T;
  let payload: Record<string, unknown> | null = null;
  try {
    payload = response.body ? (JSON.parse(response.body) as Record<string, unknown>) : null;
  } catch {
    payload = null;
  }
  if (response.status < 200 || response.status >= 300) {
    const code = (payload?.code as string | undefined) ?? "REQUEST_FAILED";
    const message =
      (payload?.message as string | undefined) ?? `Control plane returned ${response.status}`;
    throw new ControlError(code, message, response.status);
  }
  return payload as T;
}

/** The control plane's page ceiling (database.MaxPageLimit). */
const PAGE_LIMIT = 500;
/** Safety valve against a server that never reports a short page. */
const MAX_PAGES = 20;

/**
 * Walk a paginated list endpoint to completion. Without paging, the desktop
 * silently showed only the first 100 machines of a fleet.
 */
async function requestAll<T>(path: string): Promise<T[]> {
  const all: T[] = [];
  for (let page = 0; page < MAX_PAGES; page += 1) {
    const separator = path.includes("?") ? "&" : "?";
    const values = await request<T[]>(
      "GET",
      `${path}${separator}limit=${PAGE_LIMIT}&offset=${all.length}`
    );
    all.push(...values);
    if (values.length < PAGE_LIMIT) break;
  }
  return all;
}

export const control = {
  login: async (email: string, password: string) => {
    const result = await request<{ user: User; tokens: SessionTokens }>(
      "POST",
      "/v1/auth/login",
      { email, password }
    );
    return result;
  },
  logout: () => request<void>("POST", "/v1/auth/logout"),
  health: () => request<{ status: string; version?: string }>("GET", "/health"),
  organizations: () => requestAll<Organization>("/v1/organizations"),
  machines: (organizationID: string) =>
    requestAll<ControlMachine>(`/v1/organizations/${organizationID}/machines`),
};
