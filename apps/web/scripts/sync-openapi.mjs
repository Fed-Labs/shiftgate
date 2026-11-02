// Copies the canonical OpenAPI spec (api/openapi/shift.yaml) into the web
// app's public directory so /api/openapi.yaml resolves, and fails loudly if
// the two ever disagree. The canonical spec is the source of truth — never
// edit the copy. (Node's fs.copyFileSync is enough; no runtime dependency.)
//
// Drift gate: after the copy, src/lib/types.ts is checked against the spec
// (named enum unions + object property contracts). The hand-maintained client
// types must not silently drift from the spec again — a mismatch fails the
// sync, which fails `npm run dev` / `npm run build` / `npm run validate-content`.
import { copyFileSync, mkdirSync, readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { checkTypesDrift } from "./check-types-drift.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const source = resolve(here, "../../../api/openapi/shift.yaml");
const target = resolve(here, "../public/api/openapi.yaml");

try {
  // public/api does not exist in a fresh checkout; create it on demand.
  mkdirSync(dirname(target), { recursive: true });
  copyFileSync(source, target);
  // Verify the copy is byte-identical — a copy that silently wrote nothing
  // would publish a stale spec.
  if (readFileSync(source).equals(readFileSync(target))) {
    console.log(`synced openapi spec -> public/api/openapi.yaml (${readFileSync(target).length} bytes)`);
  } else {
    console.error("openapi sync: copied file does not match the source");
    process.exitCode = 1;
  }
} catch (error) {
  console.error(`openapi sync failed: ${error.message}`);
  process.exitCode = 1;
}

try {
  const drift = checkTypesDrift();
  if (drift.length) {
    console.error("openapi sync: src/lib/types.ts drifts from api/openapi/shift.yaml:");
    for (const finding of drift) console.error(`  ${finding}`);
    console.error("the spec is the source of truth — align the generated client types (or the spec) before building");
    process.exitCode = 1;
  } else {
    console.log("types drift gate: src/lib/types.ts matches api/openapi/shift.yaml");
  }
} catch (error) {
  console.error(`types drift gate failed to run: ${error.message}`);
  process.exitCode = 1;
}
