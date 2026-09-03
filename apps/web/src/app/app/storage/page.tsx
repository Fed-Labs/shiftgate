"use client";

import React from "react";
import { useQuery } from "@tanstack/react-query";
import {
  HardDrive,
  Database,
  ArrowRightLeft,
  FileBox,
  Loader2,
} from "lucide-react";
import { motion } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Mono, EmptyState } from "@/components/ui";
import { formatBytes, formatNumber } from "@/lib/utils";

/* ------------------------------------------------------------------ */
/*  Storage Breakdown Card                                             */
/* ------------------------------------------------------------------ */

function BreakdownItem({ icon: Icon, label, bytes, totalBytes }: {
  icon: React.ElementType;
  label: string;
  bytes: number;
  totalBytes: number;
}) {
  const pct = totalBytes > 0 ? (bytes / totalBytes) * 100 : 0;

  return (
    <div className="flex items-center gap-4 py-3">
      <div className="flex items-center justify-center w-9 h-9 rounded-lg bg-bg-surface border border-border shrink-0">
        <Icon className="w-4 h-4 text-text-muted" />
      </div>
      <div className="flex-1 min-w-0">
        <div className="flex items-center justify-between mb-1">
          <span className="text-sm text-text">{label}</span>
          <Mono className="text-sm text-text">{formatBytes(bytes)}</Mono>
        </div>
        <div className="h-1.5 bg-bg rounded-full overflow-hidden">
          <motion.div
            className="h-full bg-accent rounded-full"
            initial={{ width: 0 }}
            animate={{ width: `${pct}%` }}
            transition={{ duration: 0.5 }}
          />
        </div>
      </div>
      <Mono className="text-xs text-text-muted w-12 text-right">{Math.round(pct)}%</Mono>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function StoragePage() {
  const orgId = useAuth((s) => s.organization?.id || "");

  const { data: entitlement, isLoading, error } = useQuery({
    queryKey: ["entitlement", orgId],
    queryFn: () => api.getEntitlement(orgId),
    enabled: !!orgId,
  });

  const { data: usage } = useQuery({
    queryKey: ["usage", orgId],
    queryFn: () => api.listUsage(orgId),
    enabled: !!orgId,
  });

  if (isLoading) {
    return (
      <div className="flex items-center justify-center py-32">
        <Loader2 className="w-6 h-6 text-accent animate-spin" />
      </div>
    );
  }

  if (error) {
    return <EmptyState title="Failed to load storage" description="Could not fetch storage information." />;
  }

  if (!entitlement) {
    return <EmptyState title="No storage info" description="Storage information is not available." />;
  }

  const usedPct = entitlement.max_storage_bytes > 0
    ? (entitlement.used_storage_bytes / entitlement.max_storage_bytes) * 100
    : 0;

  // Break down usage by kind
  const checkpointUsage = usage?.find((u) => u.kind === "checkpoint_storage");
  const transferUsage = usage?.find((u) => u.kind === "transfer_bytes");
  const checkpointBytes = checkpointUsage?.quantity ?? 0;
  const transferBytes = transferUsage?.quantity ?? 0;
  const otherBytes = Math.max(0, entitlement.used_storage_bytes - checkpointBytes - transferBytes);

  return (
    <div>
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl font-semibold text-text">Storage</h1>
        <p className="text-sm text-text-muted mt-1">Storage usage and breakdown</p>
      </div>

      {/* Total Usage */}
      <Card className="p-5 mb-6">
        <div className="flex items-center justify-between mb-4">
          <div className="flex items-center gap-3">
            <div className="flex items-center justify-center w-10 h-10 rounded-xl bg-accent/10 border border-accent/20">
              <HardDrive className="w-5 h-5 text-accent" />
            </div>
            <div>
              <p className="text-xs text-text-muted uppercase tracking-wider">Total Storage</p>
              <p className="text-xl font-semibold text-text">
                {formatBytes(entitlement.used_storage_bytes)}{" "}
                <span className="text-sm font-normal text-text-muted">
                  / {formatBytes(entitlement.max_storage_bytes)}
                </span>
              </p>
            </div>
          </div>
          <div className="text-right">
            <p className={`text-2xl font-bold ${usedPct > 90 ? "text-status-error" : usedPct > 70 ? "text-status-warning" : "text-accent"}`}>
              {Math.round(usedPct)}%
            </p>
            <p className="text-xs text-text-muted">used</p>
          </div>
        </div>

        <div className="h-3 bg-bg-surface rounded-full overflow-hidden">
          <motion.div
            className={`h-full rounded-full ${usedPct > 90 ? "bg-status-error" : usedPct > 70 ? "bg-status-warning" : "bg-accent"}`}
            initial={{ width: 0 }}
            animate={{ width: `${Math.min(usedPct, 100)}%` }}
            transition={{ duration: 0.6 }}
          />
        </div>

        <div className="flex items-center justify-between mt-2">
          <p className="text-xs text-text-muted">
            {formatBytes(Math.max(0, entitlement.max_storage_bytes - entitlement.used_storage_bytes))} remaining
          </p>
        </div>
      </Card>

      {/* Breakdown */}
      <Card className="p-5 mb-6">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-2">Breakdown</h2>
        <div className="divide-y divide-border">
          <BreakdownItem
            icon={Database}
            label="Checkpoints"
            bytes={checkpointBytes}
            totalBytes={entitlement.used_storage_bytes}
          />
          <BreakdownItem
            icon={ArrowRightLeft}
            label="Transfers"
            bytes={transferBytes}
            totalBytes={entitlement.used_storage_bytes}
          />
          <BreakdownItem
            icon={FileBox}
            label="Other"
            bytes={otherBytes}
            totalBytes={entitlement.used_storage_bytes}
          />
        </div>
      </Card>

      {/* Usage by kind */}
      {usage && usage.length > 0 && (
        <>
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-3">All Usage</h2>
          <Card className="overflow-hidden mb-12">
            <div className="divide-y divide-border">
              {usage.map((u) => (
                <div key={u.kind} className="flex items-center justify-between p-4">
                  <div>
                    <p className="text-sm font-medium text-text">{u.kind.replace(/_/g, " ")}</p>
                    <p className="text-xs text-text-muted mt-0.5">
                      {new Date(u.period_start).toLocaleDateString()} – {new Date(u.period_end).toLocaleDateString()}
                    </p>
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
