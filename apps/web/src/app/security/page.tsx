"use client";

import { motion } from "framer-motion";
import {
  Key,
  Lock,
  Shield,
  FileCheck,
  Server,
  Eye,
  UserCheck,
} from "lucide-react";
import { Container, Section, Card, Divider } from "@/components/ui";
import { SiteHeader, SiteFooter } from "@/components/site-header";

const fadeUp = {
  initial: { opacity: 0, y: 12 },
  whileInView: { opacity: 1, y: 0 },
  viewport: { once: true },
  transition: { duration: 0.5, ease: [0.16, 1, 0.3, 1] as const },
};

const securitySections = [
  {
    icon: Key,
    title: "Machine identity",
    content:
      "Every machine running the SHIFTGATE agent generates a unique Ed25519 key pair on first boot. The public key is the machine's identity — used for authentication, key exchange, and checkpoint encryption. Private keys never leave the machine.",
    details: [
      "Ed25519 key pairs generated locally",
      "Public key registered with the control plane",
      "Private key stored in kernel keyring (keyctl)",
      "Key rotation supported without downtime",
    ],
  },
  {
    icon: Lock,
    title: "Encrypted state",
    content:
      "Checkpoint data is encrypted at rest and in transit. Each checkpoint is encrypted with a unique symmetric key derived from the target machine's public key. Even the control plane cannot read checkpoint contents.",
    details: [
      "AES-256-GCM for data encryption",
      "X25519 for key exchange",
      "Unique key per checkpoint",
      "Encrypted at rest — never written plaintext to disk",
    ],
  },
  {
    icon: Shield,
    title: "Authentication",
    content:
      "All API access is authenticated. Session tokens for interactive use, API keys for automation. Tokens are short-lived and scoped. API keys can be restricted to specific machines and operations.",
    details: [
      "JWT session tokens (15-minute expiry)",
      "API keys with optional scope restrictions",
      "Machine-level authentication via agent certificates",
      "OAuth 2.0 support for SSO integration",
    ],
  },
  {
    icon: UserCheck,
    title: "Authorization",
    content:
      "Role-based access control with four roles. Permissions are enforced at the API layer and audited. Default policy is least privilege — new users and keys start with the minimum permissions needed.",
    details: [
      "Owner — full control over the organization",
      "Admin — manage machines and users",
      "Operator — create and manage migrations",
      "Viewer — read-only access to status and logs",
    ],
  },
  {
    icon: FileCheck,
    title: "Checkpoint integrity",
    content:
      "Every checkpoint includes a signed manifest listing all chunks, their SHA-256 hashes, and metadata. The target agent verifies the manifest signature and every chunk hash before restoring. Corrupted or tampered checkpoints are rejected.",
    details: [
      "SHA-256 hash per chunk",
      "Ed25519 manifest signature",
      "Verified on target before restore",
      "Mismatched hashes abort the migration",
    ],
  },
  {
    icon: Server,
    title: "Secure transport",
    content:
      "Agent-to-agent communication uses mutually authenticated TLS 1.3. Both the source and target agents present certificates. The control plane never sees checkpoint data — it only coordinates the transfer.",
    details: [
      "TLS 1.3 with mutual authentication",
      "Certificate pinning between agents",
      "No checkpoint data through the control plane",
      "Forward secrecy via ephemeral key exchange",
    ],
  },
  {
    icon: Eye,
    title: "Audit events",
    content:
      "Every action is logged — authentication attempts, migrations, key rotations, configuration changes. Logs are structured (JSON), signed, and can be exported to your SIEM. Tamper-evident via hash chaining.",
    details: [
      "Structured JSON logs",
      "Hash-chained for tamper detection",
      "Exportable to syslog, CloudWatch, Datadog",
      "Configurable retention policies",
    ],
  },
];

