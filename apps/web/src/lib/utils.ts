import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// The API uses -1 as the "unlimited" sentinel (plan limits, storage quotas).
export const UNLIMITED_BYTES = -1;

export function formatBytes(bytes: number, decimals = 1): string {
  if (!Number.isFinite(bytes)) return "—";
  if (bytes === UNLIMITED_BYTES) return "Unlimited";
  if (bytes < 0) return "—";
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB", "PB"];
  if (bytes < 1) return "<1 B";
  const i = Math.min(sizes.length - 1, Math.max(0, Math.floor(Math.log(bytes) / Math.log(k))));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(decimals)) + " " + sizes[i];
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(1)}s`;
  const minutes = seconds / 60;
  if (minutes < 60) return `${Math.floor(minutes)}m ${Math.floor(seconds % 60)}s`;
  const hours = minutes / 60;
  return `${Math.floor(hours)}h ${Math.floor(minutes % 60)}m`;
}

export function timeAgo(date: string | Date): string {
  const now = Date.now();
  const then = new Date(date).getTime();
  if (!Number.isFinite(then)) return "—";
  const diff = now - then;
  const seconds = Math.floor(diff / 1000);
  // Future timestamps are expiries and deadlines: "in …", never "just now".
  if (seconds < 0) {
    const until = Math.abs(seconds);
    if (until < 60) return "in less than a minute";
    const minutes = Math.floor(until / 60);
    if (minutes < 60) return `in ${minutes}m`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `in ${hours}h`;
    return `in ${Math.floor(hours / 24)}d`;
  }
  if (seconds < 60) return "just now";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  const months = Math.floor(days / 30);
  return `${months}mo ago`;
}

// Locale policy: every formatter resolves the browser locale (Intl default)
// so numbers, dates, and money agree on one screen. Invalid input renders "—"
// — never "NaN…"/"Invalid Date".
export function formatNumber(n: number): string {
  if (!Number.isFinite(n)) return "—";
  return new Intl.NumberFormat().format(n);
}

function toDate(value: string | number | Date | null | undefined): Date | null {
  if (value == null) return null;
  const date = value instanceof Date ? value : new Date(value);
  return Number.isFinite(date.getTime()) ? date : null;
}

export function formatDate(value: string | number | Date | null | undefined): string {
  const date = toDate(value);
  return date ? date.toLocaleDateString() : "—";
}

export function formatDateTime(value: string | number | Date | null | undefined): string {
  const date = toDate(value);
  return date ? date.toLocaleString() : "—";
}

export function formatTime(value: string | number | Date | null | undefined): string {
  const date = toDate(value);
  return date ? date.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" }) : "—";
}

export function formatCurrency(cents: number, currency = "USD"): string {
  if (!Number.isFinite(cents)) return "—";
  return new Intl.NumberFormat(undefined, { style: "currency", currency }).format(cents / 100);
}

export function formatMemory(bytes: number): string {
  return formatBytes(bytes, 1);
}

export function errorMessage(error: unknown, fallback: string): string {
  if (
    typeof error === "object" &&
    error !== null &&
    "message" in error &&
    typeof error.message === "string"
  ) {
    return error.message;
  }
  return fallback;
}

// The API reports machine-readable failures as plain objects (ApiError), not
// Error instances — `instanceof Error` never matches them.
export function apiErrorStatus(error: unknown): number | null {
  if (
    typeof error === "object" &&
    error !== null &&
    "status" in error &&
    typeof (error as { status: unknown }).status === "number"
  ) {
    return (error as { status: number }).status;
  }
  return null;
}

export function apiErrorCode(error: unknown): string | null {
  if (
    typeof error === "object" &&
    error !== null &&
    "code" in error &&
    typeof (error as { code: unknown }).code === "string"
  ) {
    return (error as { code: string }).code;
  }
  return null;
}

// FEATURE_PLAN_REQUIRED (402) is a plan gate, not an outage: the UI must say
// so and offer the upgrade path instead of a generic load error.
export function isPlanRequired(error: unknown): boolean {
  return apiErrorStatus(error) === 402 || apiErrorCode(error) === "FEATURE_PLAN_REQUIRED";
}

// Stripe billing can be off in a deployment (billing activation pending): the
// control plane then answers checkout/portal with BILLING_NOT_CONFIGURED (503)
// or BILLING_DISABLED (501). That is a deployment state, not a customer
// error — surfaces must fail closed with activation-pending messaging instead
// of a raw error or a fake success.
export function isBillingUnavailable(error: unknown): boolean {
  const code = apiErrorCode(error);
  return code === "BILLING_NOT_CONFIGURED" || code === "BILLING_DISABLED";
}

export function relativeTime(date: string): string {
  return timeAgo(date);
}

// Role gates that mirror the control plane's requireOrganization roles:
// viewers may read; operating (agent commands, migration create/cancel) needs
// operator+; admin-only surfaces (audit, API keys, billing) need admin+.
export function canOperate(role: string | undefined | null): boolean {
  return role === "owner" || role === "admin" || role === "operator";
}

export function canAdmin(role: string | undefined | null): boolean {
  return role === "owner" || role === "admin";
}

// Mirrors billing.EntitlementRecord.Active(): only these subscription states
// count as live. Anything else fails closed, exactly like the server-side
// LiveMigrationAllowed gate the dialog must not disagree with.
export function isSubscriptionActive(status: string | undefined | null): boolean {
  const value = (status || "").trim().toLowerCase();
  return value === "active" || value === "trialing" || value === "past_due";
}
