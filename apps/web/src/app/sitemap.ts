import type { MetadataRoute } from "next";

const BASE = "https://shiftgate.dev";

export default function sitemap(): MetadataRoute.Sitemap {
  const staticPages = [
    "",
    "/product",
    "/technology",
    "/security",
    "/developers",
    "/pricing",
    "/download",
    "/about",
    "/contact",
    "/changelog",
    "/enterprise",
    "/status",
    "/compute",
    "/docs",
    "/docs/quickstart",
    "/docs/concepts",
    "/docs/machines",
    "/docs/workloads",
    "/docs/checkpoints",
    "/docs/migration",
    "/docs/api",
    "/docs/cli",
    "/docs/security",
    "/docs/troubleshooting",
  ];

  return staticPages.map((path) => ({
    url: `${BASE}${path}`,
    lastModified: new Date(),
    changeFrequency: path === "" ? "weekly" : "monthly",
    priority: path === "" ? 1 : path.startsWith("/docs") ? 0.7 : 0.8,
  }));
}
