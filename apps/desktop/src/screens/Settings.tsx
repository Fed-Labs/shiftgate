// Settings — the two endpoints this client talks to: the local agent's Unix
// socket (persisted by the Rust side) and the control plane (HTTPS, with a
// session login). Everything else the app shows is read-only data from those.

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { backend, errorMessage } from "@/lib/backend";
import { useAuth, effectiveControlURL, DEFAULT_CONTROL_URL } from "@/lib/store";
import { APP_VERSION } from "@/lib/version";
import { Badge, Button, Card, ErrorBanner, KeyValue, SectionLabel, Spinner } from "@/components/ui";

export function SettingsScreen() {
  const { controlPlaneURL, agentSocket, tokens, user, organization, organizations, configure, login, logout, selectOrganization } =
    useAuth();

  const [urlDraft, setURLDraft] = useState(effectiveControlURL(controlPlaneURL));
  const [socketDraft, setSocketDraft] = useState(agentSocket);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [loginError, setLoginError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const health = useQuery({ queryKey: ["agent-health"], queryFn: backend.health, refetchInterval: 10_000, retry: false });
  // Surfaces a saved endpoints.json that could not be read or parsed (the
  // Rust side falls back to defaults) instead of reverting silently.
  const config = useQuery({ queryKey: ["backend-config"], queryFn: backend.config, retry: false });

  const saveEndpoints = async () => {
    setBusy(true);
    setSaveError(null);
    try {
      await backend.saveEndpoints(urlDraft.trim(), socketDraft.trim());
      configure(urlDraft.trim(), socketDraft.trim() || "/run/shift/agent.sock");
    } catch (error) {
      setSaveError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  const signIn = async () => {
    setLoginError(null);
    setBusy(true);
    try {
      // Sign-in never requires a prior save: persist the URL first only when
      // it differs from the configured one (empty means the platform default).
      const target = urlDraft.trim() || DEFAULT_CONTROL_URL;
      if (target !== effectiveControlURL(controlPlaneURL)) {
        await backend.saveEndpoints(target, socketDraft.trim() || "/run/shift/agent.sock");
        configure(target, socketDraft.trim() || "/run/shift/agent.sock");
      }
      await login(email.trim(), password);
      setPassword("");
    } catch (error) {
      setLoginError(error instanceof Error ? error.message : String(error));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold">Settings</h1>
        <p className="text-[13px] text-text-muted mt-0.5">
          The desktop client talks to the local agent over its Unix socket and to the control plane over
          HTTPS.
        </p>
      </div>

      {/* Local agent */}
      <Card className="p-4 space-y-3">
        <SectionLabel>Local agent</SectionLabel>
        {health.isLoading ? (
          <Spinner />
        ) : (
          <KeyValue
            label="Connection"
            value={
              <span className="inline-flex items-center gap-2">
                {health.isSuccess ? (
                  <>
                    <Badge tone="healthy">online</Badge>
                    <span className="text-text-muted text-[12px]">
                      agent {health.data?.version} · up {health.data?.uptime}
                    </span>
                  </>
                ) : (
                  <Badge tone="error">
                    unreachable — {errorMessage(health.error) ?? "socket error"}
                  </Badge>
                )}
              </span>
            }
          />
        )}
        <label className="block">
          <span className="tlabel block mb-1.5">Agent socket path</span>
          <input
            value={socketDraft}
            onChange={(event) => setSocketDraft(event.target.value)}
            placeholder="/run/shift/agent.sock"
          />
        </label>
        <p className="text-[12px] text-text-muted leading-relaxed">
          The agent's Unix socket. The SHIFT_AGENT_ENDPOINT environment variable supplies the
          default; a path saved here overrides it.
        </p>
      </Card>

      {/* Control plane */}
      <Card className="p-4 space-y-3">
        <SectionLabel>Control plane</SectionLabel>
        <ErrorBanner message={saveError} />
        <ErrorBanner message={config.data?.load_error ?? null} />
        <ErrorBanner message={loginError} />
        <label className="block">
          <span className="tlabel block mb-1.5">Control plane URL</span>
          <input
            value={urlDraft}
            onChange={(event) => setURLDraft(event.target.value)}
            placeholder={DEFAULT_CONTROL_URL}
          />
        </label>
        <p className="text-[12px] text-text-muted leading-relaxed">
          Defaults to the platform endpoint ({DEFAULT_CONTROL_URL}) — the same default the CLI uses. A
          private deployment overrides it here.
        </p>
        <Button variant="secondary" disabled={busy} onClick={saveEndpoints}>
          Save endpoints
        </Button>

        <div className="hline my-2" />

        {tokens && user ? (
          <div className="space-y-3">
            <KeyValue label="Signed in as" value={user.email} />
            {organization && (
              <div>
                <span className="tlabel block mb-1.5">Organization</span>
                <div className="flex gap-2 flex-wrap">
                  {organizations.map((org) => (
                    <button
                      key={org.id}
                      onClick={() => selectOrganization(org)}
                      className={
                        org.id === organization.id
                          ? "px-3 py-1.5 rounded-md border border-accent/40 bg-accent/10 text-[13px]"
                          : "px-3 py-1.5 rounded-md border border-border text-[13px] text-text-secondary hover:bg-bg-hover"
                      }
                    >
                      {org.name}
                    </button>
                  ))}
                </div>
              </div>
            )}
            <div className="flex items-center gap-2">
              <Button variant="secondary" disabled={busy} onClick={() => void logout()}>
                Sign out
              </Button>
              <span className="text-[12px] text-text-faint">
                Access token expires {new Date(tokens.expires_at).toLocaleTimeString()} — renewed
                automatically while signed in
              </span>
            </div>
          </div>
        ) : (
          <div className="space-y-3">
            <p className="text-[12px] text-text-muted leading-relaxed">
              Sign in to see registered machines across the fleet. The agent connection works without it.
            </p>
            <div className="grid gap-3">
              <label className="block">
                <span className="tlabel block mb-1.5">Email</span>
                <input value={email} onChange={(event) => setEmail(event.target.value)} placeholder="you@example.com" />
              </label>
              <label className="block">
                <span className="tlabel block mb-1.5">Password</span>
                <input
                  type="password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  placeholder="••••••••"
                />
              </label>
            </div>
            <Button variant="primary" disabled={busy} onClick={signIn}>
              {busy ? "Signing in…" : "Sign in"}
            </Button>
            <p className="text-[12px] text-text-muted leading-relaxed">
              SSO organizations: password sign-in is refused when single sign-on is enforced — use{" "}
              <span className="readout">shiftgate login --sso</span> in the CLI instead.
            </p>
          </div>
        )}
      </Card>

      <AgentUpdatesCard />

      {/* About */}
      <Card className="p-4">
        <SectionLabel>About</SectionLabel>
        <KeyValue label="Client" value={`SHIFT desktop ${APP_VERSION}`} />
        <KeyValue label="Agent" value={health.data?.version ?? "—"} />
        <KeyValue label="Machine" value={health.data?.machine_id ?? "—"} />
      </Card>
    </div>
  );
}

/** Agent update state: what is installed, what is staged, and a feed check. */
function AgentUpdatesCard() {
  const queryClient = useQueryClient();
  const status = useQuery({ queryKey: ["agent-updates"], queryFn: backend.updateStatus, retry: false });

  const check = useMutation({
    mutationFn: () => backend.updateCheck(),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["agent-updates"] }),
  });

  // The agent accepts update mutations only from its own euid (root on a
  // typical install): a 403 here is a privilege boundary, not a failure, and
  // the remedy is to run the check as the agent's user.
  const checkFailure = check.isError ? String(check.error instanceof Error ? check.error.message : check.error) : null;
  const forbidden =
    checkFailure !== null &&
    (checkFailure.includes("UPDATE_FORBIDDEN") || checkFailure.includes("403") || checkFailure.toLowerCase().includes("forbidden"));

  return (
    <Card className="p-4 space-y-3">
      <SectionLabel>Agent updates</SectionLabel>
      {status.isLoading ? (
        <Spinner />
      ) : status.isError ? (
        <ErrorBanner message={status.error instanceof Error ? status.error.message : "Update status unavailable"} />
      ) : (
        <>
          <KeyValue label="Installed" value={status.data?.current_version ?? "—"} />
          <KeyValue
            label="Staged"
            value={
              status.data?.pending_version
                ? `${status.data.pending_version}${status.data.restart_required ? " (restart required)" : ""}`
                : "none"
            }
          />
          <KeyValue
            label="Available"
            value={
              typeof status.data?.available?.version === "string"
                ? status.data.available.version
                : "no newer release evaluated"
            }
          />
          {status.data?.last_check_error && (
            <KeyValue label="Last check error" value={status.data.last_check_error} />
          )}
          {(status.data?.blocked?.length ?? 0) > 0 && (
            <KeyValue
              label="Blocked"
              value={status.data!.blocked!.map((entry) => entry.version).join(", ")}
            />
          )}
        </>
      )}
      <div className="flex items-center gap-2">
        <Button variant="secondary" disabled={check.isPending} onClick={() => check.mutate()}>
          {check.isPending ? "Checking…" : "Check the release feed"}
        </Button>
        {check.isError && (
          <span className="text-[12px] text-status-error">
            {checkFailure}
          </span>
        )}
        {check.isSuccess && <span className="text-[12px] text-text-muted">Feed checked.</span>}
      </div>
      {forbidden && (
        <p className="text-[12px] text-status-warning leading-relaxed">
          The agent only accepts update mutations from its own user (root on a typical
          install) while the desktop runs as your user. Run the check from a terminal on
          this machine instead: <span className="readout">sudo shiftgate update check</span>
        </p>
      )}
      <p className="text-[12px] text-text-muted leading-relaxed">
        Applying or rolling back an update is an operator action: use{" "}
        <span className="readout">shiftgate update apply|rollback</span> on this machine.
      </p>
    </Card>
  );
}
