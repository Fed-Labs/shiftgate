// Cross-tab session coordination.
//
// The control plane rotates the refresh cookie on every use and treats a
// second presentation of a retired digest as reuse (revoking the whole
// session), so at most one tab may refresh at a time. This module serializes
// refresh across tabs (Web Locks API, with a localStorage mutex fallback).
// Auth tokens are intentionally never read from persisted browser storage.

export const REFRESH_LOCK_NAME = "shiftgate.session.refresh";
export const AUTH_STORAGE_KEY = "shiftgate-auth";

const FALLBACK_LOCK_KEY = "shiftgate-refresh-lock";
const FALLBACK_LOCK_TTL_MS = 10_000;
const FALLBACK_LOCK_WAIT_MS = 8_000;

type LocksManager = {
  request: <T>(name: string, callback: () => Promise<T> | T) => Promise<T>;
};

function webLocks(): LocksManager | null {
  if (typeof navigator === "undefined") return null;
  const locks = (navigator as { locks?: LocksManager }).locks;
  return locks && typeof locks.request === "function" ? locks : null;
}

function fallbackStorage(): Storage | null {
  try {
    return typeof localStorage !== "undefined" ? localStorage : null;
  } catch {
    return null;
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function withFallbackLock<T>(fn: () => Promise<T> | T): Promise<T> {
  const storage = fallbackStorage();
  if (!storage) return fn();
  const owner = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  const deadline = Date.now() + FALLBACK_LOCK_WAIT_MS;
  for (;;) {
    let held = false;
    try {
      const raw = storage.getItem(FALLBACK_LOCK_KEY);
      const current = raw ? (JSON.parse(raw) as { owner?: string; until?: number }) : null;
      const free = !current || current.owner === owner || (current.until ?? 0) <= Date.now();
      if (free) {
        storage.setItem(
          FALLBACK_LOCK_KEY,
          JSON.stringify({ owner, until: Date.now() + FALLBACK_LOCK_TTL_MS })
        );
        const confirmRaw = storage.getItem(FALLBACK_LOCK_KEY);
        const confirm = confirmRaw ? (JSON.parse(confirmRaw) as { owner?: string }) : null;
        held = confirm?.owner === owner;
      }
    } catch {
      // Storage unavailable/racy: better to run unlocked than to stall.
      held = true;
    }
    if (held) {
      try {
        return await fn();
      } finally {
        try {
          const raw = storage.getItem(FALLBACK_LOCK_KEY);
          const current = raw ? (JSON.parse(raw) as { owner?: string }) : null;
          if (current?.owner === owner) storage.removeItem(FALLBACK_LOCK_KEY);
        } catch {
          /* ignore */
        }
      }
    }
    if (Date.now() >= deadline) return fn();
    await sleep(120);
  }
}

/** Run `fn` while holding the single cross-tab session-refresh lock. */
export function withRefreshLock<T>(fn: () => Promise<T> | T): Promise<T> {
  const locks = webLocks();
  if (locks) return locks.request(REFRESH_LOCK_NAME, fn);
  return withFallbackLock(fn);
}
