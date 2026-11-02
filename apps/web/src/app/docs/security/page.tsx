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
            SHIFT is designed around a zero-trust model. Every machine
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
          <h2 className="text-xl font-semibold mb-4">API authentication and sessions</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            The API uses Bearer credentials: opaque session access tokens and
            opaque API keys — no JSON Web Tokens are issued or accepted anywhere
            in the system, and only keyed HMAC digests of tokens and keys are
            ever persisted, never bearer values. API keys are scoped to specific
            operations and can be set to expire. Browser sessions are stricter
            still: the dashboard&apos;s rotating refresh token travels only in
            an httpOnly, Secure, SameSite=Strict cookie scoped to the auth
            paths (<code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">/v1/auth</code>),
            so an XSS cannot read it from script-accessible storage or JSON.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`Authorization: Bearer sg_key_7f3a2b1c...

Broad scopes:
  read             — read organization resources
  operate          — create and operate resources
  admin            — administrative operations

Resource scopes:
  machines    workloads    migrations
  checkpoints compute      scim`}</code>
            </pre>
          </div>
        </div>

        {/* RBAC */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Role-based access control</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            SHIFT supports four built-in roles. Each role grants a specific
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
            Every action in SHIFT is recorded in an append-only audit log.
            This includes API calls, machine registrations, workload lifecycle
            events, and migrations. Audit logs are retained per your
            organization&apos;s retention window — 365 days by default,
            configurable from 0 (keep forever) to 36500 days. The window is
            plan-capped: Free and Pro up to 365 days, Business and Enterprise
            up to 36500. Audit events intentionally omit secrets and checkpoint
            contents.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`{
  "event": "migration.completed",
  "actor": "user_7f3a2b1c",
  "org_id": "org_a1b2c3",
  "workload_id": "wl_k8m2p4q7",
  "source_machine": "mach_7f3a2b1c",
  "source_machine_id": "mach_source",
  "destination_machine_id": "mach_target",
  "duration_ms": 5600,
  "timestamp": "2026-08-20T14:30:05Z",
  "ip": "203.0.113.42"
}`}</code>
            </pre>
          </div>
        </div>

        {/* Account erasure */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Account erasure and privacy</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">DELETE /v1/users/me</code>{" "}
            is the account-erasure mechanism. It irreversibly anonymizes the
            calling user&apos;s personal data — email, display name, password
            hash, session tokens, and audit IP/user-agent metadata — and revokes
            every session. The user row itself survives as a pseudonymous stub
            so audit trails, organization membership, and billing history keep
            valid references. The response is a counts-only report (
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">{"{user_id, email_anonymized, sessions_removed, audit_ips_scrubbed}"}</code>
            ), never the destroyed data.
          </p>
          <p className="text-sm text-text-secondary leading-relaxed">
            Accounts deactivated through SCIM reach the same end state through
            the retention sweep&apos;s anonymization pass after a 30-day grace
            window: SCIM delete revokes sessions immediately and keeps the row
            for audit integrity, and the personal data does not survive the
            grace period.
          </p>
        </div>

        {/* Key rotation */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Key rotation</h2>
          <p className="text-sm text-text-secondary leading-relaxed">
            Key management covers what is implemented today. API keys support
            overlapping rotation: create a new key, update your integrations,
            then revoke the old key. Workload data keys can be rotated, which
            versions the keyring and re-keys the workload&apos;s subsequent
            checkpoints. Machine identity keys are generated once per agent
            state directory; identity-key rotation and organization-wide
            master-key rotation are not implemented today — the keyring is
            versioned so they can be added later.
          </p>
        </div>
      </Container>
    </Section>
  );
}
