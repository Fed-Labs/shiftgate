"use client";

import { useEffect, useState, useSyncExternalStore } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import Image from "next/image";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Button, Card } from "@/components/ui";
import { apiErrorCode, errorMessage } from "@/lib/utils";
import {
  GITHUB_NOT_CONFIGURED_MESSAGE,
  cleanGithubSearch,
  githubRedirectUri,
  githubReturnMessage,
  isGithubUnavailableCode,
  readGithubReturn,
  takeGithubReturn,
} from "@/lib/github-auth";
import { GithubSignIn } from "@/components/github-signin";

type SsoState = "unknown" | "checking" | "required" | "optional";

// The refresh credential is a SameSite=Strict, host-only cookie. When the
// dashboard is served from a different *site* than the API, browsers never
// attach it to /v1/auth/refresh, so "30 days" silently degrades to the
// 15-minute access token. Surface the topology up front instead of a later
// mystery logout.
function crossSiteDeployment(): boolean {
  if (typeof window === "undefined") return false;
  try {
    const apiUrl = new URL(process.env.NEXT_PUBLIC_API_URL || "https://shiftgate.dev");
    const here = new URL(window.location.origin);
    const site = (host: string) => host.split(".").slice(-2).join(".");
    return site(apiUrl.hostname) !== site(here.hostname);
  } catch {
    return false;
  }
}

// Static for the page lifetime — the origin never changes under it.
const subscribeNoop = () => () => {};

// Browser single sign-on: PKCE (RFC 7636) against the control plane's
// /v1/auth/sso/authorize + /v1/auth/sso/token pair. The verifier and the
// minted state survive the identity-provider redirect in sessionStorage, and
// this page doubles as the redirect target (?code=&state=).
const SSO_FLOW_KEY = "shiftgate-sso-flow";

type SsoFlow = { verifier: string; state: string; email: string; redirect_uri: string };

