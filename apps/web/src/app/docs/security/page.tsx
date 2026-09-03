import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Security",
  description: "Machine identity, encrypted transport, checkpoint encryption, API authentication, RBAC, audit logging, and key rotation.",
};

export default function SecurityPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Trust model
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Security
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            SHIFTGATE is designed around a zero-trust model. Every machine
            authenticates with a cryptographic identity, all transport is
            encrypted, and checkpoint data is encrypted at rest.
          </p>
        </div>

        {/* Machine identity */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Machine identity and key pairs</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            On first start, each agent generates an Ed25519 identity. The
            private key stays in the agent state directory. Signed checkpoint
            manifests bind state to that identity, while peer traffic is
            authenticated separately with mutually authenticated TLS.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`Machine identity: /var/lib/shift/identity/identity-public.pem

{
  "algorithm": "Ed25519",
  "machine_id": "sha256-of-public-key",
  "private_key_mode": "0600"
}

Private key: /var/lib/shift/identity/identity-key.pem
Public key:  /var/lib/shift/identity/identity-public.pem`}</code>
            </pre>
          </div>
        </div>

        {/* Encrypted transport */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Encrypted transport</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            All communication between machines and the control plane is
            encrypted in transit using TLS 1.3. Machine-to-machine
            communication during migration uses mutually-authenticated TLS
            (mTLS) — both machines verify each other&apos;s identity before any
            data is transferred.
          </p>
          <div className="grid gap-3 sm:grid-cols-3">
            {[
              { label: "Protocol", value: "TLS 1.3" },
              { label: "Machine-to-machine", value: "mTLS" },
              { label: "Certificate rotation", value: "Operator managed" },
            ].map((item) => (
              <div key={item.label} className="rounded-lg border border-border bg-bg-elevated p-4 text-center">
                <p className="text-xs text-text-muted mb-1">{item.label}</p>
                <p className="text-sm font-mono font-semibold">{item.value}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Checkpoint encryption */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Checkpoint encryption</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Checkpoint chunks and local manifests are encrypted at rest using
            AES-256-GCM. Versioned per-workload data keys are wrapped by the
            agent master key. Keys remain on participating agents and are never
            sent to the control plane or object store.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["Algorithm", "AES-256-GCM"],
                  ["Data key", "Versioned per workload"],
                  ["Key protection", "Wrapped by agent master key"],
                  ["Key storage", "Local agent state, mode 0600"],
                ].map(([key, value]) => (
                  <tr key={key}>
                    <td className="px-5 py-3 text-text-muted font-medium w-1/3">{key}</td>
                    <td className="px-5 py-3 text-text-secondary font-mono text-xs">{value}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

        {/* API authentication */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">API authentication</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            The API uses Bearer token authentication. API keys are scoped to
            specific operations and can be set to expire. Each key is tied to
            a role that determines which endpoints it can access.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`Authorization: Bearer sg_key_7f3a2b1c...

Broad scopes:
  read             — read organization resources
  operate          — create and operate resources
  admin            — administrative operations

Resource scopes:
  machines         workloads
  migrations       checkpoints`}</code>
            </pre>
          </div>
        </div>

        {/* RBAC */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Role-based access control</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            SHIFTGATE supports four built-in roles. Each role grants a specific
            set of permissions across the organization&apos;s resources.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border-subtle">
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Role</th>
                  <th className="px-5 py-3 text-left text-xs font-mono text-text-muted uppercase tracking-wider">Permissions</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border-subtle">
                {[
                  ["viewer", "Read-only access to all resources"],
                  ["operator", "Read + create workloads, initiate migrations, manage checkpoints"],
                  ["admin", "Full access including machine management and API key creation"],
                  ["owner", "Full access + organization settings, billing, member management"],
                ].map(([role, perms]) => (
                  <tr key={role}>
                    <td className="px-5 py-3 font-mono text-xs text-accent">{role}</td>
                    <td className="px-5 py-3 text-text-secondary text-xs">{perms}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

        {/* Audit logging */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Audit logging</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Every action in SHIFTGATE is recorded in an append-only audit log.
            This includes API calls, machine registrations, workload lifecycle
            events, migrations, and key rotations. Audit logs are retained for
            90 days on standard plans and indefinitely on enterprise plans.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`{
  "event": "migration.completed",
  "actor": "user_7f3a2b1c",
  "org_id": "org_a1b2c3",
  "workload_id": "wl_k8m2p4q7",
  "source_machine": "mach_7f3a2b1c",
  "target_machine": "mach_9e4d8c2a",
  "duration_ms": 5600,
  "timestamp": "2026-08-20T14:30:05Z",
  "ip": "203.0.113.42"
}`}</code>
            </pre>
          </div>
        </div>

        {/* Key rotation */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Key rotation</h2>
          <p className="text-sm text-text-secondary leading-relaxed">
            SHIFTGATE supports both automatic and manual key rotation. Machine
            identity keys can be rotated without downtime — the new key is
            registered before the old one is revoked. API keys support
            overlapping rotation: create a new key, update your integrations,
            then revoke the old key. Organization KEKs can be rotated to
            re-encrypt all checkpoint data with a new master key.
          </p>
        </div>
      </Container>
    </Section>
  );
}
