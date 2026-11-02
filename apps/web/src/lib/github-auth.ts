export type GithubReturn = { kind: "code"; code: string } | { kind: "error"; code: string };

export const GITHUB_REF_KEY = "shiftgate-github-ref";

export const GITHUB_NOT_CONFIGURED_MESSAGE =
  "GitHub sign-in is not available on this deployment yet. Use email and password instead.";

export function readGithubReturn(search: string): GithubReturn | null {
  const params = new URLSearchParams(search);
  const code = params.get("github_code");
  if (code) return { kind: "code", code };
  const error = params.get("error");
  if (error && error.startsWith("github_")) return { kind: "error", code: error };
  return null;
}

let githubReturnConsumed = false;

export function takeGithubReturn(search: string): GithubReturn | null {
  if (githubReturnConsumed) return null;
  const ret = readGithubReturn(search);
  if (!ret) return null;
  githubReturnConsumed = true;
  return ret;
}

export function cleanGithubSearch(search: string): string {
  const params = new URLSearchParams(search);
  params.delete("github_code");
  const error = params.get("error");
  if (error && error.startsWith("github_")) params.delete("error");
  const rest = params.toString();
  return rest ? `?${rest}` : "";
}

export function githubRedirectUri(): string {
  return `${window.location.origin}${window.location.pathname}`;
}

export function isGithubUnavailableCode(code: string | null): boolean {
  return code === "GITHUB_NOT_CONFIGURED" || code === "unknown";
}

export function githubReturnMessage(code: string): string {
  switch (code) {
    case "github_auth_failed":
      return "GitHub sign-in was cancelled or refused. Try again whenever you are ready.";
    case "github_email_not_verified":
      return "Your GitHub account's primary email address is not verified. Verify it on GitHub, then try again.";
    case "github_org_denied":
      return "Your GitHub account is not in an organization that can use this deployment.";
    default:
      return `GitHub sign-in failed (${code}).`;
  }
}
