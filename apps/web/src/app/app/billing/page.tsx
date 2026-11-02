"use client";

import React, { Suspense, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "next/navigation";
import {
  CreditCard,
  HardDrive,
  Server,
  Zap,
  Loader2,
  ExternalLink,
  Check,
  ArrowUpRight,
  Clock,
  X,
} from "lucide-react";
import { motion } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { PlanCatalogEntry } from "@/lib/types";
import { Card, Badge, Mono, Button, EmptyState, Divider } from "@/components/ui";
import { errorMessage, formatBytes, formatCurrency, formatNumber, isBillingUnavailable } from "@/lib/utils";
import Link from "next/link";
import { getTrialStatus, startTrial, getUsageSummary, startCheckoutSession } from "./plan-api";

/* ------------------------------------------------------------------ */
/*  Helpers                                                            */
/* ------------------------------------------------------------------ */

function planLabel(plan: string): string {
  switch (plan) {
    case "free": return "Free";
    case "pro": return "Pro";
    case "business": return "Business";
    case "enterprise": return "Enterprise";
    case "paid": return "Paid";
    default: return plan;
  }
}

function formatPlanPrice(plan: PlanCatalogEntry): string {
  if (plan.price_cents < 0) return "Custom";
  if (plan.price_cents === 0) return "Free";
  return formatCurrency(plan.price_cents);
}

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// Date-only API strings ("2006-01-02") are formatted from their components.
// Parsing them with `new Date(...)` treats them as UTC midnight and shifts the
// rendered day west of UTC.
function formatDateOnly(value: string): string {
  const [year, month, day] = value.split("-").map(Number);
  if (!year || !month || !day) return value;
  return `${MONTHS[month - 1]} ${day}, ${year}`;
}

function formatDateOrTime(value: string): string {
  const dateOnly = value.length >= 10 ? value.slice(0, 10) : value;
  return formatDateOnly(dateOnly);
}

// The subscription status vocabulary from the control plane (see
// internal/database/entitlements.go): only these three carry plan entitlements.
const ACTIVE_STATUSES = ["active", "trialing", "past_due"];
function isActiveStatus(status: string): boolean {
  return ACTIVE_STATUSES.includes(status);
}

// A Stripe subscription that is not terminally dead still exists — starting a
// checkout alongside it would double-subscribe the customer. Only these end
// states make a fresh checkout a replacement rather than a parallel one.
const TERMINAL_SUBSCRIPTION_STATUSES = ["canceled", "incomplete_expired"];

/* ------------------------------------------------------------------ */
/*  Usage Bar                                                          */
/* ------------------------------------------------------------------ */

function UsageBar({ label, used, max, unit, icon: Icon }: {
  label: string;
  used: number;
  max: number;
  unit: "bytes" | "count";
  icon: React.ElementType;
}) {
  const unlimited = max < 0;
  const pct = unlimited ? 0 : max > 0 ? Math.min((used / max) * 100, 100) : 0;
  const isOver = !unlimited && pct >= 100;
  const color = isOver ? "bg-status-error" : pct > 80 ? "bg-status-warning" : "bg-accent";
  // Unlimited quotas (-1) must never reach a formatter: formatBytes(-1)
  // renders NaN and raw digit strings are unreadable for byte counts.
  const fmt = (value: number) => (unit === "bytes" ? formatBytes(Math.max(0, value)) : formatNumber(value));

  return (
    <div className="p-4 bg-bg-surface rounded-lg border border-border">
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <Icon className="w-4 h-4 text-text-muted" />
          <span className="text-sm text-text">{label}</span>
        </div>
        <span className={`text-sm font-medium ${isOver ? "text-status-error" : "text-text"}`}>
          {`${fmt(used)} / ${unlimited ? "Unlimited" : fmt(max)}`}
        </span>
      </div>
      <div className="h-2 bg-bg rounded-full overflow-hidden">
        {unlimited ? (
          <div className="h-full w-full bg-accent/30 rounded-full" />
        ) : (
          <motion.div
            className={`h-full rounded-full ${color}`}
            initial={{ width: 0 }}
            animate={{ width: `${pct}%` }}
            transition={{ duration: 0.5 }}
          />
        )}
      </div>
      <p className="text-xs text-text-muted mt-1.5">
        {unlimited ? "Unlimited" : `${Math.round(pct)}% used`}
      </p>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  Plan change dialog                                                 */
/* ------------------------------------------------------------------ */

// Self-serve add-ons (keys and prices mirror billing.AddOnCatalog — the same
// catalog the pricing page's plan feature strings quote).
const PRO_ADDONS = [
  { key: "ha", name: "HA Pack", priceLabel: "+$39/mo", description: "Warm-standby failover on Pro." },
  { key: "sourceguard", name: "SourceGuard Assurance", priceLabel: "+$19/mo", description: "Credit-back guarantee and monthly signed attestation." },
];

function PlanChangeDialog({ target, current, seatsInUse, onConfirm, onClose, pending, error }: {
  target: PlanCatalogEntry;
  current: PlanCatalogEntry | null;
  seatsInUse: number;
  onConfirm: (options: { interval: "month" | "year"; seats: number; addons: string[]; coupon: string; spendCapCents: number }) => void;
  onClose: () => void;
  pending: boolean;
  error: string | null;
}) {
  const direction =
    current && current.price_cents >= 0 && target.price_cents >= 0
      ? target.price_cents > current.price_cents
        ? "Upgrade"
        : target.price_cents < current.price_cents
        ? "Downgrade"
        : "Change"
      : "Change";
  const [interval, setInterval] = useState<"month" | "year">("month");
  const [seats, setSeats] = useState<number>(target.included_seats ?? 0);
  const [addOns, setAddOns] = useState<string[]>([]);
  const [coupon, setCoupon] = useState("");
  const [spendCap, setSpendCap] = useState("");
  const annual = (target.price_cents_annual ?? 0) > 0;
  const includedSeats = target.included_seats ?? 0;
  const seatOverage = target.seat_overage_cents ?? 0;
  const defaultSpendCap = target.overage_cap_cents_default ?? 0;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/50" onClick={onClose} />
      <Card className="relative w-full max-w-lg p-6 max-h-[90vh] overflow-y-auto">
        <div className="flex items-start justify-between mb-1">
          <h2 className="text-lg font-semibold text-text">
            {direction} to {target.name}
          </h2>
          <button onClick={onClose} aria-label="Close" className="text-text-muted hover:text-text">
            <X className="w-4 h-4" />
          </button>
        </div>
        <p className="text-sm text-text-secondary mb-4">
          {direction === "Downgrade"
            ? `This moves you from ${current?.name ?? "your current plan"} to ${target.name}. The new plan's limits are enforced when the change takes effect; your checkpoints are kept.`
            : `A ${target.name} subscription is created at checkout. The server applies every limit from the plan catalog — never from checkout metadata.`}
        </p>

        {annual && (
          <div className="mb-4">
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1.5">Billing interval</p>
            <div className="flex gap-2">
              {(["month", "year"] as const).map((value) => (
                <button
                  key={value}
                  onClick={() => setInterval(value)}
                  className={`px-3 py-2 rounded-lg border text-sm ${
                    interval === value
                      ? "border-accent/60 bg-accent/10 text-text"
                      : "border-border bg-bg-surface text-text-secondary"
                  }`}
                >
                  {value === "month"
                    ? `Monthly — ${formatPlanPrice(target)}/mo`
                    : `Annual — ${formatCurrency(target.price_cents_annual ?? 0)}/yr (2 months free)`}
                </button>
              ))}
            </div>
          </div>
        )}

        {includedSeats > 0 && (
          <div className="mb-4">
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1.5">Seats</p>
            <input
              type="number"
              min={includedSeats}
              value={seats}
              onChange={(e) => setSeats(Math.max(0, Number(e.target.value)))}
              className="w-32 px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text"
            />
            <p className="text-xs text-text-muted mt-1.5">
              {includedSeats} included · +{formatCurrency(seatOverage)}/seat/mo beyond that. Must cover every current member ({seatsInUse}).
            </p>
          </div>
        )}

        {target.key === "pro" && (
          <div className="mb-4">
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1.5">Add-ons</p>
            <div className="space-y-2">
              {PRO_ADDONS.map((addOn) => (
                <label key={addOn.key} className="flex items-start gap-2 text-sm text-text-secondary">
                  <input
                    type="checkbox"
                    checked={addOns.includes(addOn.key)}
                    onChange={(e) =>
                      setAddOns((prev) =>
                        e.target.checked ? [...prev, addOn.key] : prev.filter((k) => k !== addOn.key)
                      )
                    }
                    className="mt-1"
                  />
                  <span>
                    <span className="text-text">{addOn.name}</span> {addOn.priceLabel} — {addOn.description}
                  </span>
                </label>
              ))}
            </div>
          </div>
        )}

        <div className="grid grid-cols-2 gap-3 mb-4">
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1.5">Coupon (optional)</p>
            <input
              type="text"
              value={coupon}
              onChange={(e) => setCoupon(e.target.value)}
              placeholder="e.g. FOUNDING-TEAM"
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text"
            />
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1.5">
              Overage spend cap, $/mo (optional)
            </p>
            <input
              type="number"
              min={0}
              value={spendCap}
              onChange={(e) => setSpendCap(e.target.value)}
              placeholder={defaultSpendCap > 0 ? `default $${defaultSpendCap / 100}` : "plan default"}
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text"
            />
          </div>
        </div>

        {error && (
          <p className="text-sm text-status-error mb-3">{error}</p>
        )}

        <div className="flex items-center justify-end gap-3">
          <Button variant="secondary" onClick={onClose} disabled={pending}>Cancel</Button>
          <Button
            variant="primary"
            onClick={() =>
              onConfirm({
                interval,
                seats: includedSeats > 0 ? Math.max(seats, includedSeats) : 0,
                addons: addOns,
                coupon: coupon.trim(),
                spendCapCents: spendCap.trim() ? Math.round(Number(spendCap) * 100) : 0,
              })
            }
            disabled={pending}
          >
            {pending ? <Loader2 className="w-4 h-4 animate-spin" /> : "Continue to checkout"}
          </Button>
        </div>
      </Card>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function BillingPage() {
  return (
    <Suspense fallback={null}>
      <BillingContent />
    </Suspense>
  );
}

function BillingContent() {
  const orgId = useAuth((s) => s.organization?.id || "");
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionPending, setActionPending] = useState(false);
  const [pendingChange, setPendingChange] = useState<PlanCatalogEntry | null>(null);
  const [dialogError, setDialogError] = useState<string | null>(null);
  // Billing activation pending: the control plane answered checkout/portal
  // with BILLING_NOT_CONFIGURED/BILLING_DISABLED. Fail closed for the rest of
  // the visit — every checkout CTA stays disabled with activation-pending
  // messaging instead of retrying into the same raw failure.
  const [billingPending, setBillingPending] = useState(false);
  const searchParams = useSearchParams();

  // Stripe redirects back to ?checkout=success|cancel — acknowledge the
  // round-trip and refresh the entitlement the webhook may have just changed.
  const checkoutOutcome = searchParams.get("checkout");
  const checkoutNotice =
    checkoutOutcome === "success"
      ? "Checkout completed — your plan updates once the subscription is confirmed."
      : checkoutOutcome === "cancel"
      ? "Checkout cancelled — no changes were made to your plan."
      : null;

  const queryClient = useQueryClient();

  React.useEffect(() => {
    if (!checkoutOutcome) return;
    if (checkoutOutcome === "success") {
      queryClient.invalidateQueries({ queryKey: ["entitlement", orgId] });
    }
    const params = new URLSearchParams(window.location.search);
    params.delete("checkout");
    const query = params.toString();
    window.history.replaceState(null, "", `${window.location.pathname}${query ? `?${query}` : ""}`);
  }, [checkoutOutcome, orgId, queryClient]);

  const { data: entitlement, isLoading, error } = useQuery({
    queryKey: ["entitlement", orgId],
    queryFn: () => api.getEntitlement(orgId),
    enabled: !!orgId,
  });

  // The catalog comes from the control plane — the same document the pricing
  // page renders and the server enforces entitlements from.
  const { data: catalog } = useQuery({
    queryKey: ["plans"],
    queryFn: () => api.listPlans(),
  });

  const { data: usage } = useQuery({
    queryKey: ["usage", orgId],
    queryFn: () => api.listUsage(orgId),
    enabled: !!orgId,
  });

  const { data: machines } = useQuery({
    queryKey: ["machines", orgId],
    queryFn: () => api.listMachines(orgId),
    enabled: !!orgId,
  });

  // Usage summary is the API's own totals (machines, seats). The raw machine
  // list is paged at 100 server-side, so counting it silently caps — the
  // summary total is the number enforcement uses.
  const { data: usageSummary } = useQuery({
    queryKey: ["usage-summary", orgId],
    queryFn: () => getUsageSummary(orgId),
    enabled: !!orgId,
    retry: false,
  });

  const { data: trial, refetch: refetchTrial } = useQuery({
    queryKey: ["trial", orgId],
    queryFn: async () => {
      const status = await getTrialStatus(orgId);
      return { ...status, observed_at_ms: Date.now() };
    },
    enabled: !!orgId,
    retry: false,
  });

  const plan = useMemo(() => {
    if (!catalog || !entitlement) return null;
    return catalog.find((p) => p.key === entitlement.plan) ?? null;
  }, [catalog, entitlement]);

  const listedMachineCount = useMemo(
    () => (machines ?? []).filter((m) => m.status !== "disabled").length,
    [machines]
  );

  // Aggregated usage per kind across every period bucket in the window (the
  // default window routinely spans two monthly buckets).
  const usageByKind = useMemo(() => {
    const totals = new Map<string, { quantity: number; from: string; to: string }>();
    for (const record of usage ?? []) {
      const entry = totals.get(record.kind) ?? { quantity: 0, from: record.period_start, to: record.period_end };
      entry.quantity += record.quantity;
      if (record.period_start < entry.from) entry.from = record.period_start;
      if (record.period_end > entry.to) entry.to = record.period_end;
      totals.set(record.kind, entry);
    }
    return [...totals.entries()].map(([kind, entry]) => ({ kind, ...entry }));
  }, [usage]);

  const subscriptionActive = !!entitlement && isActiveStatus(entitlement.status);
  const trialRunning = !!entitlement?.trial_active;
  const subscriptionExists =
    !!entitlement?.stripe_customer_id &&
    !TERMINAL_SUBSCRIPTION_STATUSES.includes((entitlement.status || "").trim());
  const entitledToPaidFeatures = subscriptionActive;
  const effectivePlanKey = trialRunning ? "business" : (entitlement?.plan ?? "free");
  const effectivePlan = useMemo(
    () => (catalog ?? []).find((p) => p.key === effectivePlanKey) ?? plan,
    [catalog, effectivePlanKey, plan]
  );

  // Trial countdown boundaries: derive from expires_at (the API's days_left
  // floors, so a just-started trial reads 13). Zero days means "ended", and
  // "expiring today" is its own state until the timestamp passes. The clock is
  // observed outside render (query time, then interval ticks) so the first
  // paint is stable and the display converges to the exact boundary.
  const DAY_MS = 86_400_000;
  const [nowMs, setNowMs] = useState<number | null>(null);
  React.useEffect(() => {
    const id = window.setInterval(() => setNowMs(Date.now()), 60_000);
    return () => window.clearInterval(id);
  }, []);
  const trialExpiresMs = trial?.expires_at ? Date.parse(trial.expires_at) : NaN;
  const clockMs =
    nowMs ??
    trial?.observed_at_ms ??
    (Number.isFinite(trialExpiresMs) ? trialExpiresMs - (trial?.days_left ?? 0) * DAY_MS : NaN);
  const trialMsLeft = trialExpiresMs - clockMs;
  const trialRunningByClock = Number.isFinite(trialMsLeft) && trialMsLeft > 0;
  const trialDaysLeft = trialRunningByClock ? Math.max(1, Math.ceil(trialMsLeft / DAY_MS)) : 0;
  const trialEnded = !!trial?.trial && Number.isFinite(trialExpiresMs) && !trialRunningByClock;
  const paidSubscriptionActive =
    subscriptionActive && entitlement.plan !== "free";
  const canStartTrial = !!trial?.eligible && !paidSubscriptionActive && !trialRunning;

  function validBillingRedirect(value: string): string | null {
    try {
      const url = new URL(value, window.location.origin);
      const allowedExternalHost =
        url.protocol === "https:" &&
        (url.hostname === "checkout.stripe.com" || url.hostname === "billing.stripe.com");
      if (url.origin !== window.location.origin && !allowedExternalHost) return null;
      return url.href;
    } catch {
      return null;
    }
  }

  function localBillingUrl(path: string): string {
    return new URL(path, window.location.origin).href;
  }

  async function openPortal() {
    setActionError(null);
    setActionPending(true);
    try {
      const { url } = await api.openBillingPortal(orgId, {
        return_url: localBillingUrl("/app/billing"),
      });
      const portalUrl = validBillingRedirect(url);
      if (!portalUrl) throw new Error("Billing portal returned an invalid redirect origin.");
      window.location.assign(portalUrl);
    } catch (err) {
      if (isBillingUnavailable(err)) {
        setBillingPending(true);
      } else {
        setActionError(errorMessage(err, "Could not open the billing portal."));
      }
      setActionPending(false);
    }
  }

  async function beginTrial() {
    setActionError(null);
    setActionPending(true);
    try {
      await startTrial(orgId);
      await refetchTrial();
      queryClient.invalidateQueries({ queryKey: ["entitlement", orgId] });
    } catch (err) {
      setActionError(errorMessage(err, "Could not start the trial."));
    }
    setActionPending(false);
  }

  // Checkout is only ever a *new* subscription. An organization with a live
  // Stripe subscription must change plans through the portal, which replaces
  // the subscription — a second checkout would double-bill (the checkout API
  // has no replace parameter yet).
  async function startCheckout(
    target: PlanCatalogEntry,
    options: { interval: "month" | "year"; seats: number; addons: string[]; coupon: string; spendCapCents: number }
  ) {
    setDialogError(null);
    setActionPending(true);
    try {
      const { url } = await startCheckoutSession(orgId, {
        plan: target.key,
        success_url: localBillingUrl("/app/billing?checkout=success"),
        cancel_url: localBillingUrl("/app/billing?checkout=cancel"),
        ...(options.seats > 0 ? { seats: options.seats } : {}),
        ...(options.interval === "year" ? { interval: "year" as const } : {}),
        ...(options.addons.length > 0 ? { addons: options.addons } : {}),
        ...(options.coupon ? { coupon: options.coupon } : {}),
        ...(options.spendCapCents > 0 ? { spend_cap_cents: options.spendCapCents } : {}),
      });
      const checkoutUrl = validBillingRedirect(url);
      if (!checkoutUrl) throw new Error("Checkout returned an invalid redirect origin.");
      window.location.assign(checkoutUrl);
    } catch (err) {
      if (isBillingUnavailable(err)) {
        setBillingPending(true);
        setPendingChange(null);
      } else {
        setDialogError(errorMessage(err, "Could not start checkout."));
      }
      setActionPending(false);
    }
  }

  if (isLoading) {
    return (
      <div className="flex items-center justify-center py-32">
        <Loader2 className="w-6 h-6 text-accent animate-spin" />
      </div>
    );
  }

  if (error) {
    return <EmptyState title="Failed to load billing" description="Could not fetch billing information." />;
  }

  if (!entitlement) {
    return <EmptyState title="No billing info" description="Billing information is not available." />;
  }

  const purchasable = (catalog ?? []).filter(
    (p) => p.price_cents > 0 && p.key !== entitlement.plan
  );

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      {/* Header */}
      <div className="flex items-center justify-between mb-8">
        <div>
          <h1 className="text-2xl font-semibold text-text">Billing</h1>
          <p className="text-sm text-text-muted mt-1">Plan details and usage</p>
        </div>
        {entitlement.stripe_customer_id && (
          <Button variant="secondary" onClick={openPortal} disabled={actionPending}>
            <CreditCard className="w-4 h-4 mr-1.5" />
            Manage Subscription
            <ExternalLink className="w-3 h-3 ml-1.5 text-text-muted" />
          </Button>
        )}
      </div>

      {checkoutNotice && (
        <div className="mb-6 rounded-lg border border-accent/30 bg-accent/10 px-4 py-3 text-sm text-accent">
          {checkoutNotice}
        </div>
      )}

      {actionError && (
        <div className="mb-6 rounded-lg border border-status-error/40 bg-status-error/10 px-4 py-3 text-sm text-status-error">
          {actionError}
        </div>
      )}

      {/* Billing activation pending is a deployment state, not a failure: the
          catalog and the free tier/trial keep working; only self-serve
          checkout waits. Contact is the honest path to a plan before then. */}
      {billingPending && (
        <div className="mb-6 rounded-lg border border-status-warning/30 bg-status-warning/10 px-4 py-3 text-sm text-text-secondary">
          <span className="font-medium text-text">Billing activation pending.</span>{" "}
          Self-serve checkout for paid plans is not activated on this control
          plane yet — plan purchases and changes open when billing activation
          completes. The free tier and the 14-day trial are not affected. Need
          a paid plan before then?{" "}
          <Link href="/contact" className="text-accent hover:underline">
            Contact us
          </Link>{" "}
          and we will arrange it.
        </div>
      )}

      {/* Current Plan */}
      <Card className="p-5 mb-6">
        <div className="flex items-start justify-between">
          <div>
            <div className="flex items-center gap-3 mb-3 flex-wrap">
              <h2 className="text-lg font-semibold text-text">
                {effectivePlan ? effectivePlan.name : planLabel(effectivePlanKey)} Plan
              </h2>
              <Badge variant="accent">{effectivePlanKey}</Badge>
              {trialRunning && <Badge variant="warning">trial</Badge>}
              {!trialRunning && entitlement.stripe_customer_id && (
                <Badge variant={subscriptionActive ? "success" : "error"}>{entitlement.status || "unknown"}</Badge>
              )}
            </div>
            {effectivePlan && (
              <div className="flex items-baseline gap-1 mb-4">
                <span className="text-3xl font-bold text-text">
                  {formatPlanPrice(effectivePlan)}
                </span>
                {effectivePlan.price_cents > 0 && (
                  <span className="text-sm text-text-muted">
                    {effectivePlan.per_seat ? "/seat/mo" : "/mo"}
                  </span>
                )}
              </div>
            )}
          </div>
        </div>

        {/* Cancellation / downgrade state: enforcement has already reverted to
            free-tier capacity, so paid features are not shown as active. */}
        {!subscriptionActive && !!entitlement.stripe_customer_id && (
          <div className="mb-4 rounded-lg border border-status-warning/30 bg-status-warning/10 px-4 py-3 text-sm text-text-secondary">
            <span className="font-medium text-text">
              {planLabel(entitlement.plan)} · {entitlement.status || "inactive"}
            </span>{" "}
            — this subscription no longer carries its plan&apos;s entitlements. Paid
            features are off and capacity is the free tier&apos;s (cold-only, 2
            machines, 10 GB) until you resubscribe. Your checkpoints and history
            are preserved.
          </div>
        )}

        {/* Plan features — checkmarks only while the plan is actually entitled. */}
        {effectivePlan && (
          <>
            <Divider />
            {entitledToPaidFeatures ? (
              <div className="grid grid-cols-2 md:grid-cols-3 gap-3 mt-4">
                {effectivePlan.features.map((feature, i) => (
                  <div key={i} className="flex items-start gap-2">
                    <Check className="w-3.5 h-3.5 text-accent shrink-0 mt-0.5" />
                    <span className="text-sm text-text-secondary">{feature}</span>
                  </div>
                ))}
              </div>
            ) : (
              <div className="mt-4">
                <p className="text-xs text-text-muted uppercase tracking-wider mb-2">
                  {planLabel(effectivePlanKey)} plan features (not active)
                </p>
                <div className="grid grid-cols-2 md:grid-cols-3 gap-3 opacity-60">
                  {effectivePlan.features.map((feature, i) => (
                    <div key={i} className="flex items-start gap-2">
                      <span className="w-3.5 h-3.5 shrink-0 mt-0.5 text-text-muted">–</span>
                      <span className="text-sm text-text-muted">{feature}</span>
                    </div>
                  ))}
                </div>
              </div>
            )}
          </>
        )}
      </Card>

      {/* Trial */}
      {trialRunning && (
        <Card className="p-5 mb-6 border border-accent/30">
          <div className="flex items-center gap-2 mb-2">
            <Clock className="w-4 h-4 text-accent" />
            <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Business trial</h2>
          </div>
          <p className="text-sm text-text-secondary">
            {!trial
              ? "Your Business trial is running."
              : trialDaysLeft > 0
              ? `${trialDaysLeft} day${trialDaysLeft === 1 ? "" : "s"} left${trial.expires_at ? ` — expires ${formatDateOrTime(trial.expires_at)}` : ""}.`
              : `Less than a day left${trial.expires_at ? ` — expires ${formatDateOrTime(trial.expires_at)}` : ""}.`}{" "}
            Live migration, machine headroom, teams, and audit are enabled for the
            window. When the trial ends, entitlements revert to the free tier
            (cold-only, 2 machines, 10 GB) unless you subscribe — trial data is
            preserved either way.
          </p>
          <div className="mt-3">
            <Button size="sm" variant="primary" onClick={() => setPendingChange(effectivePlan ?? null)} disabled={!effectivePlan || actionPending || billingPending}>
              <ArrowUpRight className="w-3.5 h-3.5 mr-1" />
              Subscribe before it ends
            </Button>
            {billingPending && (
              <p className="text-xs text-text-muted mt-2">
                Subscription checkout opens when billing activation completes —
                contact us to arrange a plan before the trial ends.
              </p>
            )}
          </div>
        </Card>
      )}

      {trialEnded && (
        <Card className="p-5 mb-6">
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-2">Trial ended</h2>
          <p className="text-sm text-text-secondary">
            Your 14-day Business trial ended on {formatDateOrTime(trial?.expires_at ?? "")}.
            Entitlements have reverted to the free tier (cold-only, 2 machines,
            10 GB); trial data is preserved. Subscribe to restore Business
            capacity.
          </p>
        </Card>
      )}

      {!trial?.trial && canStartTrial && (
        <Card className="p-5 mb-6 border border-accent/30">
          <div className="flex items-center gap-2 mb-2">
            <Clock className="w-4 h-4 text-accent" />
            <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Free trial</h2>
          </div>
          <p className="text-sm text-text-secondary">
            14 days of Business, no card, once per organization — live
            migration, machine headroom, teams, and audit. Prove the move on
            your own hardware before paying for anything.
          </p>
          <div className="mt-3">
            <Button size="sm" variant="primary" onClick={beginTrial} disabled={actionPending}>
              {actionPending ? <Loader2 className="w-4 h-4 animate-spin" /> : "Start free trial"}
            </Button>
          </div>
        </Card>
      )}

      {!trial?.trial && trial?.eligible === false && !trialEnded && (
        <Card className="p-5 mb-6">
          <p className="text-sm text-text-secondary">
            The 14-day Business trial is once per organization and has already
            been used. Paid plans are available below.
          </p>
        </Card>
      )}

      {/* Usage */}
      <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">Usage</h2>
      <div className="grid md:grid-cols-2 gap-3 mb-6">
        <UsageBar
          label="Storage"
          used={entitlement.used_storage_bytes}
          max={entitlement.max_storage_bytes}
          unit="bytes"
          icon={HardDrive}
        />
        <UsageBar
          label="Machines"
          used={usageSummary?.machines.total ?? listedMachineCount}
          max={entitlement.max_machines}
          unit="count"
          icon={Server}
        />
      </div>

      {/* Plans — checkout only ever starts a first (or replacement-free) plan;
          an existing subscription changes through the portal. */}
      {purchasable.length > 0 && (
        <>
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">
            Available plans
          </h2>
          {subscriptionExists && (
            <div className="mb-4 rounded-lg border border-accent/30 bg-accent/10 px-4 py-3 text-sm text-accent">
              You already have a subscription on Stripe. Plan changes —
              upgrades, downgrades, and seat quantities — run through the
              billing portal so your current subscription is replaced, never
              duplicated. Add-ons, annual prepay, or coupons on an existing
              subscription are attached by us — contact support.{" "}
              <button
                onClick={openPortal}
                disabled={actionPending}
                className="underline hover:no-underline"
              >
                Change plan in the billing portal →
              </button>
            </div>
          )}
          <div className="grid md:grid-cols-3 gap-3 mb-6">
            {purchasable.map((target) => (
              <Card key={target.key} className="p-4 flex flex-col">
                <div className="flex items-center justify-between mb-1">
                  <span className="text-sm font-medium text-text">{target.name}</span>
                  <span className="text-sm tabular-nums text-text">
                    {formatPlanPrice(target)}
                    {target.price_cents > 0 && (
                      <span className="text-text-muted text-xs">
                        {target.per_seat ? "/seat/mo" : "/mo"}
                      </span>
                    )}
                  </span>
                </div>
                <p className="text-xs text-text-secondary mb-3 flex-1">
                  {target.description}
                </p>
                {subscriptionExists || billingPending ? (
                  <>
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled
                      title={
                        billingPending
                          ? "Billing activation pending — checkout opens when billing activation completes."
                          : "A new checkout would create a second subscription — change plans in the billing portal."
                      }
                    >
                      Switch to {target.name}
                    </Button>
                    <p className="text-xs text-text-muted mt-2">
                      {billingPending
                        ? "Billing activation pending — checkout opens when billing activation completes."
                        : "Change plans in the billing portal (Manage Subscription) — checkout would create a second subscription."}
                    </p>
                  </>
                ) : (
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => {
                      setDialogError(null);
                      setPendingChange(target);
                    }}
                    disabled={actionPending}
                  >
                    <ArrowUpRight className="w-3.5 h-3.5 mr-1" />
                    Switch to {target.name}
                  </Button>
                )}
              </Card>
            ))}
          </div>
          <p className="text-xs text-text-muted mb-6">
            {subscriptionExists
              ? "Plan changes are applied by the Stripe customer portal and take effect per the portal's proration rules."
              : "Checkouts start a new subscription; prices and limits come from the enforced plan catalog."}{" "}
            <Link href="/pricing" className="text-accent hover:underline">
              Compare all plans
            </Link>
          </p>
        </>
      )}

      {/* Usage breakdown */}
      {usageByKind.length > 0 && (
        <>
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">Usage Breakdown</h2>
          <Card className="overflow-hidden mb-12">
            <div className="divide-y divide-border">
              {usageByKind.map((u) => (
                <div key={u.kind} className="flex items-center justify-between p-4">
                  <div className="flex items-center gap-3">
                    {u.kind === "checkpoint_storage" && <HardDrive className="w-4 h-4 text-text-muted" />}
                    {u.kind === "transfer_bytes" && <Zap className="w-4 h-4 text-text-muted" />}
                    {u.kind === "machine_hours" && <Server className="w-4 h-4 text-text-muted" />}
                    {u.kind === "api_requests" && <Zap className="w-4 h-4 text-text-muted" />}
                    <div>
                      <p className="text-sm font-medium text-text">{u.kind.replace(/_/g, " ")}</p>
                      <p className="text-xs text-text-muted">
                        {formatDateOnly(u.from)} – {formatDateOnly(u.to)}
                      </p>
                    </div>
                  </div>
                  <Mono className="text-sm text-text">
                    {u.kind.includes("bytes") ? formatBytes(u.quantity) : formatNumber(u.quantity)}
                  </Mono>
                </div>
              ))}
            </div>
          </Card>
        </>
      )}

      {pendingChange && (
        <PlanChangeDialog
          target={pendingChange}
          current={effectivePlan}
          seatsInUse={usageSummary?.seats.members ?? 0}
          pending={actionPending}
          error={dialogError}
          onClose={() => {
            if (!actionPending) {
              setPendingChange(null);
              setDialogError(null);
            }
          }}
          onConfirm={(options) => startCheckout(pendingChange, options)}
        />
      )}
    </div>
  );
}
