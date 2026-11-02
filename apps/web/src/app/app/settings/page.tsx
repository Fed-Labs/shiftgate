"use client";

import React from "react";
import { useMutation } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Card, Badge, Mono, Button } from "@/components/ui";
import { errorMessage, formatDate } from "@/lib/utils";
import { AlertTriangle, User, Building2, Loader2 } from "lucide-react";
import { motion, AnimatePresence } from "framer-motion";

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function SettingsPage() {
  const { user, organization, logout } = useAuth();
  const [deleteConfirm, setDeleteConfirm] = React.useState(false);
  const [deleteInput, setDeleteInput] = React.useState("");
  const [eraseMessage, setEraseMessage] = React.useState<{ text: string; kind: "success" | "error" } | null>(null);

  const userEmail = user?.email || "";
  const canErase = deleteInput.toLowerCase() === userEmail.toLowerCase();

  // Account erasure is a real, self-service API (DELETE /v1/users/me): every
  // personal identifier is destroyed, sessions revoked, audit IPs scrubbed.
  const erase = useMutation({
    mutationFn: () => api.eraseUser(),
    onSuccess: () => {
      setEraseMessage({ text: "Account erased. Signing out…", kind: "success" });
      setTimeout(() => void logout(), 1200);
    },
    onError: (error: unknown) =>
      setEraseMessage({ text: errorMessage(error, "Erasure failed"), kind: "error" }),
  });

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      {/* Header */}
      <div className="mb-8">
        <h1 className="text-2xl font-semibold text-text">Settings</h1>
        <p className="text-sm text-text-muted mt-1">Organization and account settings</p>
      </div>

      {/* Organization */}
      <Card className="p-5 mb-6">
        <div className="flex items-center gap-2 mb-4">
          <Building2 className="w-4 h-4 text-text-muted" />
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Organization</h2>
        </div>

        <div className="space-y-4">
          <div>
            <label htmlFor="org-name" className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Name</label>
            <input
              id="org-name"
              type="text"
              readOnly
              defaultValue={organization?.name || ""}
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text-muted cursor-not-allowed focus:outline-none"
            />
            <p className="text-xs text-text-muted mt-1.5">
              Renaming an organization is not available through the API yet — the name is
              fixed at creation.
            </p>
          </div>

          <div>
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Organization ID</label>
            <Mono className="text-sm text-text-muted block px-3 py-2 bg-bg-surface rounded-lg border border-border">
              {organization?.id || "—"}
            </Mono>
          </div>

          <div>
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Your Role</label>
            <Badge variant={organization?.role === "owner" ? "accent" : "default"}>
              {organization?.role || "viewer"}
            </Badge>
          </div>

          <div>
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Created</label>
            <p className="text-sm text-text-muted">
              {organization?.created_at ? formatDate(organization.created_at) : "—"}
            </p>
          </div>
        </div>
      </Card>

      {/* Profile */}
      <Card className="p-5 mb-6">
        <div className="flex items-center gap-2 mb-4">
          <User className="w-4 h-4 text-text-muted" />
          <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider">Profile</h2>
        </div>

        <div className="space-y-4">
          <div>
            <label htmlFor="display-name" className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Display Name</label>
            <input
              id="display-name"
              type="text"
              readOnly
              defaultValue={user?.display_name || ""}
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text-muted cursor-not-allowed focus:outline-none"
            />
            <p className="text-xs text-text-muted mt-1.5">
              Editing your display name is not available through the API yet.
            </p>
          </div>

          <div>
            <label htmlFor="email" className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Email</label>
            <input
              id="email"
              type="email"
              defaultValue={user?.email || ""}
              disabled
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text-muted cursor-not-allowed"
            />
          </div>

          <div>
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">User ID</label>
            <Mono className="text-sm text-text-muted block px-3 py-2 bg-bg-surface rounded-lg border border-border">
              {user?.id || "—"}
            </Mono>
          </div>
        </div>
      </Card>

      {/* Danger Zone */}
      <Card className="p-5 border-status-error/20 mb-12">
        <div className="flex items-center gap-2 mb-4">
          <AlertTriangle className="w-4 h-4 text-status-error" />
          <h2 className="text-sm font-medium text-status-error uppercase tracking-wider">Danger Zone</h2>
        </div>

        <div className="mb-6">
          <p className="text-sm font-medium text-text">Delete Organization</p>
          <p className="text-xs text-text-muted mt-0.5">
            Organization deletion is not available through the API or the dashboard — there is
            no endpoint for it. Contact support to have an organization removed; nothing here
            pretends otherwise.
          </p>
        </div>

        <div className="border-t border-border pt-5 flex items-start justify-between gap-4">
          <div>
            <p className="text-sm font-medium text-text">Erase my account</p>
            <p className="text-xs text-text-muted mt-0.5 max-w-md">
              Permanently erases your personal data in place (GDPR erasure): identifiers are
              destroyed, every session is revoked, and audit IPs are scrubbed. Billing history
              keeps only pseudonymous references. This cannot be undone.
            </p>
          </div>

          <AnimatePresence>
            {!deleteConfirm ? (
              <Button variant="danger" size="sm" onClick={() => setDeleteConfirm(true)}>
                Erase
              </Button>
            ) : (
              <motion.div
                initial={{ opacity: 0, width: 0 }}
                animate={{ opacity: 1, width: "auto" }}
                exit={{ opacity: 0, width: 0 }}
                className="flex items-center gap-2 overflow-hidden"
              >
                <input
                  type="text"
                  value={deleteInput}
                  onChange={(e) => setDeleteInput(e.target.value)}
                  placeholder={`Type "${userEmail}"`}
                  aria-label="Type your email to confirm erasure"
                  className="px-3 py-1.5 bg-bg-surface border border-status-error/30 rounded-lg text-sm text-text placeholder:text-text-muted focus:outline-none focus:border-status-error w-52"
                />
                <Button
                  variant="danger"
                  size="sm"
                  disabled={!canErase || erase.isPending}
                  onClick={() => erase.mutate()}
                >
                  {erase.isPending ? <Loader2 className="w-4 h-4 animate-spin" /> : "Confirm Erase"}
                </Button>
                <Button variant="ghost" size="sm" onClick={() => { setDeleteConfirm(false); setDeleteInput(""); }}>
                  Cancel
                </Button>
              </motion.div>
            )}
          </AnimatePresence>
        </div>

        {eraseMessage && (
          <p className={`text-xs mt-3 ${eraseMessage.kind === "success" ? "text-status-healthy" : "text-status-error"}`}>
            {eraseMessage.text}
          </p>
        )}
      </Card>
    </div>
  );
}
