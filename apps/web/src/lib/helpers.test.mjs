// F25 regression tests for the dashboard's pure view logic (migration
// progress contract, outcome classification, workload status normalization,
// action gating). Plain node:test — apps/web has no test runner installed and
// the fix wave forbids npm install, so these run with:
//   node --test src/lib/helpers.test.mjs
// and live as .mjs to stay outside the tsconfig project.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  migrationIsActive,
  migrationOutcome,
  readMigrationProgress,
  resolveMigrationRoute,
  stageGroupIndex,
} from "./migration-view.ts";
import {
  actionsForState,
  isDeletedWorkload,
  normalizeWorkloadState,
  primaryWorkloadAction,
  timeoutForAction,
} from "./workload-status.ts";
import {
  cleanGithubSearch,
  githubReturnMessage,
  isGithubUnavailableCode,
  readGithubReturn,
  takeGithubReturn,
} from "./github-auth.ts";

function job(partial) {
  return {
    id: "mig-1",
    organization_id: "org-1",
    workload_id: "wl-1",
    source_machine_id: "src",
    destination_machine_id: "dst",
    mode: "cold",
    status: "running",
    progress: {},
    attempt_count: 0,
    managed: true,
    cancel_requested: false,
    created_by: "u-1",
    created_at: "2026-10-21T10:00:00Z",
    updated_at: "2026-10-21T10:00:00Z",
    completed_at: null,
    ...partial,
  };
}

test("reads the control-plane progress contract keys", () => {
  const out = readMigrationProgress({
    current_stage: "TRANSFER",
    percent: 42.5,
    downtime_ms: 1200,
    transfer_speed: 1048576,
    deduplicated_bytes: 7,
    transferred_bytes: 100,
    total_state_bytes: 200,
    agent_id: "agent-x",
  });
  assert.equal(out.stage, "TRANSFER");
  assert.equal(out.percent, 42.5);
  assert.equal(out.downtimeMs, 1200);
  assert.equal(out.transferSpeed, 1048576);
  assert.equal(out.deduplicatedBytes, 7);
  assert.equal(out.transferredBytes, 100);
  assert.equal(out.totalStateBytes, 200);
});

test("falls back to legacy keys and scales the 0..1 fraction to percent", () => {
  const out = readMigrationProgress({ stage: "SNAPSHOT", progress: 0.5 });
  assert.equal(out.stage, "SNAPSHOT");
  assert.equal(out.percent, 50);
});

test("clamps out-of-range percents and survives an empty document", () => {
  assert.equal(readMigrationProgress({ percent: 250 }).percent, 100);
  assert.equal(readMigrationProgress({ percent: -4 }).percent, 0);
  const empty = readMigrationProgress(undefined);
  assert.equal(empty.stage, "");
  assert.equal(empty.percent, 0);
  assert.equal(empty.downtimeMs, undefined);
});

test("stage groups cover the rollback path", () => {
  assert.equal(stageGroupIndex("TRANSFER"), 1);
  assert.equal(stageGroupIndex("ROLLED_BACK"), 4);
  assert.equal(stageGroupIndex("NOPE"), -1);
});

test("migrationOutcome distinguishes rolled-back from needs-operator", () => {
  assert.deepEqual(migrationOutcome(job({ status: "running" })), { kind: "active" });
  assert.deepEqual(migrationOutcome(job({ status: "completed" })), { kind: "completed" });

  const cleanup = migrationOutcome(
    job({ status: "completed", error_message: "destination committed; source remains preserved because cleanup failed" })
  );
  assert.equal(cleanup.kind, "completed-with-warning");

  const rolledBack = migrationOutcome(
    job({ status: "failed", progress: { stage: "ROLLED_BACK" }, error_message: "migration rolled back; source workload preserved" })
  );
  assert.deepEqual(rolledBack, { kind: "rolled-back", sourcePreserved: true });

  const needsOperator = migrationOutcome(
    job({ status: "failed", progress: { stage: "FAILED" }, error_message: "restore failed; rollback incomplete: destination rollback error" })
  );
  assert.deepEqual(needsOperator, { kind: "needs-operator", rollbackIncomplete: true });

  // The blanket "nothing was lost" reassurance is only true for a clean
  // rollback: a failed job with no stage data must never claim it.
  const unknown = migrationOutcome(job({ status: "failed", error_message: "transfer exploded" }));
  assert.equal(unknown.kind, "needs-operator");
  assert.equal(unknown.rollbackIncomplete, false);
});

test("cancelled jobs never claim the source is safe", () => {
  const settled = migrationOutcome(job({ status: "cancelled", progress: { stage: "ROLLED_BACK" } }));
  assert.equal(settled.kind, "cancelled");
  assert.equal(settled.sourceMayRemain, false);
  const unsettled = migrationOutcome(job({ status: "cancelled", progress: { stage: "TRANSFER" } }));
  assert.equal(unsettled.sourceMayRemain, true);
});

test("route resolution prefers the record, then the relay endpoint", () => {
  assert.equal(resolveMigrationRoute(job({ route: "relay" })), "relay");
  assert.equal(resolveMigrationRoute(job({ relay_endpoint: "relay.example:8443" })), "relay");
  assert.equal(resolveMigrationRoute(job({})), undefined);
});

test("active predicate matches the job statuses that keep polling", () => {
  assert.equal(migrationIsActive(job({ status: "queued" })), true);
  assert.equal(migrationIsActive(job({ status: "running" })), true);
  assert.equal(migrationIsActive(job({ status: "failed" })), false);
});

