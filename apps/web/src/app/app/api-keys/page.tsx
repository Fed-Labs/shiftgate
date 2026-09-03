"use client";

import React from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Key,
  Plus,
  Copy,
  Check,
  Trash2,
  Loader2,
  AlertTriangle,
  X,
} from "lucide-react";
import { motion, AnimatePresence } from "framer-motion";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import type { APIKey, APIKeyScope, APIKeyCreated } from "@/lib/types";
import { Card, Badge, Mono, Button, EmptyState } from "@/components/ui";
import { timeAgo } from "@/lib/utils";

/* ------------------------------------------------------------------ */
/*  Config                                                             */
/* ------------------------------------------------------------------ */

const ALL_SCOPES: { value: APIKeyScope; label: string; description: string }[] = [
  { value: "read", label: "Read", description: "Read access to all resources" },
  { value: "operate", label: "Operate", description: "Create and manage resources" },
  { value: "admin", label: "Admin", description: "Full administrative access" },
  { value: "machines", label: "Machines", description: "Machine management" },
  { value: "workloads", label: "Workloads", description: "Workload management" },
  { value: "migrations", label: "Migrations", description: "Migration management" },
  { value: "checkpoints", label: "Checkpoints", description: "Checkpoint management" },
];

function keyStatus(key: APIKey): "active" | "expired" | "revoked" {
  if (key.revoked_at) return "revoked";
  if (key.expires_at && new Date(key.expires_at) < new Date()) return "expired";
  return "active";
}

function statusVariant(status: string): "success" | "warning" | "error" {
  switch (status) {
    case "active": return "success";
    case "expired": return "warning";
    case "revoked": return "error";
    default: return "error";
  }
}

/* ------------------------------------------------------------------ */
/*  Page                                                               */
/* ------------------------------------------------------------------ */

