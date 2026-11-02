// Desktop-side display of the support-matrix policy: which unattended
// (automatic) operations the matrix allows on the observed platform. The
// shipped matrix is docs/support-matrix.json — copied here because the
// WebView loads bundled content only; keep the copy in sync when the matrix
// version changes.
//
// `evaluateSupportPolicy` is a faithful, display-only port of
// internal/supportmatrix's Matrix.Decide() (the Go code is the authority).
// Enforcement of AllowAutomatic belongs at the agent before it acts without
// an operator; this card shows operators the same answer `shiftgate verify`
// would give, clearly labeled as advisory.

import matrix from "./support-matrix.json";

export interface SupportEvidence {
  os: string;
  architecture: string;
  distribution: string;
  kernel: string;
  criuVersion: string;
  // The `criu check` probe: "passed", "failed …", or "not-run …".
  criuCheck: string;
}

export interface SupportDecision {
  matrixVersion: string;
  status: string;
  action: string;
  allowAutomatic: boolean;
  platformID?: string;
  reason: string;
}

interface MatrixPlatform {
  id: string;
  distribution: string;
  architecture: string;
  kernel_min: string;
  kernel_max?: string;
  criu_versions?: string[];
  status: string;
  evidence: string;
}

interface MatrixDocument {
  version: string;
  policy: { unknown?: string; untested?: string; gpu_evidence?: string };
  platforms: MatrixPlatform[];
}

const doc = matrix as MatrixDocument;

const ACTION_ALLOW = "allow";
const ACTION_BLOCK = "block_for_automatic_operations";
const ACTION_EXPERIMENTAL = "experimental_only";

function versionParts(value: string): number[] {
  let text = value.trim().replace(/^v/, "");
  const cut = text.search(/[-+]/);
  if (cut >= 0) text = text.slice(0, cut);
  const parts: number[] = [];
  for (const segment of text.split(".")) {
    const number = Number.parseInt(segment, 10);
    if (Number.isNaN(number)) break;
    parts.push(number);
  }
  return parts;
}

function compareVersions(left: number[], right: number[]): number {
  for (let index = 0; index < Math.max(left.length, right.length); index++) {
    const l = left[index] ?? 0;
    const r = right[index] ?? 0;
    if (l !== r) return l < r ? -1 : 1;
  }
  return 0;
}

function kernelInRange(observed: string, minimum: string, maximum?: string): boolean {
  if (!minimum) return true;
  const observedParts = versionParts(observed);
  const minParts = versionParts(minimum);
  if (observedParts.length === 0 || minParts.length === 0 || observedParts.length < minParts.length) {
    return false;
  }
  if (compareVersions(observedParts.slice(0, minParts.length), minParts) < 0) return false;
  if (maximum) {
    const maxParts = versionParts(maximum);
    if (maxParts.length === 0 || observedParts.length < maxParts.length) return false;
    if (compareVersions(observedParts.slice(0, maxParts.length), maxParts) > 0) return false;
  }
  return true;
}

function matchesCRIULine(lines: string[], observed: string): boolean {
  const target = observed.trim().replace(/^v/, "");
  return lines.some((line) => {
    const prefix = line.trim().replace(/^v/, "");
    return target === prefix || target.startsWith(`${prefix}.`);
  });
}

function matchPlatform(evidence: SupportEvidence): MatrixPlatform | null {
  let generic: MatrixPlatform | null = null;
  for (const platform of doc.platforms) {
    if (platform.architecture !== evidence.architecture) continue;
    if (!kernelInRange(evidence.kernel, platform.kernel_min, platform.kernel_max)) continue;
    if (platform.distribution.toLowerCase() === evidence.distribution.toLowerCase()) return platform;
    if (platform.distribution.toLowerCase() === "linux" && !generic) generic = platform;
  }
  return generic;
}

function policyAction(status: string, fallback: string): string {
  switch (status) {
    case "unknown":
      return doc.policy.unknown || fallback;
    case "untested":
    case "experimental":
      return doc.policy.untested || fallback;
    case "unsupported":
      return ACTION_BLOCK;
    case "certified":
    case "supported":
      return ACTION_ALLOW;
    default:
      return fallback;
  }
}

function defaultActionFor(status: string): string {
  switch (status) {
    case "certified":
    case "supported":
      return ACTION_ALLOW;
    case "untested":
    case "experimental":
      return ACTION_EXPERIMENTAL;
    default:
      return ACTION_BLOCK;
  }
}

/** The display-side mirror of supportmatrix.Matrix.Decide(). */
export function evaluateSupportPolicy(evidence: SupportEvidence): SupportDecision {
  const base = { matrixVersion: doc.version };
  if (evidence.os !== "linux" || evidence.architecture !== "amd64") {
    return {
      ...base,
      status: "unsupported",
      action: ACTION_BLOCK,
      allowAutomatic: false,
      reason: "SHIFT supports Linux x86_64 only",
    };
  }
  if (evidence.criuCheck.startsWith("failed") || evidence.criuCheck.startsWith("not-run")) {
    return {
      ...base,
      status: "unsupported",
      action: ACTION_BLOCK,
      allowAutomatic: false,
      reason: `CRIU is unavailable or failed \`criu check\`: ${evidence.criuCheck}`,
    };
  }
  const match = matchPlatform(evidence);
  if (!match) {
    const action = policyAction("unknown", ACTION_BLOCK);
    return {
      ...base,
      status: "unknown",
      action,
      allowAutomatic: action === ACTION_ALLOW,
      reason:
        "no support-matrix entry covers this distribution/kernel/CRIU combination; run scripts/verify-support-matrix.sh to certify it",
    };
  }
  let status = match.status;
  let reason = `matched support-matrix entry ${match.id}: ${match.evidence}`;
  if (match.criu_versions && match.criu_versions.length > 0 && !matchesCRIULine(match.criu_versions, evidence.criuVersion)) {
    if (status === "certified") status = "untested";
    reason = `matches support-matrix entry ${match.id} but CRIU ${evidence.criuVersion} is not one of its certified lines (${match.criu_versions.join(", ")}); run scripts/verify-support-matrix.sh`;
  }
  const action = policyAction(status, defaultActionFor(status));
  return {
    ...base,
    status,
    action,
    allowAutomatic: action === ACTION_ALLOW,
    platformID: match.id,
    reason,
  };
}
