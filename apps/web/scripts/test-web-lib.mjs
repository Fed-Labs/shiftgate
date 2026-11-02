// Regression tests for the shared web lib (F27). Run from apps/web:
//   node --test scripts/test-web-lib.mjs
// Node ≥ 22.6 strips the types of the imported .ts sources directly.

import test from "node:test";
import assert from "node:assert/strict";

import {
  formatBytes,
  timeAgo,
  apiErrorCode,
  apiErrorStatus,
  isPlanRequired,
} from "../src/lib/utils.ts";
import {
  withRefreshLock,
  readPersistedTokens,
  isRotatedSuccessor,
  AUTH_STORAGE_KEY,
} from "../src/lib/cross-tab.ts";
import { api } from "../src/lib/api.ts";

/* ------------------------------------------------------------------ */
/*  formatBytes boundary safety (report 26)                            */
/* ------------------------------------------------------------------ */

test("formatBytes renders plain byte counts", () => {
  assert.equal(formatBytes(0), "0 B");
  assert.equal(formatBytes(512), "512 B");
  assert.equal(formatBytes(1024), "1 KB");
  assert.equal(formatBytes(1536), "1.5 KB");
});

test("formatBytes handles the -1 unlimited sentinel", () => {
  assert.equal(formatBytes(-1), "Unlimited");
  assert.equal(formatBytes(-1, 2), "Unlimited");
});

test("formatBytes never renders NaN/undefined garbage", () => {
  for (const bad of [NaN, Infinity, -Infinity, Number.NaN, -7, 0.5, 1e30]) {
    const out = formatBytes(bad);
    assert.ok(!out.includes("NaN"), `${bad} -> ${out}`);
    assert.ok(!out.includes("undefined"), `${bad} -> ${out}`);
  }
  assert.equal(formatBytes(NaN), "—");
  assert.equal(formatBytes(Infinity), "—");
  assert.equal(formatBytes(-7), "—");
  assert.equal(formatBytes(0.5), "<1 B");
});

test("formatBytes clamps the unit index for extreme values", () => {
  assert.ok(formatBytes(1e30).endsWith("PB"));
  assert.ok(formatBytes(1024 ** 7).endsWith("PB"));
});

/* ------------------------------------------------------------------ */
/*  timeAgo invalid dates (report 26)                                  */
/* ------------------------------------------------------------------ */

test("timeAgo renders a fallback for invalid dates", () => {
  assert.equal(timeAgo("not-a-date"), "—");
  assert.equal(timeAgo(""), "—");
  assert.equal(timeAgo(new Date("nope")), "—");
});

test("timeAgo keeps its normal boundaries", () => {
  assert.equal(timeAgo(new Date(Date.now() - 5_000)), "just now");
  assert.ok(timeAgo(new Date(Date.now() - 3 * 60_000)).endsWith("m ago"));
  assert.ok(timeAgo(new Date(Date.now() + 3 * 60_000)).startsWith("in "));
});

/* ------------------------------------------------------------------ */
/*  api error normalization (report 26)                                */
/* ------------------------------------------------------------------ */

function stubFetch(handler) {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url: String(url), init });
    return handler(String(url), init);
  };
  return calls;
}

function jsonRes(status, body, statusText = "") {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
    statusText,
  });
}

test("api normalizes a JSON null error body instead of throwing TypeError", async () => {
  stubFetch(() =>
    new Response("null", { status: 500, headers: { "Content-Type": "application/json" }, statusText: "Server Error" })
  );
  await assert.rejects(
    () => api.listPlans(),
    (err) => {
      assert.equal(apiErrorStatus(err), 500);
      assert.equal(apiErrorCode(err), "unknown");
      assert.equal(err.message, "Server Error");
      return true;
    }
  );
});

test("api treats an empty 2xx body as undefined, not a SyntaxError", async () => {
  stubFetch(() => new Response("", { status: 200 }));
  assert.equal(await api.listPlans(), undefined);
});

test("api parses a normal 2xx JSON body", async () => {
  stubFetch(() => jsonRes(200, [{ key: "free" }]));
  assert.deepEqual(await api.listPlans(), [{ key: "free" }]);
});