export default function APIKeysPage() {
  const orgId = useAuth((s) => s.organization?.id || "");
  const queryClient = useQueryClient();

  const { data: keys, isLoading, error } = useQuery({
    queryKey: ["api-keys", orgId],
    queryFn: () => api.listAPIKeys(orgId),
    enabled: !!orgId,
  });

  const [showCreate, setShowCreate] = React.useState(false);
  const [newName, setNewName] = React.useState("");
  const [selectedScopes, setSelectedScopes] = React.useState<APIKeyScope[]>(["read"]);
  const [createdKey, setCreatedKey] = React.useState<APIKeyCreated | null>(null);
  const [copied, setCopied] = React.useState(false);
  const [revokeConfirm, setRevokeConfirm] = React.useState<string | null>(null);

  const createKey = useMutation({
    mutationFn: () =>
      api.createAPIKey(orgId, {
        name: newName,
        scopes: selectedScopes,
      }),
    onSuccess: (data) => {
      setCreatedKey(data);
      setNewName("");
      setSelectedScopes(["read"]);
      queryClient.invalidateQueries({ queryKey: ["api-keys", orgId] });
    },
  });

  const revokeKey = useMutation({
    mutationFn: (keyId: string) => api.revokeAPIKey(orgId, keyId),
    onSuccess: () => {
      setRevokeConfirm(null);
      queryClient.invalidateQueries({ queryKey: ["api-keys", orgId] });
    },
  });

  const toggleScope = (scope: APIKeyScope) => {
    setSelectedScopes((prev) =>
      prev.includes(scope) ? prev.filter((s) => s !== scope) : [...prev, scope]
    );
  };

  const copySecret = async (secret: string) => {
    await navigator.clipboard.writeText(secret);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div>
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold text-text">API Keys</h1>
          <p className="text-sm text-text-muted mt-1">Manage API keys for programmatic access</p>
        </div>
        <Button variant="primary" onClick={() => { setShowCreate(true); setCreatedKey(null); }}>
          <Plus className="w-4 h-4 mr-1.5" />
          Create Key
        </Button>
      </div>

      {/* Newly created key — show secret ONCE */}
      <AnimatePresence>
        {createdKey && (
          <motion.div
            initial={{ opacity: 0, y: -8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -8 }}
            className="mb-6"
          >
            <Card className="p-5 border-accent/30 bg-accent/5">
              <div className="flex items-start gap-3 mb-4">
                <AlertTriangle className="w-5 h-5 text-accent shrink-0 mt-0.5" />
                <div>
                  <p className="text-sm font-medium text-text">API Key Created</p>
                  <p className="text-xs text-text-muted mt-0.5">
                    Copy this secret now. It will not be shown again.
                  </p>
                </div>
                <button onClick={() => setCreatedKey(null)} className="ml-auto p-1 hover:bg-bg-hover rounded">
                  <X className="w-3.5 h-3.5 text-text-muted" />
                </button>
              </div>

              <div className="flex items-center gap-2">
                <div className="flex-1 p-3 bg-bg rounded-lg border border-border font-mono text-sm text-accent break-all select-all">
                  {createdKey.secret}
                </div>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => copySecret(createdKey.secret)}
                >
                  {copied ? (
                    <Check className="w-4 h-4 text-accent" />
                  ) : (
                    <Copy className="w-4 h-4" />
                  )}
                </Button>
              </div>
            </Card>
          </motion.div>
        )}
      </AnimatePresence>

      {/* Create form */}
      <AnimatePresence>
        {showCreate && !createdKey && (
          <motion.div
            initial={{ opacity: 0, height: 0 }}
            animate={{ opacity: 1, height: "auto" }}
            exit={{ opacity: 0, height: 0 }}
            className="mb-6 overflow-hidden"
          >
            <Card className="p-5">
              <h2 className="text-sm font-medium text-text-secondary uppercase tracking-wider mb-4">New API Key</h2>

              <div className="space-y-4">
                <div>
                  <label className="text-xs text-text-muted uppercase tracking-wider block mb-1.5">Name</label>
                  <input
                    type="text"
                    value={newName}
                    onChange={(e) => setNewName(e.target.value)}
                    placeholder="e.g., CI/CD pipeline"
                    className="w-full px-3 py-2 bg-bg-surface border border-border rounded-lg text-sm text-text placeholder:text-text-muted focus:outline-none focus:border-accent transition-colors"
                  />
                </div>

                <div>
                  <label className="text-xs text-text-muted uppercase tracking-wider block mb-2">Scopes</label>
                  <div className="grid grid-cols-2 md:grid-cols-4 gap-2">
                    {ALL_SCOPES.map((scope) => (
                      <button
                        key={scope.value}
                        onClick={() => toggleScope(scope.value)}
                        className={`p-2.5 rounded-lg border text-left transition-all ${
                          selectedScopes.includes(scope.value)
                            ? "border-accent bg-accent/5"
                            : "border-border bg-bg-surface hover:bg-bg-hover"
                        }`}
                      >
                        <div className="flex items-center gap-2">
                          <div className={`w-3.5 h-3.5 rounded border flex items-center justify-center ${
                            selectedScopes.includes(scope.value)
                              ? "border-accent bg-accent"
                              : "border-text-muted"
                          }`}>
                            {selectedScopes.includes(scope.value) && (
                              <Check className="w-2.5 h-2.5 text-black" />
                            )}
                          </div>
                          <span className="text-sm text-text">{scope.label}</span>
                        </div>
                        <p className="text-xs text-text-muted mt-1 ml-5.5">{scope.description}</p>
                      </button>
                    ))}
                  </div>
                </div>

                <div className="flex items-center gap-2 pt-2">
                  <Button
                    variant="primary"
                    onClick={() => createKey.mutate()}
                    disabled={!newName || selectedScopes.length === 0 || createKey.isPending}
                  >
                    {createKey.isPending ? <Loader2 className="w-4 h-4 animate-spin" /> : "Create Key"}
                  </Button>
                  <Button variant="ghost" onClick={() => setShowCreate(false)}>
                    Cancel
                  </Button>
                </div>
              </div>
            </Card>
          </motion.div>
        )}
      </AnimatePresence>

      {/* Keys list */}
      {isLoading ? (
        <div className="flex items-center justify-center py-32">
          <Loader2 className="w-6 h-6 text-accent animate-spin" />
        </div>
      ) : error ? (
        <EmptyState title="Failed to load API keys" description="Could not fetch API keys." />
      ) : !keys || keys.length === 0 ? (
        <EmptyState
          title="No API keys"
          description="Create an API key to enable programmatic access to the SHIFTGATE API."
          action={
            <Button variant="primary" onClick={() => setShowCreate(true)}>
              <Plus className="w-4 h-4 mr-1.5" />
              Create Key
            </Button>
          }
        />
      ) : (
        <Card className="overflow-hidden">
          <div className="divide-y divide-border">
            {keys.map((key) => {
              const status = keyStatus(key);
              return (
                <div key={key.id} className="p-4 hover:bg-bg-hover transition-colors">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center gap-3">
                      <Key className="w-4 h-4 text-text-muted" />
                      <div>
                        <div className="flex items-center gap-2">
                          <span className="text-sm font-medium text-text">{key.name}</span>
                          <Badge variant={statusVariant(status)}>{status}</Badge>
                        </div>
                        <div className="flex items-center gap-3 mt-1">
                          <Mono className="text-xs text-text-muted">{key.prefix}…</Mono>
                          <span className="text-xs text-text-muted">Created {timeAgo(key.created_at)}</span>
                          {key.last_used_at && (
                            <span className="text-xs text-text-muted">Used {timeAgo(key.last_used_at)}</span>
                          )}
                        </div>
                      </div>
                    </div>

                    <div className="flex items-center gap-2">
                      <div className="flex gap-1 flex-wrap justify-end">
                        {key.scopes.map((s) => (
                          <Mono key={s} className="text-[10px] bg-bg-surface px-1.5 py-0.5 rounded border border-border">
                            {s}
                          </Mono>
                        ))}
                      </div>

                      {status === "active" && (
                        <div className="relative">
                          {revokeConfirm === key.id ? (
                            <div className="flex items-center gap-1">
                              <Button variant="danger" size="sm" onClick={() => revokeKey.mutate(key.id)}>
                                Confirm
                              </Button>
                              <Button variant="ghost" size="sm" onClick={() => setRevokeConfirm(null)}>
                                Cancel
                              </Button>
                            </div>
                          ) : (
                            <Button variant="ghost" size="sm" onClick={() => setRevokeConfirm(key.id)}>
                              <Trash2 className="w-3.5 h-3.5 text-text-muted" />
                            </Button>
                          )}
                        </div>
                      )}
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        </Card>
      )}
    </div>
  );
}
