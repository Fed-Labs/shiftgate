"use client";

import React from "react";
import { useAuth } from "@/lib/store";
import { Card, Badge, Mono, Button } from "@/components/ui";
import { AlertTriangle, User, Building2 } from "lucide-react";
import { motion, AnimatePresence } from "framer-motion";

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function SettingsPage() {
  const { user, organization } = useAuth();
  const [deleteConfirm, setDeleteConfirm] = React.useState(false);
  const [deleteInput, setDeleteInput] = React.useState("");

  const orgName = organization?.name || "";
  const canDelete = deleteInput.toLowerCase() === orgName.toLowerCase();

  return (
    <div>
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
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Name</label>
            <input
              type="text"
              defaultValue={orgName}
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text focus:outline-none focus:border-accent transition-colors"
            />
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
              {organization?.created_at ? new Date(organization.created_at).toLocaleDateString() : "—"}
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
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Display Name</label>
            <input
              type="text"
              defaultValue={user?.display_name || ""}
              className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text focus:outline-none focus:border-accent transition-colors"
            />
          </div>

          <div>
            <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Email</label>
            <input
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

        <div className="flex items-center justify-between">
          <div>
            <p className="text-sm font-medium text-text">Delete Organization</p>
            <p className="text-xs text-text-muted mt-0.5">
              Permanently delete this organization and all its data. This cannot be undone.
            </p>
          </div>

          <AnimatePresence>
            {!deleteConfirm ? (
              <Button variant="danger" size="sm" onClick={() => setDeleteConfirm(true)}>
                Delete
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
                  placeholder={`Type "${orgName}"`}
                  className="px-3 py-1.5 bg-bg-surface border border-status-error/30 rounded-lg text-sm text-text placeholder:text-text-muted focus:outline-none focus:border-status-error w-44"
                />
                <Button
                  variant="danger"
                  size="sm"
                  disabled={!canDelete}
                  onClick={() => {
                    // Would call delete API here
                    setDeleteConfirm(false);
                    setDeleteInput("");
                  }}
                >
                  Confirm Delete
                </Button>
                <Button variant="ghost" size="sm" onClick={() => { setDeleteConfirm(false); setDeleteInput(""); }}>
                  Cancel
                </Button>
              </motion.div>
            )}
          </AnimatePresence>
        </div>
      </Card>
    </div>
  );
}
