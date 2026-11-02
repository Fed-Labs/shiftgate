import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

// Per-request CSP with a script nonce. A nonce (not 'unsafe-inline' /
// 'unsafe-eval') keeps injected HTML from becoming executable JS while Next's
// own inline bootstrap scripts — which pick up the nonce from the x-nonce
// request header — keep working. See node_modules/next/dist/docs
// content-security-policy.md for the pattern this Next version expects.
const apiOrigin = process.env.NEXT_PUBLIC_API_URL || "https://shiftgate.dev";

export function proxy(request: NextRequest) {
  const nonce = Buffer.from(crypto.randomUUID()).toString("base64");
  // React uses eval only for its development debugging helpers; production
  // never needs 'unsafe-eval'.
  const isDev = process.env.NODE_ENV === "development";
  // Nonces are injected during server rendering only, so the dynamic
  // dashboard (/app) gets a strict nonce policy while the statically
  // prerendered marketing and docs pages — whose inline hydration scripts
  // are baked at build time — fall back to 'self' + 'unsafe-inline' (still
  // without 'unsafe-eval'). See the content-security-policy guide in
  // node_modules/next/dist/docs.
  const isApp = request.nextUrl.pathname.startsWith("/app");
  const scriptSrc = isApp
    ? `script-src 'self' 'nonce-${nonce}' 'strict-dynamic'${isDev ? " 'unsafe-eval'" : ""}`
    : `script-src 'self' 'unsafe-inline'${isDev ? " 'unsafe-eval'" : ""}`;
  const contentSecurityPolicy = [
    "default-src 'self'",
    scriptSrc,
    // Inline style *attributes* (framer-motion, React style props) need
    // 'unsafe-inline' here; scripts do not.
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self' data:",
    `connect-src 'self' ${apiOrigin}`,
    "object-src 'none'",
    "frame-ancestors 'none'",
    "base-uri 'self'",
    "form-action 'self'",
  ].join("; ");

  const requestHeaders = new Headers(request.headers);
  requestHeaders.set("x-nonce", nonce);
  requestHeaders.set("Content-Security-Policy", contentSecurityPolicy);

  const response = NextResponse.next({ request: { headers: requestHeaders } });
  response.headers.set("Content-Security-Policy", contentSecurityPolicy);
  response.headers.set(
    "Strict-Transport-Security",
    "max-age=63072000; includeSubDomains"
  );
  return response;
}

export const config = {
  // Documents and app routes only; hashed static assets do not execute CSP
  // decisions of their own.
  matcher: ["/((?!_next/static|_next/image).*)"],
};
