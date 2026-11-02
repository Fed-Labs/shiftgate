// Content gate for the web surface. Fails (never passes vacuously) when:
//   - public/api/openapi.yaml drifts from api/openapi/shift.yaml
//   - src/lib/types.ts drifts from the spec (same gate as sync-openapi.mjs)
//   - public copy contains forbidden/unqualified promises (case-insensitive,
//     across ALL of src/ — app pages, marketing guides, components, lib)
//   - the spec drops a required route
//   - docs name a CLI command the dispatcher does not implement (top-level
//     dispatch only — nested subcommand case labels are NOT commands)
//   - docs use a global flag the CLI does not accept
//   - .well-known/security.txt is missing RFC 9116 fields (Contact/Expires/
//     Canonical) or its Expires is not a future date
//   - the sitemap and the app route tree disagree (missing or phantom pages)
// All checks run in one pass so a single failure never masks the rest.
// Run from anywhere: `node apps/web/scripts/validate-content.mjs`.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { checkTypesDrift } from "./check-types-drift.mjs";

const here = path.dirname(fileURLToPath(import.meta.url));
const webRoot = path.resolve(here, "..");
const repoRoot = path.resolve(webRoot, "../..");
const errors = [];

const source = fs.readFileSync(path.resolve(repoRoot, "api/openapi/shift.yaml"), "utf8");
const publicSpec = fs.readFileSync(path.resolve(webRoot, "public/api/openapi.yaml"), "utf8");
if (source !== publicSpec) errors.push("public/api/openapi.yaml is out of sync; run npm run sync-openapi");

for (const finding of checkTypesDrift()) {
  errors.push(`types drift: ${finding}`);
}

const files = [];
function walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(file);
    else if (/\.(tsx?|mdx?)$/.test(entry.name)) files.push(file);
  }
}
// Every byte of public copy: pages, marketing guides, components, and lib.
walk(path.resolve(webRoot, "src"));
const haystack = files.map((file) => fs.readFileSync(file, "utf8")).join("\n").toLowerCase();
const forbidden = [
  ["/api/v1", "public docs use the stale /api/v1 route prefix"],
  ["nothing restarted", "public copy promises restart-free behavior without qualification"],
  ["nothing is lost", "public copy promises lossless behavior without qualification"],
  ["all systems operational", "status copy claims live operations without telemetry"],
  ["status — online", "status copy claims live operations without telemetry"],
  ["target_machine", "public API example uses a stale migration field"],
  ["targetmachine", "public API example uses a stale migration field"],
  ["verify_after_restore", "public API example uses a stale migration field"],
];
for (const [needle, message] of forbidden) {
  if (haystack.includes(needle)) errors.push(message);
}

const requiredPaths = ["/health", "/ready", "/v1/organizations/{organizationID}/migrations"];
for (const route of requiredPaths) {
  if (!source.includes(`  ${route}:`)) errors.push(`OpenAPI missing required route ${route}`);
}

// Every `syntax: "shiftgate …"` string in the in-product CLI reference must
// name a command the CLI dispatcher actually implements, and every `--flag`
// must be a real global flag. The command set is parsed from the top-level
// `switch command` dispatch blocks in cmd/shift/main.go (case labels at the
// switch body's first nesting level only — subcommand switches are deeper and
// are deliberately NOT treated as commands).
const cliSource = fs.readFileSync(path.resolve(repoRoot, "cmd/shift/main.go"), "utf8");
const flagsSource = fs.readFileSync(path.resolve(repoRoot, "cmd/shift/flags.go"), "utf8");

function caseLabelsInSwitchBody(text, switchKeyword) {
  const names = [];
  let i = 0;
  while (true) {
    const start = text.indexOf(switchKeyword, i);
    if (start < 0) break;
    let j = start + switchKeyword.length;
    let depth = 1;
    while (j < text.length && depth > 0) {
      if (text[j] === "{") depth++;
      else if (text[j] === "}") depth--;
      j++;
    }
    const body = text.slice(start + switchKeyword.length, j - 1);
    let d = 0;
    let k = 0;
    while (k < body.length) {
      const c = body[k];
      if (c === "{") { d++; k++; continue; }
      if (c === "}") { d--; k++; continue; }
      if (d === 0 && body.startsWith("case ", k)) {
        const colon = body.indexOf(":", k);
        const label = body.slice(k + 5, colon);
        if (/^(\s*"[a-z-]+"\s*,?)+\s*$/.test(label)) {
          for (const m of label.matchAll(/"([a-z-]+)"/g)) names.push(m[1]);
        }
        k = colon + 1;
        continue;
      }
      k++;
    }
    i = j;
  }
  return names;
}

const dispatchCases = caseLabelsInSwitchBody(cliSource, "switch command {");
if (dispatchCases.length === 0) {
  errors.push("could not parse the top-level shiftgate command list from cmd/shift/main.go");
}
// "help" is dispatched by helpRequest() before the switch (cmd/shift/flags.go).
const cliCommands = new Set(["help", ...dispatchCases]);
const globalFlags = new Set(caseLabelsInSwitchBody(flagsSource, "switch name {"));

