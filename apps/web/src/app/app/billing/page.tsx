"use client";

import React, { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  CreditCard,
  HardDrive,
  Server,
  Zap,
  Loader2,
  ExternalLink,
  Check,
  ArrowUpRight,
} from "lucide-react";
import { motion } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { PlanCatalogEntry } from "@/lib/types";
import { Card, Badge, Mono, Button, EmptyState, Divider } from "@/components/ui";
import { formatBytes, formatNumber } from "@/lib/utils";
import Link from "next/link";

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
  return `$${Math.round(plan.price_cents / 100)}`;
}

/* ------------------------------------------------------------------ */
/*  Usage Bar                                                          */
/* ------------------------------------------------------------------ */

function UsageBar({ label, used, max, icon: Icon }: {
  label: string;
  used: number;
  max: number;
  icon: React.ElementType;
}) {
  const unlimited = max < 0;
  const pct = unlimited ? 0 : max > 0 ? Math.min((used / max) * 100, 100) : 0;
  const isOver = !unlimited && pct >= 100;
  const color = isOver ? "bg-status-error" : pct > 80 ? "bg-status-warning" : "bg-accent";

  return (
    <div className="p-4 bg-bg-surface rounded-lg border border-border">
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <Icon className="w-4 h-4 text-text-muted" />
          <span className="text-sm text-text">{label}</span>
        </div>
        <span className={`text-sm font-medium ${isOver ? "text-status-error" : "text-text"}`}>
          {max > 1000000
            ? `${formatBytes(used)} / ${unlimited ? "Unlimited" : formatBytes(max)}`
            : `${formatNumber(used)} / ${unlimited ? "Unlimited" : formatNumber(max)}`}
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
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function BillingPage() {
  const orgId = useAuth((s) => s.organization?.id || "");
  const [actionError, setActionError] = useState<string | null>(null);
  const [actionPending, setActionPending] = useState(false);

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

  const plan = useMemo(() => {
    if (!catalog || !entitlement) return null;
    return catalog.find((p) => p.key === entitlement.plan) ?? null;
  }, [catalog, entitlement]);

  const activeMachineCount = useMemo(
    () => (machines ?? []).filter((m) => m.status !== "disabled").length,
    [machines]
  );

  async function openPortal() {
    setActionError(null);
    setActionPending(true);
    try {
      const { url } = await api.openBillingPortal(orgId, {
        return_url: window.location.origin + "/app/billing",
      });
      window.location.assign(url);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Could not open the billing portal.");
      setActionPending(false);
    }
  }

  async function startCheckout(target: PlanCatalogEntry) {
    setActionError(null);
    setActionPending(true);
    try {
      const { url } = await api.startCheckout(orgId, {
        plan: target.key,
        success_url: window.location.origin + "/app/billing?checkout=success",
        cancel_url: window.location.origin + "/app/billing?checkout=cancel",
      });
      window.location.assign(url);
    } catch (err) {
      setActionError(err instanceof Error ? err.message : "Could not start checkout.");
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

  const upgradable = (catalog ?? []).filter(
    (p) => p.price_cents > 0 && p.key !== entitlement.plan
  );

  return (
    <div>
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

      {actionError && (
        <div className="mb-6 rounded-lg border border-status-error/40 bg-status-error/10 px-4 py-3 text-sm text-status-error">
          {actionError}
        </div>
      )}

      {/* Current Plan */}
      <Card className="p-5 mb-6">
        <div className="flex items-start justify-between">
          <div>
            <div className="flex items-center gap-3 mb-3">
              <h2 className="text-lg font-semibold text-text">
                {plan ? plan.name : planLabel(entitlement.plan)} Plan
              </h2>
              <Badge variant="accent">{entitlement.plan}</Badge>
            </div>
            {plan && (
              <div className="flex items-baseline gap-1 mb-4">
                <span className="text-3xl font-bold text-text">
                  {formatPlanPrice(plan)}
                </span>
                {plan.price_cents > 0 && (
                  <span className="text-sm text-text-muted">
                    {plan.per_seat ? "/seat/mo" : "/mo"}
                  </span>
                )}
              </div>
            )}
          </div>
        </div>

        {/* Plan features */}
        {plan && (
          <>
            <Divider />
            <div className="grid grid-cols-2 md:grid-cols-3 gap-3 mt-4">
              {plan.features.map((feature, i) => (
                <div key={i} className="flex items-start gap-2">
                  <Check className="w-3.5 h-3.5 text-accent shrink-0 mt-0.5" />
                  <span className="text-sm text-text-secondary">{feature}</span>
                </div>
              ))}
            </div>
          </>
        )}
      </Card>

      {/* Usage */}
      <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">Usage</h2>
      <div className="grid md:grid-cols-2 gap-3 mb-6">
        <UsageBar
          label="Storage"
          used={entitlement.used_storage_bytes}
          max={entitlement.max_storage_bytes}
          icon={HardDrive}
        />
        <UsageBar
          label="Machines"
          used={activeMachineCount}
          max={entitlement.max_machines}
          icon={Server}
        />
      </div>

      {/* Upgrades — each checkout starts from the server, which decides the
          price and the limits; the page only names the plan. */}
      {upgradable.length > 0 && (
        <>
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">
            Available plans
          </h2>
          <div className="grid md:grid-cols-3 gap-3 mb-6">
            {upgradable.map((target) => (
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
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => startCheckout(target)}
                  disabled={actionPending}
                >
                  <ArrowUpRight className="w-3.5 h-3.5 mr-1" />
                  Switch to {target.name}
                </Button>
              </Card>
            ))}
          </div>
          <p className="text-xs text-text-muted mb-6">
            Plan changes are processed by Stripe and take effect when the
            subscription is confirmed.{" "}
            <Link href="/pricing" className="text-accent hover:underline">
              Compare all plans
            </Link>
          </p>
        </>
      )}

      {/* Usage breakdown */}
      {usage && usage.length > 0 && (
        <>
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">Usage Breakdown</h2>
          <Card className="overflow-hidden mb-12">
            <div className="divide-y divide-border">
              {usage.map((u) => (
                <div key={u.kind} className="flex items-center justify-between p-4">
                  <div className="flex items-center gap-3">
                    {u.kind === "checkpoint_storage" && <HardDrive className="w-4 h-4 text-text-muted" />}
                    {u.kind === "transfer_bytes" && <Zap className="w-4 h-4 text-text-muted" />}
                    {u.kind === "machine_hours" && <Server className="w-4 h-4 text-text-muted" />}
                    {u.kind === "api_requests" && <Zap className="w-4 h-4 text-text-muted" />}
                    <div>
                      <p className="text-sm font-medium text-text">{u.kind.replace(/_/g, " ")}</p>
                      <p className="text-xs text-text-muted">
                        {new Date(u.period_start).toLocaleDateString()} – {new Date(u.period_end).toLocaleDateString()}
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
    </div>
  );
}
