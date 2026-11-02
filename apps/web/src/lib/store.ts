import { create } from "zustand";
import { persist } from "zustand/middleware";
import type { User, Organization, SessionTokens, Principal } from "./types";
import api from "./api";
import { withRefreshLock } from "./cross-tab";

interface AuthState {
  user: User | null;
  organization: Organization | null;
  organizations: Organization[];
  orgError: string | null;
  tokens: SessionTokens | null;
  principal: Principal | null;
  isAuthenticated: boolean;
  isLoading: boolean;

  setAuth: (user: User, tokens: SessionTokens, organization?: Organization) => void;
  restoreOrganization: () => Promise<void>;
  setOrganization: (organization: Organization) => void;
  setPrincipal: (p: Principal) => void;
  logout: () => Promise<void>;
  refreshSession: () => Promise<boolean>;
  hydrate: () => Promise<void>;
}

// The QueryClient lives in the root Providers; the store cannot import React
// providers, so Providers registers these hooks. Every auth transition that
// can change identity (logout, session expiry, sign-in as someone else) must
// drop the whole cache — query keys are org-scoped but shared keys and
// role-gated data collide across users on a shared machine.
export interface AuthCacheHooks {
  clear: () => void;
  invalidateAll: () => void;
}

let cacheHooks: AuthCacheHooks | null = null;

export function registerAuthCacheHooks(hooks: AuthCacheHooks | null) {
  cacheHooks = hooks;
}

function clearCaches() {
  try {
    cacheHooks?.clear();
  } catch {
    /* ignore */
  }
}

function invalidateCaches() {
  try {
    cacheHooks?.invalidateAll();
  } catch {
    /* ignore */
  }
}

// The single in-flight token refresh within this tab; cross-tab serialization
// rides withRefreshLock, so concurrent refreshers (other tabs, the proactive
// timer, the 401 replay) never present the same refresh cookie twice.
let refreshInFlight: Promise<boolean> | null = null;

// Browser sessions are anchored by the control plane's httpOnly `shift_rt`
// cookie: login/refresh strip the rotating refresh token from the response
// body, so script can never read it (S-10). Keep access tokens in memory only;
// the cookie — sent with credentials:"include" — restores a session after a
// reload without exposing a bearer token to localStorage/XSS.

function accessTokenValid(tokens: SessionTokens | null): boolean {
  if (!tokens?.access_token) return false;
  const expires = Date.parse(tokens.expires_at);
  // An unparseable expiry cannot be trusted as "never expires" — treat it as
  // expired so the 401/refresh path decides.
  return Number.isFinite(expires) && expires > Date.now() + 5_000;
}

function adoptTokens(incoming: SessionTokens): SessionTokens {
  // A cookie session's rotated refresh token comes back only in the
  // Set-Cookie header — the body copy is empty. Keep the in-memory one when
  // the response carries none (non-cookie transports).
  const merged: SessionTokens = {
    ...incoming,
    refresh_token: incoming.refresh_token || useAuth.getState().tokens?.refresh_token || "",
  };
  api.setToken(merged.access_token);
  useAuth.setState({ tokens: merged });
  scheduleProactiveRefresh(() => void useAuth.getState().refreshSession());
  return merged;
}

// Proactive refresh: rotate shortly before the access token expires, so
// interactive use never trips a 401. A silent failure leaves the 401 path to
// decide — the access token may still be good.
const REFRESH_MARGIN_MS = 60_000;
let proactiveTimer: ReturnType<typeof setTimeout> | null = null;

function scheduleProactiveRefresh(refresh: () => void) {
  if (proactiveTimer) clearTimeout(proactiveTimer);
  proactiveTimer = null;
  const token = useAuth.getState().tokens;
  if (!token) return;
  const expires = Date.parse(token.expires_at);
  if (!Number.isFinite(expires)) return;
  const delay = Math.max(expires - Date.now() - REFRESH_MARGIN_MS, 10_000);
  proactiveTimer = setTimeout(refresh, delay);
}

function stopProactiveRefresh() {
  if (proactiveTimer) clearTimeout(proactiveTimer);
  proactiveTimer = null;
}

