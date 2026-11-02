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
import Link from "next/link";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { PlanCatalogEntry, Role } from "@/lib/types";
import { Card, Badge, Button } from "@/components/ui";
import { errorMessage, formatCurrency } from "@/lib/utils";
import { getUsageSummary } from "../billing/plan-api";

/* ------------------------------------------------------------------ */
/*  Role config                                                        */
/* ------------------------------------------------------------------ */

const ROLES: { value: Role; label: string; icon: React.ElementType; description: string }[] = [
  { value: "owner", label: "Owner", icon: Crown, description: "Full control including billing and organization settings" },
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

// The subscription status vocabulary from the control plane: only these three
// carry plan entitlements (internal/database/entitlements.go Active()).
const ACTIVE_STATUSES = ["active", "trialing", "past_due"];

// Mirrors billing.PlanFeatureAllowed: a plan outside the catalog (a negotiated
// tier) includes every catalog feature; free never includes team members.
function planAllowsTeamMembers(plan: PlanCatalogEntry | undefined, planKey: string): boolean {
  if (plan) return !!plan.team_members;
  return planKey !== "free" && planKey !== "";
}

function formatSeatPrice(cents: number): string {
  return formatCurrency(cents);
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function TeamPage() {
  const { user, organization } = useAuth();
  const orgId = organization?.id || "";
  const role = organization?.role || "viewer";
  const canManage = role === "owner" || role === "admin";
  const queryClient = useQueryClient();

  const { isLoading } = useQuery({
    queryKey: ["organizations"],
    queryFn: () => api.listOrganizations(),
  });

  // Seat usage surfaces on seat plans (Business: 5 included, +$19/extra).
  // Quantities are enforced server-side at checkout and when members are
  // added (SEATS_EXCEEDED) — this page reflects, never decides, them.
  const { data: catalog } = useQuery({ queryKey: ["plans"], queryFn: () => api.listPlans() });
  const { data: entitlement } = useQuery({
    queryKey: ["entitlement", orgId],
    queryFn: () => api.getEntitlement(orgId),
    enabled: !!orgId,
  });
  const { data: usageSummary } = useQuery({
    queryKey: ["usage-summary", orgId],
    queryFn: () => getUsageSummary(orgId),
    enabled: !!orgId,
    retry: false,
  });
  const plan = catalog?.find((p) => p.key === entitlement?.plan);
  const subscriptionActive = !!entitlement && ACTIVE_STATUSES.includes(entitlement.status);
  const trialRunning = !!entitlement?.trial_active;
  // During a trial the entitlement carries Business features while the plan
  // column still reads "free"; show the Business seat model for the window.
  const effectivePlan: PlanCatalogEntry | undefined = trialRunning
    ? catalog?.find((p) => p.key === "business") ?? plan
    : plan;
  const teamEnabled = !!entitlement && subscriptionActive &&
    (trialRunning || planAllowsTeamMembers(plan, entitlement.plan));

  const includedSeats = effectivePlan?.included_seats ?? 0;
  const seatOverageCents = effectivePlan?.seat_overage_cents ?? 0;
  const seatsIncluded = usageSummary?.seats.included ?? includedSeats;
  const seatsPurchased = usageSummary?.seats.purchased ?? includedSeats;
  const memberCount = usageSummary?.seats.members ?? 0;

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
      queryClient.invalidateQueries({ queryKey: ["usage-summary", orgId] });
    },
  });

  const roleLabel = ROLES.find((r) => r.value === inviteRole)?.label || inviteRole;

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl font-semibold text-text">Team</h1>
        <p className="text-sm text-text-muted mt-1">Manage organization members and roles</p>
      </div>

      {/* Invite */}
      <Card className="p-5 mb-6">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">Add Member</h2>
        {!canManage ? (
          <p className="text-sm text-text-muted">
            Adding members requires the admin or owner role — your role is {role}.
          </p>
        ) : !teamEnabled ? (
          <div>
            <p className="text-sm text-text-secondary">
              Team members are a Business feature (also included on Enterprise
              and during the 14-day trial). Your current plan is{" "}
              {effectivePlan?.name ?? "Free"} — upgrade or start the trial to
              add members.
            </p>
            <div className="mt-3">
              <Link href="/app/billing">
                <span className="text-xs text-accent hover:underline">
                  See upgrade and trial options in Billing →
                </span>
              </Link>
            </div>
          </div>
        ) : (
          <>
            {includedSeats > 0 && (
              <p className="text-xs text-text-muted mb-3">
                Seats: {memberCount > 0 ? memberCount : 1} of{" "}
                {seatsPurchased > 0 ? seatsPurchased : seatsIncluded} used ·{" "}
                {seatsIncluded} included · {formatSeatPrice(seatOverageCents)}/seat/mo beyond that. Direct
                additions past the seat ceiling are refused with{" "}
                <span className="font-mono text-xs">SEATS_EXCEEDED</span> —
                purchase more seats in the billing portal first.
              </p>
            )}
            <div className="flex items-end gap-3">
          <div className="flex-1">
            <label
              htmlFor="invite-email"
              className="text-xs text-text-muted uppercase tracking-wider block mb-1.5"
            >
              Email
            </label>
            <div className="relative">
              <Mail className="absolute left-3 top-1/2 -translate-y-1/2 w-4 h-4 text-text-muted" />
              <input
                id="invite-email"
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
            <div
              className="relative"
              onKeyDown={(e) => {
                if (e.key === "Escape") setShowRoleMenu(false);
              }}
            >
              <button
                onClick={() => setShowRoleMenu(!showRoleMenu)}
                aria-label="Role"
                aria-haspopup="true"
                aria-expanded={showRoleMenu}
                className="w-full flex items-center justify-between px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text hover:bg-bg-hover transition-colors"
              >
                <span>{ROLES.find((r) => r.value === inviteRole)?.label}</span>
                <ChevronDown className="w-3.5 h-3.5 text-text-muted" />
              </button>

              {showRoleMenu && (
                <>
                  <div className="fixed inset-0 z-40" onClick={() => setShowRoleMenu(false)} />
                  <div
                    role="menu"
                    aria-label="Role"
                    className="absolute right-0 top-full mt-1 z-50 w-full bg-bg-elevated border border-border rounded-lg shadow-xl py-1"
                  >
                    {ROLES.filter((r) => r.value !== "owner").map((role) => (
                      <button
                        key={role.value}
                        role="menuitemradio"
                        aria-checked={inviteRole === role.value}
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
                Add
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
            Member added with the {roleLabel} role.
          </motion.p>
        )}
        {invite.isError && (
          <p className="text-sm text-status-error mt-3">
            {errorMessage(invite.error, "Could not add the member.")}
          </p>
        )}
          </>
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
      <Card className="p-5 mb-6">
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
                    {(user?.email || "?")[0].toUpperCase()}
                  </span>
                </div>
                <div>
                  <p className="text-sm font-medium text-text">
                    {user?.display_name || user?.email}
                  </p>
                  <p className="text-xs text-text-muted">{user?.email}</p>
                </div>
              </div>
              <Badge variant={roleVariant(role)}>{role}</Badge>
            </div>
          </div>
        )}
        <p className="text-xs text-text-muted mt-3">
          {memberCount > 0
            ? `This organization has ${memberCount} member${memberCount === 1 ? "" : "s"}. `
            : null}
          The API does not expose a member-list endpoint yet, so only your own membership is
          shown here. Member removal is not self-serve — remove members through SCIM
          deprovisioning or contact support. On the Business plan, SCIM provisioning keeps the
          identity-provider roster authoritative; changes made with the form above take effect
          immediately (no invitation email is sent).
        </p>
      </Card>

      {/* Seats */}
      <Card className="p-5 mb-12">
        <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">Seats</h2>
        {effectivePlan && (effectivePlan.per_seat || includedSeats > 0) ? (
          <>
            <p className="text-sm text-text-secondary">
              {includedSeats > 0 ? (
                <>
                  The {effectivePlan.name} plan includes {seatsIncluded} seats and adds
                  seats at {formatSeatPrice(seatOverageCents)}/seat/mo beyond that.
                  {" "}Purchased seats: {seatsPurchased > 0 ? seatsPurchased : seatsIncluded}.
                  Members: {memberCount}.
                </>
              ) : (
                <>
                  The {effectivePlan.name} plan is billed per seat. Seats are purchased at checkout
                  and the control plane refuses member additions beyond the seat count with{" "}
                  <span className="font-mono text-xs">SEATS_EXCEEDED</span>.
                </>
              )}
              {" "}The seat quantity is owned by the subscription — change it from the
              billing portal. Seat additions made by SCIM provisioning are not seat-capped
              today; the identity-provider roster is authoritative there.
            </p>
            <div className="mt-4">
              <Link href="/app/billing">
                <span className="text-xs text-accent hover:underline">Manage seats in Billing →</span>
              </Link>
            </div>
          </>
        ) : (
          <p className="text-sm text-text-muted">
            Your current plan is not seat-metered — members are included up to the team
            feature of the plan. The Business plan covers {seatsIncluded || 5} seats with the
            flat price and adds seats at {formatSeatPrice(seatOverageCents || 1900)}/seat/mo;
            seat usage appears here once you are on it.
          </p>
        )}
      </Card>
    </div>
  );
}
