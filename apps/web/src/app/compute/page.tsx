import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section } from "@/components/ui";
import {
  Cpu,
  MemoryStick,
  Gpu,
  DollarSign,
  Clock,
  Bell,
  AlertTriangle,
} from "lucide-react";
import { ComputeNotifyForm } from "./notify-form";

export const metadata: Metadata = {
  title: "Compute Marketplace",
  description:
    "Find machines by CPU, RAM, GPU, price, and latency. The SHIFTGATE compute marketplace concept — on-demand portable compute.",
};

const FILTER_OPTIONS = [
  { icon: Cpu, label: "CPU", placeholder: "Any" },
  { icon: MemoryStick, label: "RAM", placeholder: "Any" },
  { icon: Gpu, label: "GPU", placeholder: "Any" },
  { icon: DollarSign, label: "Max price", placeholder: "Any" },
  { icon: Clock, label: "Latency", placeholder: "Any" },
];

const CONCEPT_CARDS = [
  {
    title: "How it works",
    body: "Publish your workload requirements — CPU, RAM, GPU, network, storage — and SHIFTGATE finds compatible machines in the marketplace. You set a maximum price, and the system matches you with the best available option based on cost, latency, and availability.",
  },
  {
    title: "For machine owners",
    body: "Register your machines with the marketplace and set your pricing. Earn revenue from idle capacity. SHIFTGATE handles scheduling, migration, and billing — you provide the hardware.",
  },
  {
    title: "For workload owners",
    body: "Access compute on demand without long-term commitments. Move workloads to cheaper or closer machines as conditions change. No vendor lock-in — your workloads are portable by design.",
  },
];

export default function ComputePage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14">
        <Section>
          <Container>
            {/* Banner */}
            <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-4 mb-12 flex items-start gap-3">
              <AlertTriangle size={16} className="text-status-warning shrink-0 mt-0.5" />
              <div>
                <p className="text-sm font-medium text-status-warning mb-1">
                  The compute marketplace is not yet available
                </p>
                <p className="text-sm text-text-secondary">
                  This page shows the product concept. Marketplace functionality
                  is under development. Sign up below to be notified when it
                  launches.
                </p>
              </div>
            </div>

            {/* Header */}
            <div className="mb-12 max-w-2xl">
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
                Concept
              </p>
              <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                Compute marketplace
              </h1>
              <p className="text-text-secondary text-lg leading-relaxed">
                Find machines based on CPU, RAM, GPU, price, latency, and
                availability. Move workloads to the best available compute —
                automatically.
              </p>
            </div>

            {/* Concept filter bar */}
            <div className="mb-12">
              <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-3">
                Filter bar (concept)
              </p>
              <div className="rounded-lg border border-border bg-bg-elevated p-4">
                <div className="grid gap-3 grid-cols-2 sm:grid-cols-3 lg:grid-cols-5">
                  {FILTER_OPTIONS.map((filter) => {
                    const Icon = filter.icon;
                    return (
                      <div
                        key={filter.label}
                        className="rounded-md border border-border-subtle bg-bg-surface px-3 py-2.5"
                      >
                        <div className="flex items-center gap-1.5 mb-1">
                          <Icon size={12} className="text-text-muted" />
                          <span className="text-xs text-text-muted">{filter.label}</span>
                        </div>
                        <span className="text-xs text-text-secondary font-mono">
                          {filter.placeholder}
                        </span>
                      </div>
                    );
                  })}
                </div>
              </div>
            </div>

            {/* Concept machine cards */}
            <div className="mb-16">
              <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-3">
                Machine cards (concept)
              </p>
              <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                {[1, 2, 3].map((i) => (
                  <div
                    key={i}
                    className="rounded-lg border border-border bg-bg-elevated p-5 opacity-50"
                  >
                    <div className="flex items-center justify-between mb-4">
                      <span className="text-sm font-mono text-text-muted">
                        machine_{String(i).padStart(3, "0")}
                      </span>
                      <div className="h-2 w-2 rounded-full bg-status-offline" />
                    </div>
                    <div className="space-y-2 text-xs text-text-muted">
                      <div className="flex justify-between">
                        <span>CPU</span>
                        <span className="font-mono">—</span>
                      </div>
                      <div className="flex justify-between">
                        <span>RAM</span>
                        <span className="font-mono">—</span>
                      </div>
                      <div className="flex justify-between">
                        <span>GPU</span>
                        <span className="font-mono">—</span>
                      </div>
                      <div className="flex justify-between">
                        <span>Price</span>
                        <span className="font-mono">—</span>
                      </div>
                      <div className="flex justify-between">
                        <span>Latency</span>
                        <span className="font-mono">—</span>
                      </div>
                    </div>
                  </div>
                ))}
              </div>
              <p className="text-xs text-text-muted mt-3 text-center">
                No marketplace data available — cards shown as placeholders. No imaginary providers.
              </p>
            </div>

            {/* How it will work */}
            <div className="mb-16">
              <h2 className="text-xl font-semibold mb-6">How it will work</h2>
              <div className="grid gap-4 sm:grid-cols-3">
                {CONCEPT_CARDS.map((card) => (
                  <div
                    key={card.title}
                    className="rounded-lg border border-border bg-bg-elevated p-5"
                  >
                    <h3 className="text-sm font-semibold mb-2">{card.title}</h3>
                    <p className="text-sm text-text-secondary leading-relaxed">
                      {card.body}
                    </p>
                  </div>
                ))}
              </div>
            </div>

            {/* Notify me */}
            <div className="rounded-lg border border-border bg-bg-elevated p-6 max-w-md">
              <div className="flex items-center gap-2 mb-3">
                <Bell size={16} className="text-accent" />
                <h3 className="text-sm font-semibold">Get notified</h3>
              </div>
              <p className="text-sm text-text-secondary mb-4">
                Be the first to know when the compute marketplace launches.
              </p>
              <ComputeNotifyForm />
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