export const useAuth = create<AuthState>()(
  persist(
    (set, get) => ({
      user: null,
      organization: null,
      organizations: [],
      orgError: null,
      tokens: null,
      principal: null,
      isAuthenticated: false,
      isLoading: true,

      setAuth: (user, tokens, organization) => {
        // A different human at the keyboard must never inherit the previous
        // user's cached tenant data.
        const previous = get().user;
        if (previous && previous.id !== user.id) clearCaches();
        api.setToken(tokens.access_token);
        set({
          user,
          tokens,
          organization,
          organizations: organization ? [organization] : [],
          orgError: null,
          isAuthenticated: true,
        });
        scheduleProactiveRefresh(() => void get().refreshSession());
      },

      // Only signup's response names an organization; login's does not, so a
      // plain sign-in leaves the store without one and every organization page
      // idles on an empty id. Always refetch the membership list so the stored
      // organization (and its role) tracks server truth — a demoted admin must
      // not keep seeing management UI — and so multi-org users can switch.
      restoreOrganization: async () => {
        try {
          const organizations = await api.listOrganizations();
          const current = get().organization;
          const refreshed = current ? organizations.find((o) => o.id === current.id) : undefined;
          set({
            organizations,
            organization: refreshed ?? current ?? organizations[0] ?? null,
            orgError: null,
          });
        } catch (error) {
          // Keep a known organization usable on transient failures, but flag
          // the failure when we have none: pages gate on `!!orgId` and would
          // otherwise render misleading empty states.
          if (!get().organization) {
            set({
              orgError:
                error && typeof error === "object" && "message" in error
                  ? String((error as { message: unknown }).message)
                  : "Could not load your organization.",
            });
          }
        }
      },

      // The org switcher: adopt the chosen organization and refetch every
      // cached query — org-scoped keys cannot serve the old tenant's data.
      setOrganization: (organization) => {
        if (get().organization?.id === organization.id) return;
        set({ organization, orgError: null });
        invalidateCaches();
      },

      setPrincipal: (p) => set({ principal: p }),

      logout: async () => {
        stopProactiveRefresh();
        // Revoke server-side first — the request must carry the current
        // bearer token — but never let the network call delay the local
        // sign-out: clear state and caches immediately, then await.
        let revoke: Promise<unknown> = Promise.resolve();
        try {
          revoke = api.logout();
        } catch {
          /* ignore */
        }
        api.setToken(null);
        set({
          user: null,
          tokens: null,
          organization: null,
          organizations: [],
          orgError: null,
          principal: null,
          isAuthenticated: false,
        });
        clearCaches();
        try {
          await revoke;
        } catch {
          /* ignore */
        }
      },

      // Refresh rides the httpOnly shift_rt cookie (credentials:"include");
      // the body refresh token is optional and empty after reload. The
      // control plane rotates the refresh cookie on use and revokes the whole
      // session on reuse detection, so exactly one refresh runs at a time —
      // across tabs too: the cross-tab lock serializes refreshers, and a
      // caller that waited behind another tab's rotation adopts the rotated
      // tokens instead of presenting the retired cookie again.
      refreshSession: async () => {
        if (refreshInFlight) return refreshInFlight;
        refreshInFlight = (async () => {
          try {
            return await withRefreshLock(async () => {
              const before = get().tokens;
              try {
                // Cookie-only refresh is supported after a reload; an in-memory
                // refresh token is retained only for non-browser transports.
                const newTokens = await api.refresh(before?.refresh_token || "");
                adoptTokens({ ...newTokens, refresh_token: newTokens.refresh_token || before?.refresh_token || "" });
                return true;
              } catch {
                // A concurrent refresher may have rotated the cookie first;
                // the next request will retry through the cookie session.
                return false;
              }
            });
          } finally {
            refreshInFlight = null;
          }
        })();
        return refreshInFlight;
      },

      hydrate: async () => {
        try {
          const { tokens, refreshSession } = get();
          // A resumed session may have no in-memory token at all. Attempt the
          // cookie-only refresh so reloads restore authentication without
          // putting an access token in localStorage.
          let ok = await refreshSession();
          if (!ok && accessTokenValid(tokens)) {
            api.setToken(tokens?.access_token ?? null);
            scheduleProactiveRefresh(() => void refreshSession());
            ok = true;
          }
          if (ok) {
            set({ isAuthenticated: true });
            try {
              const principal = await api.me();
              set({ principal });
            } catch { /* ignore */ }
            await get().restoreOrganization();
          } else {
            api.setToken(null);
            set({
              user: null,
              tokens: null,
              organization: null,
              organizations: [],
              orgError: null,
              principal: null,
              isAuthenticated: false,
            });
          }
        } catch {
          /* never reject: AppShell's hydrate() call must not go unhandled */
        } finally {
          set({ isLoading: false });
        }
      },
    }),
    {
      name: "shiftgate-auth",
      // Persist only non-sensitive display state. Access and refresh tokens
      // remain memory-only; the httpOnly cookie re-establishes the session.
      partialize: (state) => ({
        user: state.user,
        organization: state.organization,
      }),
    }
  )
);

// Tokens rotate (and sessions end) in other tabs: adopt the rotated access
// token here instead of spending the retired cookie, and follow a sign-out
// that happened elsewhere so a shared machine never keeps stale identity.
if (typeof window !== "undefined") {
  // Persisted state is intentionally non-sensitive. A storage change still
  // matters for sign-out, but tokens are never read back from localStorage.
  window.addEventListener("storage", (event) => {
    if (event.key !== "shiftgate-auth") return;
    let signedOut = !event.newValue;
    if (event.newValue) {
      try {
        const parsed = JSON.parse(event.newValue) as { state?: { user?: unknown } };
        signedOut = !parsed.state?.user;
      } catch {
        signedOut = false;
      }
    }
    if (signedOut && useAuth.getState().tokens) {
      stopProactiveRefresh();
      api.setToken(null);
      useAuth.setState({
        user: null,
        tokens: null,
        organization: null,
        organizations: [],
        orgError: null,
        principal: null,
        isAuthenticated: false,
      });
      clearCaches();
    }
  });
}

// The API client's 401 path calls back into the store: one refresh, then the
// request replays; a failed refresh ends the session and the /app layout's
// auth gate lands the user on /login.
api.setSessionHandlers(
  () => useAuth.getState().refreshSession(),
  () => {
    void useAuth.getState().logout();
  }
);
