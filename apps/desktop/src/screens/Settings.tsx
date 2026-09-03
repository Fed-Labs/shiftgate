// Settings — the two endpoints this client talks to: the local agent's Unix
// socket (persisted by the Rust side) and the control plane (HTTPS, with a
// session login). Everything else the app shows is read-only data from those.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { backend } from "@/lib/backend";
import { useAuth } from "@/lib/store";
import { Badge, Button, Card, ErrorBanner, KeyValue, SectionLabel, Spinner } from "@/components/ui";

export function SettingsScreen() {
  const { controlPlaneURL, agentSocket, tokens, user, organization, organizations, configure, login, logout, selectOrganization } =
    useAuth();

  const [urlDraft, setURLDraft] = useState(controlPlaneURL);
  const [socketDraft, setSocketDraft] = useState(agentSocket);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [loginError, setLoginError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const health = useQuery({ queryKey: ["agent-health"], queryFn: backend.health, refetchInterval: 10_000, retry: false });

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
                    unreachable — {health.error instanceof Error ? health.error.message : "socket error"}
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
          The agent's Unix socket. The SHIFT_AGENT_ENDPOINT environment variable overrides this at startup.
        </p>
      </Card>

      {/* Control plane */}
      <Card className="p-4 space-y-3">
        <SectionLabel>Control plane</SectionLabel>
        <ErrorBanner message={saveError} />
        <label className="block">
          <span className="tlabel block mb-1.5">Control plane URL</span>
          <input
            value={urlDraft}
            onChange={(event) => setURLDraft(event.target.value)}
            placeholder="https://shift.example.com"
          />
        </label>
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
                Session expires {new Date(tokens.expires_at).toLocaleTimeString()}
              </span>
            </div>
          </div>
        ) : (
          <div className="space-y-3">
            <ErrorBanner message={loginError} />
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
            <Button variant="primary" disabled={busy || !controlPlaneURL} onClick={signIn}>
              {busy ? "Signing in…" : "Sign in"}
            </Button>
            {!controlPlaneURL && (
              <p className="text-[12px] text-status-warning">
                Set and save the control plane URL first.
              </p>
            )}
          </div>
        )}
      </Card>

      {/* About */}
      <Card className="p-4">
        <SectionLabel>About</SectionLabel>
        <KeyValue label="Client" value="SHIFT desktop 0.1.0" />
        <KeyValue label="Agent" value={health.data?.version ?? "—"} />
        <KeyValue label="Machine" value={health.data?.machine_id ?? "—"} />
      </Card>
    </div>
  );
}
