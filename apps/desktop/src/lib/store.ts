// Persistent client state: control-plane connection, session tokens, chosen
// organization. Tokens live in localStorage — the same trust model as the web
// dashboard (bearer credentials held by the client, short-lived access token,
// transactional refresh) — and never leave the machine except to the
// configured control plane.

import { create } from "zustand";
import { persist } from "zustand/middleware";
import type { SessionTokens, Organization, User } from "./control";
import { control } from "./control";
import { backend, type BackendConfig } from "./backend";

interface AuthState {
  controlPlaneURL: string;
  agentSocket: string;
  tokens: SessionTokens | null;
  user: User | null;
  organization: Organization | null;
  organizations: Organization[];
  configure: (controlPlaneURL: string, agentSocket: string) => void;
  login: (email: string, password: string) => Promise<void>;
  refresh: () => Promise<boolean>;
  logout: () => Promise<void>;
  selectOrganization: (organization: Organization) => void;
}

const STORAGE_KEY = "shift-desktop-auth";

export const useAuth = create<AuthState>()(
  persist(
    (set, get) => ({
      controlPlaneURL: "",
      agentSocket: "",
      tokens: null,
      user: null,
      organization: null,
      organizations: [],

      configure: (controlPlaneURL, agentSocket) => {
        set({ controlPlaneURL: controlPlaneURL.replace(/\/$/, ""), agentSocket });
      },

      login: async (email, password) => {
        const { user, tokens } = await control.login(email, password);
        set({ user, tokens });
        const organizations = await control.organizations();
        const organization = organizations[0] ?? null;
        set({ organizations, organization });
      },

      refresh: async () => {
        const { tokens, controlPlaneURL } = get();
        if (!tokens?.refresh_token || !controlPlaneURL) return false;
        try {
          const response = await fetch(`${controlPlaneURL}/v1/auth/refresh`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ refresh_token: tokens.refresh_token }),
          });
          if (!response.ok) {
            set({ tokens: null, user: null });
            return false;
          }
          const next = (await response.json()) as SessionTokens;
          set({ tokens: next });
          return true;
        } catch {
          return false;
        }
      },

      logout: async () => {
        try {
          await control.logout();
        } finally {
          set({ tokens: null, user: null, organization: null, organizations: [] });
        }
      },

      selectOrganization: (organization) => set({ organization }),
    }),
    {
      name: STORAGE_KEY,
      partialize: (state) => ({
        controlPlaneURL: state.controlPlaneURL,
        agentSocket: state.agentSocket,
        tokens: state.tokens,
        user: state.user,
        organization: state.organization,
        organizations: state.organizations,
      }),
    }
  )
);

// The Rust side also persists the endpoints (it needs the socket path before
// any command runs); sync both directions once at startup.
export async function syncBackendConfig(): Promise<BackendConfig | null> {
  try {
    const config = await backend.config();
    const state = useAuth.getState();
    if (!state.controlPlaneURL && config.control_plane_url) {
      state.configure(config.control_plane_url, config.agent_socket);
      return config;
    }
    if (
      state.controlPlaneURL !== config.control_plane_url ||
      (state.agentSocket && state.agentSocket !== config.agent_socket)
    ) {
      return await backend.saveEndpoints(
        state.controlPlaneURL,
        state.agentSocket || config.agent_socket
      );
    }
    return config;
  } catch {
    return null;
  }
}
