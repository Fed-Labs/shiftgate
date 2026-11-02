"use client";

import React from "react";
import { useQuery } from "@tanstack/react-query";
import { Loader2, Filter, ChevronDown } from "lucide-react";
import { motion } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, Mono, EmptyState } from "@/components/ui";
import { PlanRequiredState, ErrorState } from "@/components/state";
import { canAdmin, errorMessage, formatDateTime, isPlanRequired, timeAgo } from "@/lib/utils";

/* ------------------------------------------------------------------ */
/*  Helpers                                                            */
/* ------------------------------------------------------------------ */

// The server's resource_type vocabulary — anything the audit trail actually
// writes. A filter value that no event can carry is a dead button.
const RESOURCE_TYPES = [
  "all",
  "machine",
  "workload",
  "migration",
  "checkpoint",
  "api_key",
  "organization",
  "organization_member",
  "user",
  "session",
  "plan",
  "compute_offer",
  "compute_reservation",
] as const;

const PAGE_SIZE = 100;

function actionVariant(action: string): "default" | "success" | "warning" | "error" | "info" | "accent" {
  const a = action.toLowerCase();
  if (a.includes("create") || a.includes("register")) return "success";
  if (a.includes("delete") || a.includes("revoke") || a.includes("remove")) return "error";
  if (a.includes("update") || a.includes("migrate")) return "accent";
  if (a.includes("login") || a.includes("auth")) return "info";
  return "default";
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function ActivityPage() {
  const orgId = useAuth((s) => s.organization?.id || "");
  const role = useAuth((s) => s.organization?.role);
  const [filter, setFilter] = React.useState<string>("all");
  const [showFilter, setShowFilter] = React.useState(false);
  // The log is paged: the key carries the page size so this query can never
  // share cache with any other audit consumer (the Security page shows a
  // 10-row preview under its own key).
  const [limit, setLimit] = React.useState(PAGE_SIZE);

  const { data: events, isLoading, isRefetchError, error, dataUpdatedAt, refetch } = useQuery({
    queryKey: ["audit-events", orgId, "activity", limit],
    queryFn: () => api.listAuditEvents(orgId, limit),
    enabled: !!orgId && canAdmin(role),
  });

  const filtered = React.useMemo(() => {
    if (!events) return [];
    if (filter === "all") return events;
    return events.filter((e) => e.resource_type === filter);
  }, [events, filter]);

  const admin = canAdmin(role);

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold text-text">Activity</h1>
          <p className="text-sm text-text-muted mt-1">Audit log of all actions in your organization</p>
        </div>

        {/* Filter */}
        <div
          className="relative"
          onKeyDown={(e) => {
            if (e.key === "Escape") setShowFilter(false);
          }}
        >
          <button
            onClick={() => setShowFilter(!showFilter)}
            aria-haspopup="true"
            aria-expanded={showFilter}
            className="flex items-center gap-2 px-3 py-2 text-sm bg-bg-elevated border border-border rounded-lg hover:bg-bg-hover transition-colors"
          >
            <Filter className="w-3.5 h-3.5 text-text-muted" />
            <span className="text-text-secondary">{filter === "all" ? "All types" : filter}</span>
            <ChevronDown className="w-3.5 h-3.5 text-text-muted" />
          </button>

          {showFilter && (
            <>
              <div className="fixed inset-0 z-40" onClick={() => setShowFilter(false)} />
              <div className="absolute right-0 top-full mt-1 z-50 w-52 bg-bg-elevated border border-border rounded-lg shadow-xl py-1">
                {RESOURCE_TYPES.map((type) => (
                  <button
                    key={type}
                    onClick={() => { setFilter(type); setShowFilter(false); }}
                    className={`w-full text-left px-3 py-1.5 text-sm transition-colors ${
                      filter === type ? "text-accent bg-bg-hover" : "text-text-secondary hover:bg-bg-hover"
                    }`}
                  >
                    {type === "all" ? "All types" : type}
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
      </div>

      {/* Content */}
      {!admin ? (
        <div className="border border-border-subtle bg-bg-elevated py-14 px-6 text-center" role="status">
          <p className="font-mono text-[12px] text-text-secondary mb-2">AUDIT LOG REQUIRES THE ADMIN ROLE</p>
          <p className="text-text-muted text-sm">
            Reading organization history is limited to admins and owners — your role is {role || "unknown"}.
          </p>
        </div>
      ) : isLoading ? (
        <div className="flex items-center justify-center py-32">
          <Loader2 className="w-6 h-6 text-accent animate-spin" />
        </div>
      ) : error && !events ? (
        isPlanRequired(error) ? (
          <PlanRequiredState feature="Audit logging" plan="Business" />
        ) : (
          <ErrorState
            title="FAILED TO LOAD ACTIVITY"
            message={errorMessage(error, "The control plane did not answer the audit query.")}
            onRetry={() => void refetch()}
          />
        )
      ) : filtered.length === 0 ? (
        <EmptyState
          title={filter !== "all" ? `No ${filter} events` : "No activity yet"}
          description={filter !== "all" ? "No audit events match this filter." : "Actions will appear here as they happen."}
        />
      ) : (
        <>
          {isRefetchError && (
            <div className="rounded-lg border border-status-warning/30 bg-status-warning/10 px-4 py-3 text-sm text-status-warning mb-4">
              Showing last known data — a background refresh failed. It retries automatically.
            </div>
          )}
          <Card className="overflow-hidden">
            {/* Table header */}
            <div className="grid grid-cols-2 md:grid-cols-12 gap-x-4 gap-y-1 px-5 py-3 border-b border-border text-xs text-text-muted uppercase tracking-wider">
              <div className="col-span-1 md:col-span-3">Action</div>
              <div className="col-span-1 md:col-span-2">Resource</div>
              <div className="col-span-2 md:col-span-3">Resource ID</div>
              <div className="col-span-1 md:col-span-2">Actor</div>
              <div className="col-span-1 md:col-span-2 text-right">Time</div>
            </div>

            {/* Rows */}
            <div className="divide-y divide-border">
              {filtered.map((event, i) => (
                <motion.div
                  key={event.id}
                  initial={{ opacity: 0 }}
                  animate={{ opacity: 1 }}
                  transition={{ delay: Math.min(i * 0.02, 0.5) }}
                  className="grid grid-cols-2 md:grid-cols-12 gap-x-4 gap-y-2 px-5 py-3 items-center hover:bg-bg-hover transition-colors"
                >
                  <div className="col-span-1 md:col-span-3">
                    <Badge variant={actionVariant(event.action)}>{event.action}</Badge>
                  </div>
                  <div className="col-span-1 md:col-span-2">
                    <span className="text-sm text-text-secondary">{event.resource_type}</span>
                  </div>
                  <div className="col-span-2 md:col-span-3">
                    <Mono className="text-xs text-text-muted break-all">{(event.resource_id ?? "").slice(0, 16)}</Mono>
                  </div>
                  <div className="col-span-1 md:col-span-2">
                    {event.actor_user_id ? (
                      <Mono className="text-xs text-text-muted">{event.actor_user_id.slice(0, 8)}</Mono>
                    ) : (
                      <span className="text-xs text-text-muted">System</span>
                    )}
                  </div>
                  <div className="col-span-1 md:col-span-2 text-right">
                    <span className="text-xs text-text-muted" title={formatDateTime(event.created_at)}>
                      {timeAgo(event.created_at)}
                    </span>
                  </div>
                </motion.div>
              ))}
            </div>
          </Card>

          <div className="flex items-center justify-between mt-4">
            <p className="text-xs text-text-muted">
              Showing {events?.length ?? 0} event{(events?.length ?? 0) !== 1 ? "s" : ""}
              {dataUpdatedAt > 0 && ` · updated ${new Date(dataUpdatedAt).toLocaleTimeString()}`}
            </p>
            {(events?.length ?? 0) >= limit && (
              <button
                onClick={() => setLimit((n) => n + PAGE_SIZE)}
                className="font-mono text-[10px] tracking-[0.14em] text-accent border border-accent/40 px-3 py-2 hover:bg-accent hover:text-bg transition-colors"
              >
                LOAD MORE
              </button>
            )}
          </div>
        </>
      )}
    </div>
  );
}
