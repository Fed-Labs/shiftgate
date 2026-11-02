import type { NextConfig } from "next";
import { PHASE_PRODUCTION_BUILD } from "next/constants";
import path from "node:path";

const nextConfig: NextConfig = {
  reactStrictMode: true,
  // Minimal production container output (.next/standalone + server.js); the
  // deployment image in deployments/docker/Dockerfile.web ships exactly that.
  output: "standalone",
  turbopack: {
    root: path.resolve(__dirname),
  },
  experimental: {
    optimizePackageImports: ["lucide-react", "framer-motion"],
    useTypeScriptCli: false,
    // Cap static-generation workers. The default tracks host CPU count (11 on
    // the builder), and every worker is a full renderer process — on the
    // memory-capped deployment builder that OOM-kills `next build` mid
    // prerender ("Killed", exit 1, no error output). Two workers keeps the
    // peak flat and the build seconds slower.
    cpus: 2,
  },
  async headers() {
    // The CSP lives in src/proxy.ts: it needs a fresh script nonce per
    // request, which static headers() cannot provide. Only the
    // request-independent hardening headers stay here.
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
        ],
      },
    ];
  },
};

const configForPhase = (phase: string) => {
  // NEXT_PUBLIC_API_URL is inlined into the client bundle at build time. A
  // production build without it would silently bake the api.ts fallback
  // origin, and a production build from a developer checkout would bake that
  // checkout's localhost .env — either way the shipped dashboard would call
  // the wrong control plane. Fail the build instead (report 23). env files
  // are loaded before this config runs, so .env.production satisfies it.
  if (phase === PHASE_PRODUCTION_BUILD && !process.env.NEXT_PUBLIC_API_URL) {
    throw new Error(
      "NEXT_PUBLIC_API_URL must be set for production builds (see apps/web/.env.example; .env.production supplies the default)"
    );
  }
  return nextConfig;
};

export default configForPhase;
