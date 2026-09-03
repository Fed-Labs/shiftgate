"use client";

import React from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Key,
  Monitor,
  Activity,
  ChevronRight,
  Fingerprint,
  Globe,
} from "lucide-react";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, Mono, EmptyState } from "@/components/ui";
import { timeAgo } from "@/lib/utils";
import Link from "next/link";

/* ------------------------------------------------------------------ */
/*  Helpers                                                            */
/* ------------------------------------------------------------------ */

function actionVariant(action: string): "default" | "success" | "warning" | "error" | "info" | "accent" {
  const a = action.toLowerCase();
  if (a.includes("create") || a.includes("register")) return "success";
  if (a.includes("delete") || a.includes("revoke")) return "error";
  if (a.includes("update") || a.includes("migrate")) return "accent";
  if (a.includes("login") || a.includes("auth")) return "info";
  return "default";
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function SecurityPage() {
  const orgId = useAuth((s) => s.organization?.id || "");
  const { user, principal, tokens } = useAuth();

  const { data: keys } = useQuery({
    queryKey: ["api-keys", orgId],
    queryFn: () => api.listAPIKeys(orgId),
    enabled: !!orgId,
  });

  const { data: events } = useQuery({
    queryKey: ["audit-events", orgId],
    queryFn: () => api.listAuditEvents(orgId, 10),
    enabled: !!orgId,
  });

  const activeKeys = keys?.filter((k) => !k.revoked_at) || [];

  return (
    <div>
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl font-semibold text-text">Security</h1>
        <p className="text-sm text-text-muted mt-1">Session info and security overview</p>
      </div>

      {/* Session Info */}
      <Card className="p-5 mb-6">
        <div className="flex items-center gap-2 mb-4">
          <Fingerprint className="w-4 h-4 text-text-muted" />
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Current Session</h2>
        </div>

        <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider">Session ID</p>
            <Mono className="text-xs text-text mt-1 block">
              {principal?.session_id?.slice(0, 16) || tokens?.session_id?.slice(0, 16) || "—"}
            </Mono>
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider">User</p>
            <p className="text-sm text-text mt-1">{user?.email || "—"}</p>
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider">Auth Method</p>
            <Badge variant={principal?.api_key_id ? "info" : "success"}>
              {principal?.api_key_id ? "API Key" : "Session"}
            </Badge>
          </div>
          <div>
            <p className="text-xs text-text-muted uppercase tracking-wider">Expires</p>
            <p className="text-sm text-text mt-1">
              {tokens?.expires_at ? timeAgo(tokens.expires_at) : "—"}
            </p>
          </div>
        </div>
      </Card>

      <div className="grid md:grid-cols-2 gap-6 mb-6">
        {/* API Key Summary */}
        <Card className="p-5">
          <div className="flex items-center justify-between mb-4">
            <div className="flex items-center gap-2">
              <Key className="w-4 h-4 text-text-muted" />
              <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">API Keys</h2>
            </div>
            <Link href="/app/api-keys">
              <span className="text-xs text-accent hover:underline flex items-center gap-1">
                Manage <ChevronRight className="w-3 h-3" />
              </span>
            </Link>
          </div>

          <div className="flex items-center gap-6">
            <div>
              <p className="text-2xl font-bold text-text">{activeKeys.length}</p>
              <p className="text-xs text-text-muted">Active keys</p>
            </div>
            <div>
              <p className="text-2xl font-bold text-text">{(keys?.length || 0) - activeKeys.length}</p>
              <p className="text-xs text-text-muted">Revoked</p>
            </div>
          </div>

          {activeKeys.length > 0 && (
            <div className="mt-4 space-y-2">
              {activeKeys.slice(0, 3).map((key) => (
                <div key={key.id} className="flex items-center justify-between py-1.5">
                  <div className="flex items-center gap-2">
                    <Key className="w-3 h-3 text-text-muted" />
                    <span className="text-sm text-text">{key.name}</span>
                  </div>
                  <Mono className="text-xs text-text-muted">{key.prefix}…</Mono>
                </div>
              ))}
            </div>
          )}
        </Card>

        {/* Active Sessions */}
        <Card className="p-5">
          <div className="flex items-center gap-2 mb-4">
            <Monitor className="w-4 h-4 text-text-muted" />
            <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Sessions</h2>
          </div>

          <div className="space-y-3">
            <div className="flex items-center justify-between p-3 bg-bg-surface rounded-lg border border-border">
              <div className="flex items-center gap-3">
                <div className="w-2 h-2 rounded-full bg-accent animate-pulse" />
                <div>
                  <p className="text-sm font-medium text-text">Current Session</p>
                  <div className="flex items-center gap-2 mt-0.5">
                    <Globe className="w-3 h-3 text-text-muted" />
                    <span className="text-xs text-text-muted">
                      {principal?.api_key_id ? "API key auth" : "Browser session"}
                    </span>
                  </div>
                </div>
              </div>
              <Badge variant="success">Active</Badge>
            </div>
          </div>

          <p className="text-xs text-text-muted mt-3">
            Session tokens are stored locally and refreshed automatically.
          </p>
        </Card>
      </div>

      {/* Recent Audit Events */}
      <Card className="p-5 mb-12">
        <div className="flex items-center justify-between mb-4">
          <div className="flex items-center gap-2">
            <Activity className="w-4 h-4 text-text-muted" />
            <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Recent Activity</h2>
          </div>
          <Link href="/app/activity">
            <span className="text-xs text-accent hover:underline flex items-center gap-1">
              View all <ChevronRight className="w-3 h-3" />
            </span>
          </Link>
        </div>

        {!events || events.length === 0 ? (
          <EmptyState title="No recent activity" description="Audit events will appear here." />
        ) : (
          <div className="space-y-1">
            {events.slice(0, 10).map((event) => (
              <div key={event.id} className="flex items-center justify-between py-2 px-2 rounded hover:bg-bg-hover transition-colors">
                <div className="flex items-center gap-3">
                  <Badge variant={actionVariant(event.action)}>{event.action}</Badge>
                  <span className="text-sm text-text-secondary">{event.resource_type}</span>
                  <Mono className="text-xs text-text-muted">{event.resource_id.slice(0, 8)}</Mono>
                </div>
                <span className="text-xs text-text-muted">{timeAgo(event.created_at)}</span>
              </div>
            ))}
          </div>
        )}
      </Card>
    </div>
  );
}