const cliDoc = fs.readFileSync(path.resolve(webRoot, "src/app/docs/cli/page.tsx"), "utf8");
const syntaxStrings = [...cliDoc.matchAll(/syntax: "([^"]+)"/g)].map((match) => match[1]);
const unknownSyntax = [];
for (const syntax of syntaxStrings) {
  const words = syntax.split(/\s+/);
  if (words[0] !== "shiftgate" || words.length < 2) continue;
  const command = words[1];
  if (command === "|" || command.startsWith("|")) continue;
  if (command.startsWith("--")) {
    const flag = command.replace(/^--?([a-z-]+).*/, "$1");
    if (!globalFlags.has(flag)) unknownSyntax.push(`${syntax} (unknown global flag --${flag})`);
    continue;
  }
  if (!/^[a-z][a-z-]*$/.test(command)) continue;
  if (!cliCommands.has(command)) unknownSyntax.push(syntax);
}
if (unknownSyntax.length) {
  errors.push(`web CLI reference documents commands the CLI does not implement: ${unknownSyntax.join("; ")}`);
}

// Every `shiftgate <command>` invocation anywhere in the website source — copy-
// paste examples in code blocks as well as inline prose, in docs AND marketing
// guides/use-cases/compare pages — must name a real command, so an example that
// would fail at the prompt fails this test instead.
let invocationCount = 0;
const unknownInvocations = [];
const invocationPattern = /(?:^[\t ]*(?:sudo[\t ]+)?|[>'"`(])shiftgate[\t ]+([a-z][a-z-]*)/gm;
for (const file of files) {
  const text = fs.readFileSync(file, "utf8");
  for (const match of text.matchAll(invocationPattern)) {
    invocationCount++;
    if (!cliCommands.has(match[1])) {
      unknownInvocations.push(`${path.relative(webRoot, file)}: shiftgate ${match[1]}`);
    }
  }
}
if (unknownInvocations.length) {
  errors.push(`website shows commands the CLI does not implement: ${unknownInvocations.join("; ")}`);
}

// security.txt (RFC 9116) is the canonical security contact path; it must ship
// with the build and validators treat a missing/expired Expires as invalid.
const securityTxt = fs.readFileSync(path.resolve(webRoot, "public/.well-known/security.txt"), "utf8");
for (const field of ["Contact:", "Expires:", "Canonical:"]) {
  if (!new RegExp(`^${field}`, "m").test(securityTxt)) {
    errors.push(`public/.well-known/security.txt is missing the RFC 9116 ${field} field`);
  }
}
const expires = securityTxt.match(/^Expires:\s*(.+)$/m);
if (!expires || Number.isNaN(Date.parse(expires[1])) || Date.parse(expires[1]) <= Date.now()) {
  errors.push("public/.well-known/security.txt Expires must be a future RFC 9116 timestamp");
}

// Sitemap <-> route tree: every public page route must be advertised in
// sitemap.ts and every advertised path must be a real route. /app (dashboard),
// /login, /signup (robots-disallowed) and dynamic segments are excluded.
const routePaths = [];
(function walkRoutes(dir, prefix) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (!entry.isDirectory()) continue;
    const child = path.join(dir, entry.name);
    const route = `${prefix}/${entry.name}`;
    if (fs.existsSync(path.join(child, "page.tsx"))) routePaths.push(route);
    walkRoutes(child, route);
  }
})(path.resolve(webRoot, "src/app"), "");
if (fs.existsSync(path.resolve(webRoot, "src/app/page.tsx"))) routePaths.push("");

const sitemapSource = fs.readFileSync(path.resolve(webRoot, "src/app/sitemap.ts"), "utf8");
const sitemapPaths = [...sitemapSource.matchAll(/"([^"]*)"/g)]
  .map((m) => m[1])
  .filter((p) => p === "" || /^\/[a-z0-9\-\/]*$/.test(p));

const excluded = (route) =>
  route === "/login" || route === "/signup" || route === "/app" || route.startsWith("/app/") || route.includes("[");
const expected = new Set(routePaths.filter((route) => !excluded(route)));
const advertised = new Set(sitemapPaths);
const missing = [...expected].filter((route) => !advertised.has(route));
const phantom = [...advertised].filter((route) => !expected.has(route));
if (missing.length || phantom.length) {
  errors.push(
    "sitemap.ts and the app route tree disagree" +
      (missing.length ? `; missing from sitemap: ${missing.join(", ")}` : "") +
      (phantom.length ? `; sitemap lists unknown routes: ${phantom.join(", ")}` : "")
  );
}

if (errors.length) {
  throw new Error(`content validation failed:\n  - ${errors.join("\n  - ")}`);
}
console.log(
  `content validation passed (${files.length} files; OpenAPI synchronized; types drift gate clean; ` +
    `${syntaxStrings.length} CLI syntax strings, ${invocationCount} shiftgate invocations, ` +
    `${cliCommands.size} commands, ${expected.size} sitemap routes, security.txt valid)`
);
