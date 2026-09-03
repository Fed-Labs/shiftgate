"use client";

import React from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  UserPlus,
  ShieldCheck,
  Wrench,
  Eye,
  Crown,
  Loader2,
  Mail,
  ChevronDown,
  Check,
} from "lucide-react";
import { motion } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { Role } from "@/lib/types";
import { Card, Badge, Button } from "@/components/ui";

/* ------------------------------------------------------------------ */
/*  Role config                                                        */
/* ------------------------------------------------------------------ */

const ROLES: { value: Role; label: string; icon: React.ElementType; description: string }[] = [
  { value: "owner", label: "Owner", icon: Crown, description: "Full control including billing and org deletion" },
  { value: "admin", label: "Admin", icon: ShieldCheck, description: "Manage members, machines, and settings" },
  { value: "operator", label: "Operator", icon: Wrench, description: "Manage machines, workloads, and migrations" },
  { value: "viewer", label: "Viewer", icon: Eye, description: "Read-only access to all resources" },
];

function roleVariant(role: string): "default" | "success" | "warning" | "error" | "info" | "accent" {
  switch (role) {
    case "owner": return "accent";
    case "admin": return "success";
    case "operator": return "info";
    case "viewer": return "default";
    default: return "default";
  }
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function TeamPage() {
  const orgId = useAuth((s) => s.organization?.id || "");
  const queryClient = useQueryClient();

  const { isLoading } = useQuery({
    queryKey: ["organizations"],
    queryFn: () => api.listOrganizations(),
  });

  const [inviteEmail, setInviteEmail] = React.useState("");
  const [inviteRole, setInviteRole] = React.useState<Role>("operator");
  const [showRoleMenu, setShowRoleMenu] = React.useState(false);
  const [inviteSuccess, setInviteSuccess] = React.useState(false);

  const invite = useMutation({
    mutationFn: () => api.upsertMember(orgId, inviteEmail, inviteRole),
    onSuccess: () => {
      setInviteEmail("");
      setInviteRole("operator");
      setInviteSuccess(true);
      setTimeout(() => setInviteSuccess(false), 3000);
      queryClient.invalidateQueries({ queryKey: ["organizations"] });
    },
  });

  return (
    <div>
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl font-semibold text-text">Team</h1>
        <p className="text-sm text-text-muted mt-1">Manage organization members and roles</p>
      </div>

      {/* Invite */}
      <Card className="p-5 mb-6">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">Invite Member</h2>
        <div className="flex items-end gap-3">
          <div className="flex-1">
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Email</label>
            <div className="relative">
              <Mail className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-text-muted" />
              <input
                type="email"
                value={inviteEmail}
                onChange={(e) => setInviteEmail(e.target.value)}
                placeholder="user@example.com"
                className="w-full pl-10 pr-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text placeholder:text-text-muted focus:outline-none focus:border-accent transition-colors"
              />
            </div>
          </div>

          <div className="w-44">
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Role</label>
            <div className="relative">
              <button
                onClick={() => setShowRoleMenu(!showRoleMenu)}
                className="w-full flex items-center justify-between px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text hover:bg-bg-hover transition-colors"
              >
                <span>{ROLES.find((r) => r.value === inviteRole)?.label}</span>
                <ChevronDown className="w-3.5 h-3.5 text-text-muted" />
              </button>

              {showRoleMenu && (
                <>
                  <div className="fixed inset-0 z-40" onClick={() => setShowRoleMenu(false)} />
                  <div className="absolute right-0 top-full mt-1 z-50 w-full bg-bg-elevated border border-border rounded-lg shadow-xl py-1">
                    {ROLES.filter((r) => r.value !== "owner").map((role) => (
                      <button
                        key={role.value}
                        onClick={() => { setInviteRole(role.value); setShowRoleMenu(false); }}
                        className="w-full text-left px-3 py-2 hover:bg-bg-hover transition-colors"
                      >
                        <div className="flex items-center gap-2">
                          <role.icon className="w-3.5 h-3.5 text-text-muted" />
                          <span className="text-sm text-text">{role.label}</span>
                          {inviteRole === role.value && <Check className="w-3.5 h-3.5 text-accent ml-auto" />}
                        </div>
                      </button>
                    ))}
                  </div>
                </>
              )}
            </div>
          </div>

          <Button
            variant="primary"
            onClick={() => invite.mutate()}
            disabled={!inviteEmail || invite.isPending}
          >
            {invite.isPending ? (
              <Loader2 className="w-4 h-4 animate-spin" />
            ) : (
              <>
                <UserPlus className="w-4 h-4 mr-1.5" />
                Invite
              </>
            )}
          </Button>
        </div>

        {inviteSuccess && (
          <motion.p
            initial={{ opacity: 0, y: -4 }}
            animate={{ opacity: 1, y: 0 }}
            className="text-sm text-accent mt-3"
          >
            Invitation sent successfully.
          </motion.p>
        )}
        {invite.isError && (
          <p className="text-sm text-status-error mt-3">
            Failed to send invitation. {(invite.error as Error)?.message || "Please try again."}
          </p>
        )}
      </Card>

      {/* Role Descriptions */}
      <Card className="p-5 mb-6">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">Roles</h2>
        <div className="grid grid-cols-2 md:grid-cols-4 gap-3">
          {ROLES.map((role) => (
            <div key={role.value} className="p-3 bg-bg-surface rounded-lg border border-border">
              <div className="flex items-center gap-2 mb-1.5">
                <role.icon className="w-3.5 h-3.5 text-text-muted" />
                <span className="text-sm font-medium text-text">{role.label}</span>
              </div>
              <p className="text-xs text-text-muted leading-relaxed">{role.description}</p>
            </div>
          ))}
        </div>
      </Card>

      {/* Members */}
      <Card className="p-5 mb-12">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">Members</h2>
        {isLoading ? (
          <div className="flex items-center justify-center py-12">
            <Loader2 className="w-5 h-5 text-accent animate-spin" />
          </div>
        ) : (
          <div className="space-y-1">
            {/* Current user */}
            <div className="flex items-center justify-between py-3 px-3 rounded-lg bg-bg-surface border border-border">
              <div className="flex items-center gap-3">
                <div className="w-8 h-8 rounded-full bg-accent/10 flex items-center justify-center">
                  <span className="text-sm font-medium text-accent">
                    {(useAuth.getState().user?.email || "?")[0].toUpperCase()}
                  </span>
                </div>
                <div>
                  <p className="text-sm font-medium text-text">
                    {useAuth.getState().user?.display_name || useAuth.getState().user?.email}
                  </p>
                  <p className="text-xs text-text-muted">{useAuth.getState().user?.email}</p>
                </div>
              </div>
              <Badge variant={roleVariant(useAuth.getState().organization?.role || "viewer")}>
                {useAuth.getState().organization?.role || "viewer"}
              </Badge>
            </div>
          </div>
        )}
      </Card>
    </div>
  );
}
