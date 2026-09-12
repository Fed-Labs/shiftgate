import { create } from "zustand";
import { persist } from "zustand/middleware";
import type { User, Organization, SessionTokens, Principal } from "./types";
import api from "./api";

interface AuthState {
  user: User | null;
  organization: Organization | null;
  tokens: SessionTokens | null;
  principal: Principal | null;
  isAuthenticated: boolean;
  isLoading: boolean;

  setAuth: (user: User, tokens: SessionTokens, organization?: Organization) => void;
  restoreOrganization: () => Promise<void>;
  setPrincipal: (p: Principal) => void;
  logout: () => Promise<void>;
  refreshSession: () => Promise<boolean>;
  hydrate: () => Promise<void>;
}

// The single in-flight token refresh; see refreshSession.
let refreshInFlight: Promise<boolean> | null = null;

export const useAuth = create<AuthState>()(
  persist(
    (set, get) => ({
      user: null,
      organization: null,
      tokens: null,
      principal: null,
      isAuthenticated: false,
      isLoading: true,

      setAuth: (user, tokens, organization) => {
        api.setToken(tokens.access_token);
        set({ user, tokens, organization, isAuthenticated: true });
      },

      // Only signup's response names an organization; login's does not, so a
      // plain sign-in leaves the store without one and every organization page
      // idles on an empty id. Adopt the user's first organization — an
      // organization already in the store (signup, an earlier restore) wins by
      // never refetching.
      restoreOrganization: async () => {
        if (get().organization) return;
        try {
          const organizations = await api.listOrganizations();
          if (organizations.length > 0) set({ organization: organizations[0] });
        } catch { /* unhandled: pages render their empty states */ }
      },

      setPrincipal: (p) => set({ principal: p }),

      logout: async () => {
        try { await api.logout(); } catch { /* ignore */ }
        api.setToken(null);
        set({ user: null, tokens: null, organization: null, principal: null, isAuthenticated: false });
      },

      refreshSession: async () => {
        const { tokens } = get();
        if (!tokens) return false;
        // One refresh at a time: the control plane rotates the refresh token
        // on use, so a second concurrent call would send a token that was just
        // consumed, fail with 401, and sign the user out. Concurrent callers —
        // React's dev-mode double mount, two components hydrating, another
        // tab — share the single request's result.
        if (refreshInFlight) return refreshInFlight;
        refreshInFlight = (async () => {
          try {
            const newTokens = await api.refresh(tokens.refresh_token);
            api.setToken(newTokens.access_token);
            set({ tokens: newTokens });
            return true;
          } catch {
            api.setToken(null);
            set({ user: null, tokens: null, organization: null, principal: null, isAuthenticated: false });
            return false;
          } finally {
            refreshInFlight = null;
          }
        })();
        return refreshInFlight;
      },

      hydrate: async () => {
        const { tokens, refreshSession } = get();
        if (!tokens) {
          set({ isLoading: false });
          return;
        }
        const ok = await refreshSession();
        if (ok) {
          try {
            const principal = await api.me();
            set({ principal });
          } catch { /* ignore */ }
          await get().restoreOrganization();
        }
        set({ isLoading: false });
      },
    }),
    {
      name: "shiftgate-auth",
      partialize: (state) => ({
        user: state.user,
        organization: state.organization,
        tokens: state.tokens,
      }),
    }
  )
);
