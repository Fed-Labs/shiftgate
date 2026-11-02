// Drift gate between the canonical OpenAPI spec (api/openapi/shift.yaml) and
// the hand-maintained web client types (src/lib/types.ts). The spec is the
// source of truth: every named enum schema must match its exported union, and
// every spec-required property of a shared object schema must exist in the
// matching interface with the same optionality. Imported by sync-openapi.mjs
// (blocks the build) and validate-content.mjs (blocks test:content).
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));

function specSchemas(specText) {
  const start = specText.search(/^  schemas:\s*$/m);
  if (start < 0) return new Map();
  const section = specText.slice(start).split(/\n(?=[a-z])/)[0];
  const schemas = new Map();
  const lines = section.split("\n");
  let name = null;
  let body = [];
  const flush = () => {
    if (name) schemas.set(name, body.join("\n"));
    name = null;
    body = [];
  };
  for (const line of lines) {
    const heading = line.match(/^    ([A-Za-z][A-Za-z0-9]*):\s*$/);
    if (heading) {
      flush();
      name = heading[1];
      continue;
    }
    if (/^    \S/.test(line)) {
      // Inline schema form: "    Name: { ... }"
      const inline = line.match(/^    ([A-Za-z][A-Za-z0-9]*):\s+(\{.*)$/);
      if (inline) {
        flush();
        name = inline[1];
        body.push("        " + inline[2]);
        continue;
      }
    }
    if (name) body.push(line);
  }
  flush();
  return schemas;
}

function flowList(text, key) {
  const m = text.match(new RegExp("\\b" + key + ":\\s*\\[([^\\]]*)\\]"));
  return m ? m[1].split(",").map((s) => s.trim()).filter(Boolean) : null;
}

function tsUnions(typesText) {
  const unions = new Map();
  const re = /export type ([A-Za-z][A-Za-z0-9]*)\s*=\s*([^;]+);/g;
  for (const m of typesText.matchAll(re)) {
    const values = [...m[2].matchAll(/"([^"]+)"/g)].map((v) => v[1]);
    if (values.length) unions.set(m[1], values);
  }
  return unions;
}

function tsInterfaces(typesText) {
  const interfaces = new Map();
  const re = /export interface ([A-Za-z][A-Za-z0-9]*)\s*\{([\s\S]*?)\n\}/g;
  for (const m of typesText.matchAll(re)) {
    const props = new Map();
    for (const p of m[2].matchAll(/^\s{2}([A-Za-z_][A-Za-z0-9_]*)(\?)?:/gm)) {
      props.set(p[1], p[2] !== "?");
    }
    interfaces.set(m[1], props);
  }
  return interfaces;
}

// Returns a list of human-readable drift findings (empty when in sync).
export function checkTypesDrift() {
  const specText = fs.readFileSync(path.resolve(here, "../../../api/openapi/shift.yaml"), "utf8");
  const typesText = fs.readFileSync(path.resolve(here, "../src/lib/types.ts"), "utf8");
  const schemas = specSchemas(specText);
  const unions = tsUnions(typesText);
  const interfaces = tsInterfaces(typesText);
  const failures = [];

  for (const [name, body] of schemas) {
    const specEnum = flowList(body, "enum");
    if (specEnum && /type:\s*string/.test(body)) {
      const tsValues = unions.get(name);
      if (!tsValues) continue;
      const specSet = new Set(specEnum);
      const tsSet = new Set(tsValues);
      const missing = specEnum.filter((v) => !tsSet.has(v));
      const extra = tsValues.filter((v) => !specSet.has(v));
      if (missing.length || extra.length) {
        failures.push(
          `types.ts ${name} union drifts from shift.yaml enum` +
            (missing.length ? `; missing: ${missing.join(", ")}` : "") +
            (extra.length ? `; extra: ${extra.join(", ")}` : "")
        );
      }
    }
    if (!/type:\s*object/.test(body)) continue;
    const tsProps = interfaces.get(name);
    if (!tsProps) continue;
    const required = flowList(body, "required") || [];
    const propNames = new Set();
    const propBlock = body.match(/properties:\s*\n([\s\S]*)/);
    if (propBlock) {
      for (const p of propBlock[1].matchAll(/^        ([A-Za-z_][A-Za-z0-9_]*):/gm)) propNames.add(p[1]);
    }
    for (const prop of required) {
      if (!tsProps.has(prop)) {
        failures.push(`types.ts ${name} omits spec-required property ${prop}`);
      } else if (!tsProps.get(prop)) {
        failures.push(`types.ts ${name}.${prop} is optional but shift.yaml requires it`);
      }
    }
    for (const [prop, tsRequired] of tsProps) {
      if (propNames.has(prop) && tsRequired && !required.includes(prop)) {
        failures.push(`types.ts ${name}.${prop} is required but shift.yaml marks it optional`);
      }
    }
  }
  return failures;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const failures = checkTypesDrift();
  if (failures.length) {
    console.error("generated client drifts from api/openapi/shift.yaml:");
    for (const failure of failures) console.error("  " + failure);
    process.exitCode = 1;
  } else {
    console.log("types.ts matches shift.yaml (named enums + required properties)");
  }
}
