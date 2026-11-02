import { connection } from "next/server";
import { AppShell } from "./app-shell";

// The dashboard renders per request so the CSP proxy can nonce the inline
// hydration scripts: Next.js injects nonces during server rendering only, so
// the security-critical surface (the authenticated app) is dynamic while the
// marketing and docs pages stay statically prerendered.
export default async function AppLayout({ children }: { children: React.ReactNode }) {
  await connection();
  return <AppShell>{children}</AppShell>;
}
