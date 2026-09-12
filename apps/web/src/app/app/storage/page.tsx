"use client";

import React from "react";
import { useQuery } from "@tanstack/react-query";
import {
  HardDrive,
  Database,
  ArrowRightLeft,
  FileBox,
  Loader2,
  Cloud,
  AlertTriangle,
  Clock,
} from "lucide-react";
import { motion } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { StorageStatus } from "@/lib/types";
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
/*  Hosted Storage Card                                                */
/* ------------------------------------------------------------------ */

function HostedStorageCard({ status }: { status: StorageStatus }) {
  const usedPct = status.max_storage_bytes > 0
    ? (status.used_storage_bytes / status.max_storage_bytes) * 100
    : 0;

  return (
    <Card className="p-5 mb-6">
      <div className="flex items-center justify-between mb-4">
        <div className="flex items-center gap-3">
          <div className="flex items-center justify-center w-10 h-10 rounded-xl bg-accent/10 border border-accent/20">
            <Cloud className="w-5 h-5 text-accent" />
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider">Hosted Checkpoint Storage</p>
            <p className="text-xl font-semibold text-text">
              {formatBytes(status.used_storage_bytes)}{" "}
              <span className="text-sm font-normal text-text-muted">
                / {status.max_storage_bytes > 0 ? formatBytes(status.max_storage_bytes) : "unlimited"}
              </span>
            </p>
          </div>
        </div>
        <div className="text-right">
          <p className={`text-2xl font-bold ${status.over_quota ? "text-status-error" : usedPct > 90 ? "text-status-error" : usedPct > 70 ? "text-status-warning" : "text-accent"}`}>
            {status.max_storage_bytes > 0 ? `${Math.round(Math.min(usedPct, 100))}%` : "—"}
          </p>
          <p className="text-xs text-text-muted">used</p>
        </div>
      </div>

      <div className="h-3 bg-bg-surface rounded-full overflow-hidden">
        <motion.div
          className={`h-full rounded-full ${status.over_quota || usedPct > 90 ? "bg-status-error" : usedPct > 70 ? "bg-status-warning" : "bg-accent"}`}
          initial={{ width: 0 }}
          animate={{ width: `${Math.min(usedPct, 100)}%` }}
          transition={{ duration: 0.6 }}
        />
      </div>

      {status.over_quota && (
        <div className="mt-4 flex items-start gap-3 p-3 rounded-lg bg-status-error/10 border border-status-error/20">
          <AlertTriangle className="w-4 h-4 text-status-error shrink-0 mt-0.5" />
          <div className="text-sm">
            <p className="text-status-error font-medium">Storage quota exceeded</p>
            <p className="text-text-muted text-xs mt-1">
              New checkpoint mirrors are suspended — agents cannot receive storage credentials until
              usage drops below the plan limit or the plan is upgraded. Local checkpointing on your
              machines is unaffected.
            </p>
          </div>
        </div>
      )}

      {status.endpoint && (
        <div className="grid grid-cols-3 gap-4 mt-4 pt-4 border-t border-border">
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1">Endpoint</p>
            <Mono className="text-xs text-text break-all">{status.endpoint}</Mono>
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1">Bucket</p>
            <Mono className="text-xs text-text break-all">{status.bucket}</Mono>
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider mb-1">Prefix</p>
            <Mono className="text-xs text-text break-all">{status.prefix}</Mono>
          </div>
        </div>
      )}

      {status.last_reconciled_at && (
        <div className="flex items-center gap-1.5 mt-4">
          <Clock className="w-3.5 h-3.5 text-text-muted" />
          <p className="text-xs text-text-muted">
            Metered from the bucket — last reconciled{" "}
            {new Date(status.last_reconciled_at).toLocaleString()}
          </p>
        </div>
      )}
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/*  Self-Hosted Note (hosting disabled)                                */
/* ------------------------------------------------------------------ */

function SelfHostedNote() {
  return (
    <Card className="p-5 mb-6">
      <div className="flex items-start gap-3">
        <div className="flex items-center justify-center w-10 h-10 rounded-xl bg-bg-surface border border-border shrink-0">
          <HardDrive className="w-5 h-5 text-text-muted" />
        </div>
        <div>
          <p className="text-sm font-medium text-text">This control plane does not host storage</p>
          <p className="text-sm text-text-muted mt-1">
            Checkpoints live on your agents&apos; local disks. Your plan&apos;s storage allowance
            applies only to platform-hosted mirroring, which is not enabled on this deployment.
            To keep a cloud copy, configure an S3-compatible object store on the agent
            (<code className="text-xs">SHIFT_OBJECTSTORE_BACKEND=s3</code>) — see the
            installation docs.
          </p>
        </div>
      </div>
    </Card>
  );
}

/* ------------------------------------------------------------------ */
/*  Agent Setup Snippet                                                */
/* ------------------------------------------------------------------ */

function AgentSetupSnippet() {
  const snippet = [
    "# /etc/shift/agent.env — hosted checkpoint mirroring",
    "SHIFT_OBJECTSTORE_ENABLED=true",
    "SHIFT_OBJECTSTORE_BACKEND=control-plane",
    "SHIFT_CONTROL_ORGANIZATION_ID=<your organization id>",
    "SHIFT_CONTROL_MACHINE_ID=<this machine's id>",
    "SHIFT_CONTROL_API_KEY=<machines-scope api key>",
    "# No SHIFT_CONTROL_URL needed — unset, agents talk to the platform endpoint.",
  ].join("\n");

  return (
    <Card className="p-5 mb-6">
      <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-2">
        Agent Setup
      </h2>
      <p className="text-sm text-text-muted mb-3">
        Add these lines to each agent&apos;s environment to mirror checkpoints into hosted
        storage. Agents fetch short-lived, organization-scoped credentials automatically
        using a machines-scope API key — no storage secrets on the agent.
      </p>
      <pre className="bg-bg-surface border border-border rounded-lg p-4 overflow-x-auto text-xs leading-relaxed">
        {snippet}
      </pre>
    </Card>
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

  const { data: storage } = useQuery({
    queryKey: ["storage", orgId],
    queryFn: () => api.getStorageStatus(orgId),
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

      {/* Hosted storage / self-hosted note */}
      {storage === undefined ? null : storage.enabled ? (
        <>
          <HostedStorageCard status={storage} />
          <AgentSetupSnippet />
        </>
      ) : (
        <SelfHostedNote />
      )}

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