test("api maps fetch aborts/timeouts to typed errors", async () => {
  stubFetch(async () => {
    const err = new Error("The operation timed out.");
    err.name = "TimeoutError";
    throw err;
  });
  await assert.rejects(
    () => api.listPlans(),
    (err) => {
      assert.equal(apiErrorCode(err), "REQUEST_TIMEOUT");
      return true;
    }
  );

  stubFetch(async () => {
    const err = new Error("boom");
    err.name = "TypeError";
    throw err;
  });
  await assert.rejects(
    () => api.listPlans(),
    (err) => {
      assert.equal(apiErrorCode(err), "NETWORK_ERROR");
      assert.equal(apiErrorStatus(err), 0);
      return true;
    }
  );
});

test("api merges a Headers instance into the auth headers", async () => {
  const calls = stubFetch(() => jsonRes(200, []));
  api.setToken("tok-123");
  await api.request("/v1/x", { headers: new Headers({ "X-Custom": "yes" }) });
  const headers = calls[0].init.headers;
  assert.ok(headers instanceof Headers);
  assert.equal(headers.get("X-Custom"), "yes");
  assert.equal(headers.get("Authorization"), "Bearer tok-123");
  assert.equal(headers.get("Content-Type"), "application/json");
  api.setToken(null);
});

test("api URL-encodes workload ids in checkpoint queries", async () => {
  const calls = stubFetch(() => jsonRes(200, []));
  await api.listCheckpoints("org-1", "wl id&x=1");
  assert.ok(calls[0].url.includes("workload_id=wl+id%26x%3D1"));
});

test("apiError helpers tolerate non-objects", () => {
  assert.equal(apiErrorCode(null), null);
  assert.equal(apiErrorStatus("nope"), null);
  assert.equal(isPlanRequired(undefined), false);
});

/* ------------------------------------------------------------------ */
/*  cross-tab refresh coordination (report 35)                         */
/* ------------------------------------------------------------------ */

function fakeStorage() {
  const map = new Map();
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, String(v)),
    removeItem: (k) => map.delete(k),
  };
}

test("isRotatedSuccessor adopts only rotated, not-shorter tokens", () => {
  const base = {
    session_id: "s",
    access_token: "a1",
    token_type: "Bearer",
    expires_at: new Date(Date.now() + 60_000).toISOString(),
  };
  assert.equal(isRotatedSuccessor(base, { ...base }), false);
  assert.equal(
    isRotatedSuccessor(base, { ...base, access_token: "a2" }),
    true
  );
  assert.equal(
    isRotatedSuccessor(
      { ...base, expires_at: new Date(Date.now() + 60_000).toISOString() },
      { ...base, access_token: "a2", expires_at: new Date(Date.now() + 10_000).toISOString() }
    ),
    false
  );
});

test("readPersistedTokens parses the zustand persist envelope", () => {
  const storage = fakeStorage();
  globalThis.localStorage = storage;
  assert.equal(readPersistedTokens(), null);
  storage.setItem(
    AUTH_STORAGE_KEY,
    JSON.stringify({ state: { tokens: { access_token: "a1", expires_at: "2030-01-01T00:00:00Z" } }, version: 0 })
  );
  assert.equal(readPersistedTokens()?.access_token, "a1");
  storage.setItem(AUTH_STORAGE_KEY, "{not json");
  assert.equal(readPersistedTokens(), null);
  delete globalThis.localStorage;
});

test("withRefreshLock serializes fallback-lock critical sections", async () => {
  globalThis.localStorage = fakeStorage();
  let concurrent = 0;
  let maxConcurrent = 0;
  const job = () =>
    withRefreshLock(async () => {
      concurrent += 1;
      maxConcurrent = Math.max(maxConcurrent, concurrent);
      await new Promise((resolve) => setTimeout(resolve, 30));
      concurrent -= 1;
    });
  await Promise.all([job(), job(), job()]);
  assert.equal(maxConcurrent, 1);
  delete globalThis.localStorage;
});