export default function SecurityPage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14">
        {/* Hero */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div
              className="max-w-3xl"
              initial={{ opacity: 0, y: 16 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.6, ease: [0.16, 1, 0.3, 1] as const }}
            >
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Security
              </p>
              <h1 className="text-4xl md:text-5xl font-bold tracking-tight mb-6">
                Encrypted by default.
                <br />
                Verified at every step.
              </h1>
              <p className="text-lg text-text-secondary leading-relaxed max-w-2xl">
                Moving process state between machines means moving sensitive
                data. Every layer of SHIFTGATE is designed around that reality.
              </p>
            </motion.div>
          </Container>
        </Section>

        {/* Security sections */}
        <Section className="border-t border-border-subtle">
          <Container>
            <div className="space-y-8">
              {securitySections.map((section, i) => (
                <motion.div
                  key={section.title}
                  {...fadeUp}
                  transition={{ delay: i * 0.04 }}
                >
                  <div className="grid grid-cols-1 md:grid-cols-12 gap-6">
                    <div className="md:col-span-4">
                      <div className="flex items-center gap-3 mb-3">
                        <div className="w-8 h-8 rounded-md border border-border flex items-center justify-center shrink-0">
                          <section.icon className="w-4 h-4 text-text-secondary" />
                        </div>
                        <h2 className="text-lg font-semibold">{section.title}</h2>
                      </div>
                      <p className="text-sm text-text-secondary leading-relaxed">
                        {section.content}
                      </p>
                    </div>
                    <div className="md:col-span-7 md:col-start-6">
                      <div className="bg-bg rounded-md border border-border-subtle p-4">
                        {section.details.map((detail) => (
                          <div
                            key={detail}
                            className="flex items-start gap-3 py-2 first:pt-0 last:pb-0"
                          >
                            <div className="w-1 h-1 rounded-full bg-accent mt-2 shrink-0" />
                            <span className="text-sm text-text-secondary">
                              {detail}
                            </span>
                          </div>
                        ))}
                      </div>
                    </div>
                  </div>
                  {i < securitySections.length - 1 && (
                    <div className="mt-8">
                      <Divider />
                    </div>
                  )}
                </motion.div>
              ))}
            </div>
          </Container>
        </Section>

        {/* Roles table */}
        <Section className="border-t border-border-subtle">
          <Container>
            <motion.div {...fadeUp}>
              <p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">
                Access control
              </p>
              <h2 className="text-2xl md:text-3xl font-bold tracking-tight mb-8">
                Role permissions.
              </h2>
            </motion.div>

            <motion.div {...fadeUp}>
              <Card className="overflow-x-auto">
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border">
                      <th className="text-left p-4 font-medium text-text-muted text-xs font-mono uppercase tracking-wider">
                        Permission
                      </th>
                      <th className="text-center p-4 font-medium text-text-muted text-xs font-mono uppercase tracking-wider">
                        Owner
                      </th>
                      <th className="text-center p-4 font-medium text-text-muted text-xs font-mono uppercase tracking-wider">
                        Admin
                      </th>
                      <th className="text-center p-4 font-medium text-text-muted text-xs font-mono uppercase tracking-wider">
                        Operator
                      </th>
                      <th className="text-center p-4 font-medium text-text-muted text-xs font-mono uppercase tracking-wider">
                        Viewer
                      </th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border-subtle">
                    {[
                      { perm: "View machines and workloads", roles: [true, true, true, true] },
                      { perm: "Create migrations", roles: [true, true, true, false] },
                      { perm: "Manage machines", roles: [true, true, false, false] },
                      { perm: "Manage users and roles", roles: [true, true, false, false] },
                      { perm: "Manage API keys", roles: [true, true, false, false] },
                      { perm: "View audit logs", roles: [true, true, false, false] },
                      { perm: "Organization settings", roles: [true, false, false, false] },
                      { perm: "Delete organization", roles: [true, false, false, false] },
                    ].map((row) => (
                      <tr key={row.perm}>
                        <td className="p-4 text-text-secondary">{row.perm}</td>
                        {row.roles.map((allowed, i) => (
                          <td key={i} className="p-4 text-center">
                            {allowed ? (
                              <span className="text-accent">✓</span>
                            ) : (
                              <span className="text-text-muted">—</span>
                            )}
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </Card>
            </motion.div>
          </Container>
        </Section>

        {/* Bottom note */}
        <Section className="border-t border-border-subtle">
          <Container narrow>
            <motion.div {...fadeUp}>
              <div className="bg-bg-elevated border border-border rounded-lg p-6">
                <p className="text-sm text-text-secondary leading-relaxed">
                  SHIFTGATE does not claim compliance certifications we
                  don&apos;t have. The system is designed with security best
                  practices — encrypted transport, least privilege, audit
                  logging — but we leave compliance attestation to the
                  organizations deploying it. The code is open source. Audit it
                  yourself.
                </p>
              </div>
            </motion.div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
