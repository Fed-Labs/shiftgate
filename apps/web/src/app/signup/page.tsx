"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import Image from "next/image";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Button, Card } from "@/components/ui";
import { apiErrorCode, errorMessage } from "@/lib/utils";
import {
  GITHUB_NOT_CONFIGURED_MESSAGE,
  GITHUB_REF_KEY,
  cleanGithubSearch,
  githubRedirectUri,
  githubReturnMessage,
  isGithubUnavailableCode,
  takeGithubReturn,
} from "@/lib/github-auth";
import { GithubSignIn } from "@/components/github-signin";

type SsoState = "unknown" | "checking" | "required" | "optional";

function ssoGate(domain: string, onReset: () => void) {
  return (
    <div className="space-y-4" role="status">
      <div className="bg-accent/10 border border-accent/20 rounded-md px-4 py-3 text-sm text-text-secondary">
        <p className="font-medium text-text mb-1">Single sign-on required</p>
        <p>
          {domain ? `The domain ${domain} enforces` : "Your organization enforces"} single
          sign-on: password accounts cannot be created or signed in. Your account is
          provisioned through your identity provider — sign in with SSO from the CLI
          (<span className="font-mono text-xs">shiftgate login --sso</span>) or the SHIFT
          desktop app.
        </p>
      </div>
      <Button type="button" variant="secondary" className="w-full" onClick={onReset}>
        Use a different email
      </Button>
    </div>
  );
}

export default function SignupPage() {
  const router = useRouter();
  const { setAuth, restoreOrganization, user, tokens } = useAuth();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [orgName, setOrgName] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [sso, setSso] = useState<SsoState>("unknown");
  const [githubBusy, setGithubBusy] = useState(false);

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
          const { user: created, tokens: session } = await api.githubToken({ code: ret.code });
          setAuth(created, session);
          await restoreOrganization();
          const ref = window.sessionStorage.getItem(GITHUB_REF_KEY);
          window.sessionStorage.removeItem(GITHUB_REF_KEY);
          const orgId = useAuth.getState().organization?.id;
          if (ref && orgId) void api.claimReferral(orgId, ref).catch(() => {});
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

  // Registration is refused for SSO-enforced domains (403 SSO_REQUIRED) —
  // check before the form pretends a password account can exist.
  const checkSso = async (value: string): Promise<boolean | null> => {
    const trimmed = value.trim();
    if (!trimmed.includes("@")) return null;
    setSso("checking");
    try {
      const { required } = await api.ssoRequired(trimmed);
      setSso(required ? "required" : "optional");
      return required;
    } catch {
      setSso("unknown");
      return null;
    }
  };

  const startGithub = async () => {
    setError("");
    setGithubBusy(true);
    try {
      const ref = new URLSearchParams(window.location.search).get("ref");
      if (ref) window.sessionStorage.setItem(GITHUB_REF_KEY, ref);
      else window.sessionStorage.removeItem(GITHUB_REF_KEY);
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

    if (password.length < 12) {
      setError("Password must be at least 12 characters");
      return;
    }

    setLoading(true);

    try {
      const enforced = await checkSso(email);
      if (enforced) return;
      const { user: created, tokens: session, organization } = await api.register(
        email,
        password,
        displayName,
        orgName || undefined
      );
      setAuth(created, session, organization);
      // A /r/<code> referral link lands on /signup?ref=CODE: stamp the code on
      // the new organization. Best effort — a missing or refused code must
      // never block account creation.
      const ref = new URLSearchParams(window.location.search).get("ref");
      if (ref && organization?.id) {
        void api.claimReferral(organization.id, ref).catch(() => {});
      }
      router.push("/app");
    } catch (err: unknown) {
      if (apiErrorCode(err) === "SSO_REQUIRED") {
        setSso("required");
      } else {
        setError(errorMessage(err, "Signup failed"));
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
          <h2 className="text-xl font-semibold text-text mb-6">Create account</h2>

          {error && (
            <div className="bg-status-error/10 border border-status-error/20 rounded-md px-4 py-3 text-sm text-status-error mb-4">
              {error}
            </div>
          )}

          <GithubSignIn busy={githubBusy} onStart={() => void startGithub()} />

          <form onSubmit={handleSubmit} className="space-y-4">
            {sso === "required" ? (
              ssoGate(email.trim().split("@")[1] || "", () => {
                setSso("unknown");
                setPassword("");
                setError("");
              })
            ) : (
            <>
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
              <label htmlFor="displayName" className="block text-sm font-medium text-text-secondary mb-2">
                Display name
              </label>
              <input
                id="displayName"
                type="text"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                required
                className="w-full px-3 py-2 bg-bg-surface border border-border rounded-md text-text placeholder:text-text-muted focus:outline-none focus:ring-2 focus:ring-accent focus:border-transparent"
                placeholder="Jane Doe"
              />
            </div>

            <div>
              <label htmlFor="orgName" className="block text-sm font-medium text-text-secondary mb-2">
                Organization name <span className="text-text-muted">(optional)</span>
              </label>
              <input
                id="orgName"
                type="text"
                value={orgName}
                onChange={(e) => setOrgName(e.target.value)}
                className="w-full px-3 py-2 bg-bg-surface border border-border rounded-md text-text placeholder:text-text-muted focus:outline-none focus:ring-2 focus:ring-accent focus:border-transparent"
                placeholder="Acme Corp"
              />
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
                minLength={12}
                className="w-full px-3 py-2 bg-bg-surface border border-border rounded-md text-text placeholder:text-text-muted focus:outline-none focus:ring-2 focus:ring-accent focus:border-transparent"
                placeholder="Minimum 12 characters"
              />
            </div>

            <Button type="submit" className="w-full" disabled={loading}>
              {loading ? "Creating account..." : "Create account"}
            </Button>
            </>
            )}
          </form>

          <div className="mt-6 text-center text-sm text-text-secondary">
            Already have an account?{" "}
            <Link href="/login" className="text-accent hover:text-accent-dim font-medium">
              Sign in
            </Link>
          </div>
        </Card>
      </div>
    </div>
  );
}
