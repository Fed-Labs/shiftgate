import type { Metadata } from "next";
import Link from "next/link";
import { Container, Section } from "@/components/ui";
import {
  Rocket,
  Lightbulb,
  Monitor,
  Box,
  Camera,
  ArrowLeftRight,
  Braces,
  Terminal,
  Shield,
  Wrench,
} from "lucide-react";

export const metadata: Metadata = {
  title: "Documentation",
  description: "SHIFTGATE documentation — guides, references, and concepts for moving running computation between machines.",
};

const CATEGORIES = [
  {
    title: "Quickstart",
    slug: "quickstart",
    icon: Rocket,
    description: "Build the agent, verify CRIU support, checkpoint a scoped workload, and restore it.",
  },
  {
    title: "Concepts",
    slug: "concepts",
    icon: Lightbulb,
    description: "Core ideas behind SHIFTGATE — workloads, machines, checkpoints, and state classes.",
  },
  {
    title: "Machines",
    slug: "machines",
    icon: Monitor,
    description: "System requirements, registration, capabilities, heartbeats, and GPU compatibility.",
  },
  {
    title: "Workloads",
    slug: "workloads",
    icon: Box,
    description: "Defining workloads, resource specs, device policies, and the workload lifecycle.",
  },
  {
    title: "Checkpoints",
    slug: "checkpoints",
    icon: Camera,
    description: "Full and incremental checkpoints, manifests, storage, deduplication, and restore boundaries.",
  },
  {
    title: "Migration",
    slug: "migration",
    icon: ArrowLeftRight,
    description: "Cold and live migration, the migration pipeline, compatibility, and failure handling.",
  },
  {
    title: "API Reference",
    slug: "api",
    icon: Braces,
    description: "REST API endpoints, authentication, request and response formats.",
  },
  {
    title: "CLI Reference",
    slug: "cli",
    icon: Terminal,
    description: "Every shiftgate command with syntax, flags, and expected output.",
  },
  {
    title: "Security",
    slug: "security",
    icon: Shield,
    description: "Machine identity, encrypted transport, checkpoint encryption, RBAC, and audit logging.",
  },
  {
    title: "Troubleshooting",
    slug: "troubleshooting",
    icon: Wrench,
    description: "Common issues, diagnostics, and how to get help when something goes wrong.",
  },
];

export default function DocsIndexPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Documentation
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            SHIFTGATE documentation
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Everything you need to move running computation between machines.
            Start with the quickstart or browse by topic below.
          </p>
        </div>

        {/* Quick install */}
        <div className="mb-16 rounded-lg border border-border bg-bg-elevated p-6">
          <p className="text-xs font-mono uppercase tracking-wider text-text-muted mb-3">
            Quick install
          </p>
          <pre className="code-block text-sm">
            <code>{`curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash
shiftgate doctor`}</code>
          </pre>
        </div>

        {/* Category grid */}
        <div className="grid gap-4 sm:grid-cols-2">
          {CATEGORIES.map((cat) => {
            const Icon = cat.icon;
            return (
              <Link
                key={cat.slug}
                href={`/docs/${cat.slug}`}
                className="group rounded-lg border border-border bg-bg-elevated p-5 transition-colors hover:border-border-hover hover:bg-bg-hover"
              >
                <div className="flex items-start gap-4">
                  <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-bg-surface border border-border-subtle">
                    <Icon size={16} className="text-accent" />
                  </div>
                  <div className="min-w-0">
                    <h3 className="text-sm font-semibold mb-1 group-hover:text-accent transition-colors">
                      {cat.title}
                    </h3>
                    <p className="text-sm text-text-secondary leading-relaxed">
                      {cat.description}
                    </p>
                  </div>
                </div>
              </Link>
            );
          })}
        </div>
      </Container>
    </Section>
  );
}
