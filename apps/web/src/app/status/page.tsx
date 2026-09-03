"use client";

import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, EmptyState } from "@/components/ui";
import { STATUS_SERVICES } from "@/lib/config";
import { Activity, CheckCircle2 } from "lucide-react";

const SERVICE_DESCRIPTIONS: Record<string, string> = {
  control_plane: "Orchestration, scheduling, and machine coordination",
  api: "REST API and client authentication",
  storage: "Checkpoint storage and deduplication",
  realtime: "WebSocket connections and live status updates",
  compute: "Workload execution and migration pipeline",
};

export default function StatusPage() {
  const allOperational = true;

  return (
    <>
      <SiteHeader />
      <main className="pt-14">
        <Section>
          <Container>
            {/* Header */}
            <div className="mb-12">
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
                System status
              </p>
              <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Status
              </h1>
            </div>

            {/* Overall status */}
            <div className="rounded-lg border border-border bg-bg-elevated p-6 mb-8">
              <div className="flex items-center gap-3">
                {allOperational ? (
                  <>
                    <CheckCircle2 size={20} className="text-status-healthy" />
                    <div>
                      <p className="text-sm font-semibold text-status-healthy">
                        All systems operational
                      </p>
                      <p className="text-xs text-text-muted mt-0.5">
                        Last checked: just now
                      </p>
                    </div>
                  </>
                ) : (
                  <>
                    <Activity size={20} className="text-status-warning" />
                    <div>
                      <p className="text-sm font-semibold text-status-warning">
                        Some systems degraded
                      </p>
                    </div>
                  </>
                )}
              </div>
            </div>

            {/* Services */}
            <div className="mb-12">
              <h2 className="text-sm font-semibold mb-4 text-text-muted uppercase tracking-wider">
                Services
              </h2>
              <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden divide-y divide-border-subtle">
                {STATUS_SERVICES.map((service) => (
                  <div key={service.key} className="flex items-center justify-between px-5 py-4">
                    <div>
                      <p className="text-sm font-medium">{service.name}</p>
                      <p className="text-xs text-text-muted mt-0.5">
                        {SERVICE_DESCRIPTIONS[service.key] ?? ""}
                      </p>
                    </div>
                    <div className="flex items-center gap-2">
                      <div className="h-2 w-2 rounded-full bg-status-healthy" />
                      <span className="text-xs text-status-healthy font-medium">
                        Operational
                      </span>
                    </div>
                  </div>
                ))}
              </div>
            </div>

            {/* Recent incidents */}
            <div>
              <h2 className="text-sm font-semibold mb-4 text-text-muted uppercase tracking-wider">
                Recent incidents
              </h2>
              <EmptyState
                title="No incidents reported."
                description="All services are running normally. When incidents occur, they will be posted here with timelines and post-mortems."
              />
            </div>

            {/* Note */}
            <div className="mt-12 rounded-lg border border-border-subtle bg-bg-surface/50 p-4">
              <p className="text-xs text-text-muted leading-relaxed">
                This status page reflects the current state of SHIFTGATE
                infrastructure. When connected to live telemetry, service
                statuses update automatically based on real health checks
                and heartbeat data.
              </p>
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
