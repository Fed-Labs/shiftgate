import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section } from "@/components/ui";
import {
  Server,
  Globe,
  HeadphonesIcon,
  FileCheck,
  KeyRound,
  ClipboardList,
} from "lucide-react";
import { EnterpriseContactForm } from "./contact-form";

export const metadata: Metadata = {
  title: "Enterprise",
  description:
    "SHIFT for organizations that need self-hosted deployment, deployment control, and contract-based support terms.",
};

const FEATURES = [
  {
    icon: Server,
    title: "Self-hosted",
    description:
      "Deploy the full SHIFT control plane in your own infrastructure. Your data never leaves your network. Self-hosting is also how you keep checkpoint storage and audit data inside a region you choose.",
  },
  {
    icon: Globe,
    title: "Deployment location",
    description:
      "Self-hosted deployments keep all data wherever you run them. The hosted service is single-region today — there is no per-customer region pinning on the hosted platform. For residency requirements, deploy self-hosted or talk to us.",
  },
  {
    icon: HeadphonesIcon,
    title: "Contract-based support",
    description:
      "Direct access to the SHIFT engineering team under a support agreement. Response times and channels are defined in your contract — there is no published SLA today.",
  },
  {
    icon: FileCheck,
    title: "Agreement terms",
    description:
      "Availability and support terms are defined per agreement. We do not sell an off-the-shelf SLA; if your procurement needs one, we negotiate it with you.",
  },
  {
    icon: KeyRound,
    title: "SSO integration",
    description:
      "OIDC single sign-on (authorization-code flow with PKCE) and SCIM user provisioning. Each deployment federates to one OIDC issuer; per-customer identity providers are a self-hosted capability. SCIM covers the Users resource only.",
  },
  {
    icon: ClipboardList,
    title: "Audit controls",
    description:
      "Audit events are available through the audit API with configurable retention per organization (default 365 days). Export runs through your deployment's logging pipeline — there is no built-in SIEM connector today.",
  },
];

export default function EnterprisePage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14">
        <Section>
          <Container>
            {/* Header */}
            <div className="mb-16 max-w-2xl">
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
                Enterprise
              </p>
              <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
                For organizations that need control
              </h1>
              <p className="text-text-secondary text-lg leading-relaxed">
                SHIFT Enterprise is for organizations that need to run the
                full platform in their own infrastructure, with deployment
                control and support terms defined by contract. Everything on
                this page describes what the product does today; nothing is
                promised that we do not implement.
              </p>
            </div>

            {/* Features */}
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3 mb-20">
              {FEATURES.map((feature) => {
                const Icon = feature.icon;
                return (
                  <div
                    key={feature.title}
                    className="rounded-lg border border-border bg-bg-elevated p-5"
                  >
                    <Icon size={18} className="text-accent mb-3" />
                    <h3 className="text-sm font-semibold mb-2">{feature.title}</h3>
                    <p className="text-sm text-text-secondary leading-relaxed">
                      {feature.description}
                    </p>
                  </div>
                );
              })}
            </div>

            {/* Contact form */}
            <div className="max-w-lg">
              <h2 className="text-xl font-semibold mb-2">Contact us</h2>
              <p className="text-sm text-text-secondary mb-6">
                Tell us about your requirements and we&apos;ll get back to you.
              </p>
              <EnterpriseContactForm />
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
