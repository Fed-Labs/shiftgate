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
    "SHIFTGATE for organizations that need self-hosted deployment, custom data residency, dedicated support, and compliance controls.",
};

const FEATURES = [
  {
    icon: Server,
    title: "Self-hosted",
    description:
      "Deploy the full SHIFTGATE control plane in your own infrastructure. Your data never leaves your network.",
  },
  {
    icon: Globe,
    title: "Custom data residency",
    description:
      "Choose where your data lives. Meet regulatory requirements with region-specific deployments and data isolation.",
  },
  {
    icon: HeadphonesIcon,
    title: "Dedicated support",
    description:
      "Direct access to the SHIFTGATE engineering team. SLA-backed response times for critical issues.",
  },
  {
    icon: FileCheck,
    title: "SLA",
    description:
      "Contractual guarantees on availability, performance, and support response. Custom SLAs for enterprise requirements.",
  },
  {
    icon: KeyRound,
    title: "SSO integration",
    description:
      "Connect your identity provider. OIDC single sign-on and SCIM user provisioning for seamless authentication across your organization.",
  },
  {
    icon: ClipboardList,
    title: "Audit controls",
    description:
      "Extended audit logging, custom retention policies, and integration with your SIEM for compliance monitoring.",
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
                SHIFTGATE Enterprise is for organizations that need to run
                the full platform in their own infrastructure, with custom
                data residency, compliance controls, and dedicated support.
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
