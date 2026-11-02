import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";
import { SiteHeader, SiteFooter } from "@/components/site-header";

export const metadata: Metadata = {
  title: "Status telemetry unavailable",
  description: "SHIFT service telemetry is not connected in this deployment.",
};

export default function StatusPage() {
  return <><SiteHeader /><main className="pt-14"><Section><Container narrow><p className="text-xs font-mono uppercase tracking-widest text-text-muted mb-4">Status</p><h1 className="text-4xl md:text-5xl font-bold tracking-tight mb-6">Telemetry is not connected.</h1><p className="text-lg text-text-secondary leading-relaxed mb-10">This site does not expose live control-plane or agent health. For a deployment check, use the health and readiness endpoints documented for your own control plane.</p><div className="border border-border bg-bg-elevated p-6 font-mono text-sm text-text-secondary leading-relaxed"><div><span className="text-text-muted">CONTROL PLANE</span> <span className="text-text-faint">NOT MONITORED HERE</span></div><div><span className="text-text-muted">AGENTS</span> <span className="text-text-faint">NOT MONITORED HERE</span></div><div><span className="text-text-muted">STORAGE</span> <span className="text-text-faint">NOT MONITORED HERE</span></div></div></Container></Section></main><SiteFooter /></>;
}