function base64Url(bytes: Uint8Array): string {
  let binary = "";
  bytes.forEach((b) => {
    binary += String.fromCharCode(b);
  });
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function ssoPkce(): Promise<{ verifier: string; challenge: string }> {
  const random = new Uint8Array(32);
  crypto.getRandomValues(random);
  const verifier = base64Url(random);
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return { verifier, challenge: base64Url(new Uint8Array(digest)) };
}

function ssoRedirectUri(): string {
  return `${window.location.origin}${window.location.pathname}`;
}

function ssoGate(
  domain: string,
  onStartSso: () => void,
  onReset: () => void,
  busy: boolean,
  note: string | null
) {
  return (
    <div className="space-y-4" role="status">
      <div className="bg-accent/10 border border-accent/20 rounded-md px-4 py-3 text-sm text-text-secondary">
        <p className="font-medium text-text mb-1">Single sign-on required</p>
        <p>
          {domain ? `The domain ${domain} enforces` : "Your organization enforces"} single
          sign-on, so password sign-in is disabled. Continue to your identity provider here,
          or use the CLI (<span className="font-mono text-xs">shiftgate login --sso</span>) or
          the SHIFT desktop app.
        </p>
      </div>
      <Button type="button" className="w-full" onClick={onStartSso} disabled={busy}>
        {busy ? "Starting sign-on…" : "Continue with SSO"}
      </Button>
      {note && <p className="text-xs text-status-warning leading-relaxed">{note}</p>}
      <Button type="button" variant="secondary" className="w-full" onClick={onReset}>
        Use a different email
      </Button>
    </div>
  );
}

export default function LoginPage() {
  const router = useRouter();
  const { setAuth, restoreOrganization, user, tokens } = useAuth();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [remember, setRemember] = useState(false);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [sso, setSso] = useState<SsoState>("unknown");
  const [ssoBusy, setSsoBusy] = useState(false);
  const [ssoError, setSsoError] = useState<string | null>(null);
  const [githubBusy, setGithubBusy] = useState(false);
  const crossSite = useSyncExternalStore(subscribeNoop, crossSiteDeployment, () => false);

  // An already signed-in visitor has no business minting a second session.
  useEffect(() => {
    if (user && tokens) router.replace("/app");
  }, [user, tokens, router]);

  useEffect(() => {
    const ret = takeGithubReturn(window.location.search);
    if (!ret) return;
    router.replace(window.location.pathname + cleanGithubSearch(window.location.search));
    window.setTimeout(() => {
      if (ret.kind === "error") {
        setError(githubReturnMessage(ret.code));
        return;
      }
      setGithubBusy(true);
      (async () => {
        try {
          const { user: signedIn, tokens: session } = await api.githubToken({ code: ret.code });
          setAuth(signedIn, session);
          await restoreOrganization();
          router.push("/app");
        } catch (err: unknown) {
          if (isGithubUnavailableCode(apiErrorCode(err))) {
            setError(GITHUB_NOT_CONFIGURED_MESSAGE);
          } else {
            setError(errorMessage(err, "GitHub sign-in failed"));
          }
          setGithubBusy(false);
        }
      })();
    }, 0);
  }, [router, setAuth, restoreOrganization]);

  // The identity provider redirects back to this page with ?code=&state= —
  // finish the PKCE exchange exactly once, then clear the query string. The
  // React state work runs in a deferred callback, never in the effect body.
  useEffect(() => {
    if (readGithubReturn(window.location.search)) return;
    const params = new URLSearchParams(window.location.search);
    const code = params.get("code");
    const state = params.get("state");
    const idpError = params.get("error");
    if (!code && !idpError) return;
    const raw = window.sessionStorage.getItem(SSO_FLOW_KEY);
    window.sessionStorage.removeItem(SSO_FLOW_KEY);
    window.history.replaceState({}, "", window.location.pathname);
    const handle = window.setTimeout(() => {
      if (idpError) {
        setError(`Single sign-on was cancelled or refused (${idpError}).`);
        return;
      }
      let flow: SsoFlow | null = null;
      try {
        flow = raw ? (JSON.parse(raw) as SsoFlow) : null;
      } catch {
        flow = null;
      }
      if (!flow || !code || !state || state !== flow.state) {
        setError("The single sign-on response did not match this session. Start again.");
        return;
      }
      setSsoBusy(true);
      (async () => {
        try {
          const { user: signedIn, tokens: session } = await api.ssoToken({
            code,
            state,
            code_verifier: flow.verifier,
            redirect_uri: flow.redirect_uri,
          });
          setAuth(signedIn, session);
          await restoreOrganization();
          router.push("/app");
        } catch (err: unknown) {
          setError(errorMessage(err, "Single sign-on failed"));
          setSsoBusy(false);
        }
      })();
    }, 0);
    return () => window.clearTimeout(handle);
  }, [router, setAuth, restoreOrganization]);

  // SSO-enforced domains refuse password auth (403 SSO_REQUIRED): detect the
  // enforcement before asking for a password that can never succeed.
  const checkSso = async (value: string): Promise<boolean | null> => {
    const trimmed = value.trim();
    if (!trimmed.includes("@")) return null;
    setSso("checking");
    try {
      const { required } = await api.ssoRequired(trimmed);
      setSso(required ? "required" : "optional");
      return required;
    } catch {
      // A failed check must not block password sign-in — the server's typed
      // SSO_REQUIRED rejection is the backstop.
      setSso("unknown");
      return null;
    }
  };

  // Start the browser SSO flow: mint a PKCE pair, ask the control plane for
  // the issuer URL, and hand the tab to the identity provider.
  const startSso = async () => {
    setSsoError(null);
    setSsoBusy(true);
    try {
      const { verifier, challenge } = await ssoPkce();
      const redirect_uri = ssoRedirectUri();
      const { authorization_url, state } = await api.ssoAuthorize({
        email: email.trim(),
        redirect_uri,
        code_challenge: challenge,
      });
      window.sessionStorage.setItem(
        SSO_FLOW_KEY,
        JSON.stringify({ verifier, state, email: email.trim(), redirect_uri })
      );
      window.location.assign(authorization_url);
    } catch (err: unknown) {
      if (apiErrorCode(err) === "REDIRECT_INVALID") {
        setSsoError(
          "This control plane only accepts loopback sign-on redirects, so browser SSO cannot complete from this address. Use the CLI (shiftgate login --sso) or the SHIFT desktop app, or serve the dashboard from the same host as the control plane."
        );
      } else {
        setSsoError(errorMessage(err, "Could not start single sign-on"));
      }
      setSsoBusy(false);
    }
  };

  const startGithub = async () => {
    setError("");
    setGithubBusy(true);
    try {
      const { authorize_url } = await api.githubAuthorize({ redirect_uri: githubRedirectUri() });
      window.location.assign(authorize_url);
    } catch (err: unknown) {
      if (isGithubUnavailableCode(apiErrorCode(err))) {
        setError(GITHUB_NOT_CONFIGURED_MESSAGE);
      } else {
        setError(errorMessage(err, "Could not start GitHub sign-in"));
      }
      setGithubBusy(false);
    }
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");
    setLoading(true);

    try {
      const enforced = await checkSso(email);
      if (enforced) return;
      const { user: signedIn, tokens: session } = await api.login(email, password, remember);
      setAuth(signedIn, session);
      // A login response carries no organization; adopt the user's existing
      // one so the dashboard's organization pages have an id to query with.
      await restoreOrganization();
      router.push("/app");
    } catch (err: unknown) {
      if (apiErrorCode(err) === "SSO_REQUIRED") {
        setSso("required");
      } else {
        setError(errorMessage(err, "Login failed"));
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-bg px-4">
      <div className="w-full max-w-md">
        <div className="mb-8 text-center">
          <div className="flex justify-center mb-4">
            <Image
              src="/logo.png"
              alt="SHIFT"
              width={180}
              height={42}
              className="h-10 w-auto"
              priority
            />
          </div>
          <p className="text-text-secondary text-sm">Move running computation between machines</p>
        </div>

        <Card className="p-8">
          <h2 className="text-xl font-semibold text-text mb-6">Sign in</h2>

          {error && (
            <div className="bg-status-error/10 border border-status-error/20 rounded-md px-4 py-3 text-sm text-status-error mb-4">
              {error}
            </div>
          )}

          <GithubSignIn busy={githubBusy} onStart={() => void startGithub()} />

          {sso === "required" ? (
            ssoGate(
              email.trim().split("@")[1] || "",
              () => void startSso(),
              () => {
                setSso("unknown");
                setPassword("");
                setError("");
                setSsoError(null);
              },
              ssoBusy,
              ssoError
            )
          ) : (
          <form onSubmit={handleSubmit} className="space-y-4">
            <div>
              <label htmlFor="email" className="block text-sm font-medium text-text-secondary mb-2">
                Email
              </label>
              <input
                id="email"
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                onBlur={() => void checkSso(email)}
                required
                className="w-full px-3 py-2 bg-bg-surface border border-border rounded-md text-text placeholder:text-text-muted focus:outline-none focus:ring-2 focus:ring-accent focus:border-transparent"
                placeholder="you@example.com"
              />
              {sso === "checking" && (
                <p className="text-xs text-text-muted mt-1">Checking sign-on requirements…</p>
              )}
            </div>

            <div>
              <label htmlFor="password" className="block text-sm font-medium text-text-secondary mb-2">
                Password
              </label>
              <input
                id="password"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                className="w-full px-3 py-2 bg-bg-surface border border-border rounded-md text-text placeholder:text-text-muted focus:outline-none focus:ring-2 focus:ring-accent focus:border-transparent"
                placeholder="••••••••"
              />
            </div>

            <label htmlFor="remember" className="flex items-center gap-2 text-sm text-text-secondary select-none cursor-pointer">
              <input
                id="remember"
                type="checkbox"
                checked={remember}
                onChange={(e) => setRemember(e.target.checked)}
                className="h-4 w-4 rounded border-border bg-bg-surface text-accent focus:ring-accent"
              />
              Keep me signed in for 30 days
            </label>

            {crossSite && (
              <p className="text-xs text-status-warning leading-relaxed">
                This dashboard and the API are on different sites, so the browser will not
                send the session cookie between them — sign-in will last about 15 minutes,
                not 30 days. Serve the dashboard from the same site as the API (see
                SHIFT_ALLOWED_ORIGINS in the troubleshooting docs) for full session length.
              </p>
            )}

            <Button type="submit" className="w-full" disabled={loading}>
              {loading ? "Signing in..." : "Sign in"}
            </Button>
          </form>
          )}

          <div className="mt-6 text-center text-sm text-text-secondary">
            Don&apos;t have an account?{" "}
            <Link href="/signup" className="text-accent hover:text-accent-dim font-medium">
              Sign up
            </Link>
          </div>
        </Card>
      </div>
    </div>
  );
}
