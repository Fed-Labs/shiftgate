import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Storage",
  description: "Encrypted checkpoint storage in SHIFT — self-hosted S3 mirroring and platform-hosted storage with reconciled, quota-enforced usage.",
};

export default function StoragePage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Infrastructure
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Checkpoint storage
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Every checkpoint is encrypted and content-addressed on the agent.
            Off-machine copies come in two flavors: mirror to your own
            S3-compatible bucket, or let the platform host storage for your
            organization with plan quotas enforced.
          </p>
        </div>
        {/* What is stored */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">What gets stored</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            A checkpoint is an archive of the workload root plus CRIU process
            images, split into chunks, compressed, and encrypted with
            per-workload keys before anything leaves the machine. Chunks are
            content-addressed and deduplicated — an incremental checkpoint
            stores only the chunks that changed. The storage backend, whoever
            operates it, only ever receives AEAD ciphertext: workload keys
            never leave agent state, and bucket-level encryption on your side
            is complementary, not a substitute.
          </p>
          <p className="text-sm text-text-muted leading-relaxed">
            Local pruning deletes manifests and checkpoint directories only —
            content-addressed chunks survive for deduplication and restore, and
            mirrored cloud objects are not garbage-collected by local pruning.
            Reclaim chunk-store space with the offline sweeper: see{" "}
            <a href="/docs/operations" className="text-text hover:text-accent transition-colors">Operations</a>.
          </p>
        </div>

        {/* Self-hosted mirroring */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Self-hosted mirroring</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Point the agent at your own S3-compatible bucket (or a local
            backend) and configured checkpoints can mirror there. A
            checkpoint whose mirror fails reports the failure while the local
            copy persists — mirroring never silently degrades to local-only.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`# agent.json — or the SHIFT_S3_* environment variables
{
  "object_store": {
    "backend": "s3",
    "endpoint": "https://s3.example.com",
    "region": "us-east-1",
    "bucket": "shift-checkpoints"
  }
}`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm">
              <code className="text-accent">{`# retry a failed publication
shiftgate checkpoint mirror CHECKPOINT_ID`}</code>
            </pre>
          </div>
        </div>

        {/* Hosted storage */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Platform-hosted storage</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Plans include hosted storage (10 GB free, 120 GB Pro, and 500 GB
            pooled + 100 GB per seat on Business, capped at 2 TB). The control
            plane brokers access to one store it operates — agents fetch
            short-lived, organization-scoped credentials from it and write under
            their own prefix. Opt the agent in with{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">object_store.backend: control-plane</code>;
            the exact lines for your deployment come from{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate storage</code>.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate storage
# status: endpoint, bucket, used vs quota bytes, over-quota flag,
# plus the exact agent configuration lines for hosted mode`}</code>
            </pre>
          </div>
        </div>

        {/* How hosting works */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">How hosting works</h2>
          <div className="grid gap-3 sm:grid-cols-2">
            {[
              { label: "Scoped credentials", detail: "Short-lived STS credentials limited to org/{id}/*; the parent store credential never leaves the control plane, and issuance is audited with the principal only — never a secret." },
              { label: "Reconciled usage", detail: "A background job lists each organization's prefix and sums actual object bytes. Quotas are measured from the bucket's truth, never from agent claims." },
              { label: "Fail-closed quota", detail: "Over the limit, credential issuance stops (STORAGE_QUOTA_EXCEEDED) so new mirrors fail loudly — but local checkpointing on the agent never stops, and exactly-at-quota still issues so restores keep working." },
              { label: "Ciphertext only", detail: "The control plane brokers access but never sees plaintext or workload keys; objects are AEAD ciphertext before they leave the agent." },
            ].map((item) => (
              <div key={item.label} className="rounded-lg border border-border bg-bg-elevated p-4">
                <p className="text-sm font-semibold mb-1">{item.label}</p>
                <p className="text-xs text-text-muted leading-relaxed">{item.detail}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Over quota */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Over quota</h2>
          <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-5">
            <p className="text-sm text-status-warning font-medium mb-2">
              What an over-quota organization sees
            </p>
            <ul className="text-sm text-text-secondary space-y-1.5">
              <li>• New checkpoint creates report &quot;persisted locally but mirror failed&quot;</li>
              <li>• The dashboard storage page shows the over-quota banner with used vs quota bytes</li>
              <li>• Local checkpoints, restores, and migration all keep working</li>
              <li>• Mirroring resumes automatically once usage drops below the limit or the plan is upgraded</li>
            </ul>
          </div>
        </div>
      </Container>
    </Section>
  );
}
