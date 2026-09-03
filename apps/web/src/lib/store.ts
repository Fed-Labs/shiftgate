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
  setPrincipal: (p: Principal) => void;
  logout: () => Promise<void>;
  refreshSession: () => Promise<boolean>;
  hydrate: () => Promise<void>;
}

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

      setPrincipal: (p) => set({ principal: p }),

      logout: async () => {
        try { await api.logout(); } catch { /* ignore */ }
        api.setToken(null);
        set({ user: null, tokens: null, organization: null, principal: null, isAuthenticated: false });
      },

      refreshSession: async () => {
        const { tokens } = get();
        if (!tokens) return false;
        try {
          const newTokens = await api.refresh(tokens.refresh_token);
          api.setToken(newTokens.access_token);
          set({ tokens: newTokens });
          return true;
        } catch {
          api.setToken(null);
          set({ user: null, tokens: null, organization: null, principal: null, isAuthenticated: false });
          return false;
        }
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
