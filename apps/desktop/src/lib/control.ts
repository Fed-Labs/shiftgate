// Control-plane client. Requests run through the official Tauri HTTP plugin
// (its Rust side performs the transport, including TLS — the WebView itself
// has no network access), with the same JSON shapes as the web dashboard.

import { fetch } from "@tauri-apps/plugin-http";
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

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  retry = true
): Promise<T> {
  const { controlPlaneURL, tokens, refresh } = useAuth.getState();
  const base = controlPlaneURL.replace(/\/$/, "");
  if (!base) throw new ControlError("NOT_CONFIGURED", "No control plane is configured.", 0);
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (tokens?.access_token) headers.Authorization = `Bearer ${tokens.access_token}`;
  const response = await fetch(`${base}${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.status === 401 && retry && tokens?.refresh_token) {
    // Access tokens are short-lived; one refresh, then replay the request.
    const refreshed = await refresh();
    if (refreshed) return request<T>(method, path, body, false);
  }
  if (response.status === 204) return undefined as T;
  const payload = await response.json().catch(() => null);
  if (!response.ok) {
    const code = payload?.code ?? "REQUEST_FAILED";
    const message = payload?.message ?? `Control plane returned ${response.status}`;
    throw new ControlError(code, message, response.status);
  }
  return payload as T;
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
  organizations: () => request<Organization[]>("GET", "/v1/organizations"),
  machines: (organizationID: string) =>
    request<ControlMachine[]>("GET", `/v1/organizations/${organizationID}/machines`),
};