test("normalizes the state key and the legacy status key to one enum", () => {
  assert.equal(normalizeWorkloadState({ state: "running" }), "running");
  assert.equal(normalizeWorkloadState({ status: "registered" }), "registered");
  assert.equal(normalizeWorkloadState({ state: " RUNNING " }), "running");
  assert.equal(normalizeWorkloadState(undefined), "unknown");
  assert.equal(normalizeWorkloadState({}), "unknown");
});

test("maps legacy restore-record transaction states to the normalized enum", () => {
  // The old restore dispatch wrote the checkpoint restore state into the
  // workload state field; both fleet views must agree on what it means.
  assert.equal(normalizeWorkloadState({ state: "COMMITTED" }), "running");
  assert.equal(normalizeWorkloadState({ state: "VALIDATED" }), "restoring");
  assert.equal(normalizeWorkloadState({ state: "PREPARING" }), "restoring");
  assert.equal(normalizeWorkloadState({ state: "ROLLED_BACK" }), "failed");
  assert.equal(normalizeWorkloadState({ state: "FAILED" }), "failed");
});

test("deleted tombstones are recognized and hidden from the fleet", () => {
  assert.equal(normalizeWorkloadState({ state: "deleted" }), "deleted");
  assert.equal(isDeletedWorkload({ state: "deleted" }), true);
  assert.equal(isDeletedWorkload({ state: "running" }), false);
});

test("action table never offers a state the agent rejects", () => {
  const keys = (state) => actionsForState(state).map((a) => a.key);
  assert.deepEqual(keys("registered"), ["start", "delete"]);
  assert.deepEqual(keys("running"), ["pause", "checkpoint", "stop"]);
  assert.deepEqual(keys("paused"), ["resume", "stop"]);
  assert.deepEqual(keys("checkpointed"), ["start"]);
  assert.deepEqual(keys("stopped"), ["start", "delete"]);
  assert.deepEqual(keys("failed"), ["delete"]);
  // In-flight and terminal states get no lifecycle command at all — the old
  // detail page offered RESUME here and the agent refused it every time.
  assert.deepEqual(keys("starting"), []);
  assert.deepEqual(keys("restoring"), []);
  assert.deepEqual(keys("deleted"), []);
});

test("primary action follows the three-state control per state", () => {
  assert.equal(primaryWorkloadAction("registered")?.key, "start");
  assert.equal(primaryWorkloadAction("running")?.key, "pause");
  assert.equal(primaryWorkloadAction("paused")?.key, "resume");
  assert.equal(primaryWorkloadAction("checkpointed")?.key, "start");
  assert.equal(primaryWorkloadAction("failed"), null);
  assert.equal(primaryWorkloadAction("starting"), null);
});

test("checkpoint dispatch gets the long timeout, everything else the short one", () => {
  assert.equal(timeoutForAction("checkpoint"), 120);
  assert.equal(timeoutForAction("start"), 30);
  assert.equal(timeoutForAction("stop"), 30);
  assert.equal(timeoutForAction("migrate"), 30);
});

test("destructive actions keep their confirm labels", () => {
  const stop = actionsForState("running").find((a) => a.key === "stop");
  const del = actionsForState("failed").find((a) => a.key === "delete");
  assert.equal(stop?.confirm, "CONFIRM STOP");
  assert.equal(del?.confirm, "CONFIRM DELETE");
  assert.equal(stop?.danger, true);
});

test("github return query parses to a one-time code or a typed error", () => {
  assert.deepEqual(readGithubReturn("?github_code=abc123"), { kind: "code", code: "abc123" });
  assert.deepEqual(readGithubReturn("?error=github_auth_failed"), {
    kind: "error",
    code: "github_auth_failed",
  });
  assert.equal(readGithubReturn("?code=oidc-code&state=s"), null);
  assert.equal(readGithubReturn("?error=access_denied&state=s"), null);
  assert.equal(readGithubReturn(""), null);
});

test("github return cleanup strips only the flow params", () => {
  assert.equal(cleanGithubSearch("?github_code=abc&ref=TEAM"), "?ref=TEAM");
  assert.equal(cleanGithubSearch("?error=github_org_denied"), "");
  assert.equal(cleanGithubSearch("?github_code=abc&error=github_auth_failed"), "");
  assert.equal(cleanGithubSearch("?error=access_denied&state=s"), "?error=access_denied&state=s");
  assert.equal(cleanGithubSearch(""), "");
});

test("github return handling happens exactly once per document", () => {
  assert.deepEqual(takeGithubReturn("?github_code=once"), { kind: "code", code: "once" });
  assert.equal(takeGithubReturn("?github_code=twice"), null);
});

test("github error codes map to typed messages without raw dumps", () => {
  assert.match(githubReturnMessage("github_auth_failed"), /cancelled/i);
  assert.match(githubReturnMessage("github_email_not_verified"), /verified/i);
  assert.match(githubReturnMessage("github_org_denied"), /organization/i);
  assert.equal(githubReturnMessage("github_something_new"), "GitHub sign-in failed (github_something_new).");
});

test("unconfigured and unrouted github endpoints share the deployment-state message", () => {
  assert.equal(isGithubUnavailableCode("GITHUB_NOT_CONFIGURED"), true);
  assert.equal(isGithubUnavailableCode("unknown"), true);
  assert.equal(isGithubUnavailableCode("NETWORK_ERROR"), false);
  assert.equal(isGithubUnavailableCode("github_auth_failed"), false);
  assert.equal(isGithubUnavailableCode(null), false);
});
