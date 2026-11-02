import type { MetadataRoute } from "next";

const BASE = process.env.NEXT_PUBLIC_SITE_URL || "https://shiftgate.dev";

const GUIDES = [
  "/guides/linux-workload-mobility",
  "/guides/criu-checkpoint-restore-tutorial",
  "/guides/migrate-running-process-between-linux-servers",
  "/guides/live-migration-without-shared-storage",
  "/guides/gpu-node-drain-without-requeue",
  "/guides/burst-ci-workers-from-checkpoint",
  "/guides/warm-standby-linux-workloads",
  "/guides/criu-limitations-explained",
  "/guides/checkpoint-security-and-encryption",
  "/guides/fleet-checkpoint-policy",
  "/guides/air-gapped-workload-migration",
  "/guides/clone-checkpoint-into-n-workers",
  "/guides/linux-maintenance-window-without-downtime-cost",
  "/guides/criu-lazy-restore",
  "/guides/criu-tcp-repair",
  "/guides/criu-pre-dump",
  "/guides/criu-process-tree",
];

const COMPARES = [
  "/compare/criu-scripts-vs-shift",
  "/compare/vm-live-migration-vs-shift",
  "/compare/restart-and-rewarm-vs-checkpoint",
  "/compare/rsync-and-scatter-vs-shift",
  "/compare/shift-vs-manual-checkpointing",
];

const USE_CASES = [
  "/use-cases/ml-gpu-fleets",
  "/use-cases/ci-burst-clusters",
  "/use-cases/homelab-maintenance",
  "/use-cases/research-computing",
  "/use-cases/edge-fleets",
  "/use-cases/failover-and-dr",
];

export default function sitemap(): MetadataRoute.Sitemap {
  const staticPages = [
    "",
    "/product",
    "/technology",
    "/security",
    "/developers",
    "/pricing",
    "/upgrade",
    "/trial",
    "/download",
    "/about",
    "/contact",
    "/changelog",
    "/enterprise",
    "/status",
    "/compute",
    "/numbers",
    "/docs",
    "/docs/quickstart",
    "/docs/ci-burst",
    "/docs/concepts",
    "/docs/machines",
    "/docs/platforms",
    "/docs/workloads",
    "/docs/checkpoints",
    "/docs/migration",
    "/docs/operations",
    "/docs/benchmarks",
    "/docs/storage",
    "/docs/marketplace",
    "/docs/api",
    "/docs/cli",
    "/docs/security",
    "/docs/troubleshooting",
    "/docs/versioning",
    "/guides",
    ...GUIDES,
    ...COMPARES,
    ...USE_CASES,
  ];

  return staticPages.map((path) => ({
    url: `${BASE}${path}`,
    changeFrequency: path === "" ? "weekly" : path.startsWith("/docs") ? "monthly" : "monthly",
    priority: path === "" ? 1 : path.startsWith("/docs") ? 0.7 : 0.8,
  }));
}
