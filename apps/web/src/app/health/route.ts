// Liveness probe for platform healthchecks (load balancers). The
// dashboard holds no server state: a constant 200 is the whole check. A route
// handler, not a page route, so the sitemap gate never sees it.
export function GET() {
  return Response.json({ status: "ok" });
}
