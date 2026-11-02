// Persistent client state: control-plane connection, session tokens, chosen
// organization. Tokens live in localStorage — acceptable here because the
// WebView loads only this app's bundled content (it makes no network requests
// itself and renders no third-party markup), unlike the web dashboard, which
// keeps its refresh token in memory only. Tokens never leave the machine
// except to the configured control plane.

import { create } from "zustand";
import { persist } from "zustand/middleware";
import type { SessionTokens, Organization, User } from "./control";
import { control, requestRaw } from "./control";
import { backend, type BackendConfig } from "./backend";

// The platform's control plane — the same endpoint the CLI defaults to
// (DefaultControlPlaneURL in internal/config/config.go). Sign-in works with
// no prior configuration; a private deployment overrides it in Settings.
export const DEFAULT_CONTROL_URL = "https://shiftgate.dev";

/** The URL requests go to: the saved override, or the platform default. */
export function effectiveControlURL(controlPlaneURL: string): string {
  return (controlPlaneURL || DEFAULT_CONTROL_URL).replace(/\/$/, "");
}

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
  revalidateOrganizations: () => Promise<void>;
}

const STORAGE_KEY = "shift-desktop-auth";

// The single in-flight token refresh; see refresh().
let refreshInFlight: Promise<boolean> | null = null;

function pickOrganization(
  organizations: Organization[],
  previousID: string | undefined
): Organization | null {
  // Keep the operator's choice across re-login and revalidation; fall back to
  // the first organization only when it is gone.
  return organizations.find((org) => org.id === previousID) ?? organizations[0] ?? null;
}

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
        const previousOrgID = get().organization?.id;
        const { user, tokens } = await control.login(email, password);
        // The session is live the moment tokens land: drop the previous
        // account's organization before any request can run with the new
        // tokens against the old org.
        set({ user, tokens, organization: null, organizations: [] });
        let organizations: Organization[];
        try {
          organizations = await control.organizations();
        } catch (error) {
          const reason = error instanceof Error ? error.message : String(error);
          throw new Error(
            `SIGNED_IN_ORG_LOOKUP_FAILED: signed in, but the organization list could not be loaded — ${reason}. ` +
              "Your session is active; retry from Settings."
          );
        }
        set({ organizations, organization: pickOrganization(organizations, previousOrgID) });
      },

      refresh: async () => {
        // One refresh at a time: the control plane rotates the refresh token
        // on use and treats a replay of the previous one as theft (session
        // revoked). Two concurrent 401s — machines + workloads failing
        // together — must share a single rotation.
        if (refreshInFlight) return refreshInFlight;
        const { tokens } = get();
        if (!tokens?.refresh_token) return false;
        refreshInFlight = (async () => {
          try {
            const response = await requestRaw(
              "POST",
              "/v1/auth/refresh",
              { refresh_token: tokens.refresh_token },
              false
            );
            if (response.status < 200 || response.status >= 300) {
              // The refresh token is dead: clear the whole session, not just
              // the tokens, so no code keeps seeing a stale organization.
              set({ tokens: null, user: null, organization: null, organizations: [] });
              return false;
            }
            const next = JSON.parse(response.body) as SessionTokens;
            set({ tokens: next });
            void get().revalidateOrganizations();
            return true;
          } catch {
            // Transient transport failure: keep the session, retry later.
            return false;
          } finally {
            refreshInFlight = null;
          }
        })();
        return refreshInFlight;
      },

      logout: async () => {
        try {
          await control.logout();
        } catch {
          // A failed remote logout must not strand local state: the local
          // session ends either way.
        }
        set({ tokens: null, user: null, organization: null, organizations: [] });
      },

      selectOrganization: (organization) => set({ organization }),

      revalidateOrganizations: async () => {
        const { tokens, organization } = get();
        if (!tokens) return;
        try {
          const organizations = await control.organizations();
          set({ organizations, organization: pickOrganization(organizations, organization?.id) });
        } catch {
          // Keep the last known list; the next refresh or sign-in retries.
        }
      },
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
