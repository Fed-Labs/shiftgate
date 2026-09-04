import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Checkpoints",
  description: "Full and incremental checkpoints, manifest structure, storage, deduplication, and restore behavior in SHIFTGATE.",
};

export default function CheckpointsPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            State capture
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Checkpoints
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            A checkpoint captures the execution state that CRIU and configured
            adapters can represent, plus explicitly scoped filesystem data.
            Checkpoints are the foundation of migration — every migration begins
            with one.
          </p>
        </div>

        {/* Full vs incremental */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Full vs incremental</h2>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="rounded-lg border border-border bg-bg-elevated p-5">
              <div className="flex items-center gap-2 mb-3">
                <div className="h-2 w-2 rounded-full bg-accent" />
                <h3 className="text-sm font-semibold">Full checkpoint</h3>
              </div>
              <p className="text-sm text-text-secondary leading-relaxed mb-4">
                Captures the entire execution state independently. Self-contained
                — can be restored without any other checkpoint. Larger in size
                but simpler to manage.
              </p>
              <div className="text-xs text-text-muted space-y-1">
                <div className="flex justify-between">
                  <span>Size</span>
                  <span className="font-mono">Workload-dependent</span>
                </div>
                <div className="flex justify-between">
                  <span>Time</span>
                  <span className="font-mono">State-dependent</span>
                </div>
                <div className="flex justify-between">
                  <span>Dependencies</span>
                  <span>None</span>
                </div>
              </div>
            </div>
            <div className="rounded-lg border border-border bg-bg-elevated p-5">
              <div className="flex items-center gap-2 mb-3">
                <div className="h-2 w-2 rounded-full bg-status-info" />
                <h3 className="text-sm font-semibold">Incremental checkpoint</h3>
              </div>
              <p className="text-sm text-text-secondary leading-relaxed mb-4">
                Uses CRIU parent images for changed process state and records the
                parent checkpoint for lineage and deduplication. The packaged image
                set remains restorable even when the chain is transferred elsewhere.
              </p>
              <div className="text-xs text-text-muted space-y-1">
                <div className="flex justify-between">
                  <span>Size</span>
                  <span className="font-mono">Changed-state-dependent</span>
                </div>
                <div className="flex justify-between">
                  <span>Time</span>
                  <span className="font-mono">Changed-state-dependent</span>
                </div>
                <div className="flex justify-between">
                  <span>Dependencies</span>
                  <span>Parent checkpoint</span>
                </div>
              </div>
            </div>
          </div>
        </div>

        {/* Manifest structure */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Checkpoint manifest</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Every checkpoint includes a manifest that describes its contents,
            dependencies, and integrity hashes. The manifest is used during
            restore and migration to validate state before loading.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <div className="border-b border-border-subtle px-5 py-2">
              <span className="text-xs font-mono text-text-muted">manifest.json</span>
            </div>
            <div className="p-5">
              <pre className="text-sm text-text-secondary whitespace-pre-wrap">
                <code>{`{
  "checkpoint_id": "ckpt_a1b2c3d4",
  "workload_id": "wl_k8m2p4q7",
  "type": "incremental",
  "parent_id": "ckpt_z9y8x7w6",
  "created_at": "2026-08-20T14:30:00Z",
  "machine_id": "mach_7f3a2b1c",
  "assets": [
    {
      "name": "process-state",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb924...",
      "chunks": ["..."]
    }
  ],
  "state_inventory": [
    { "name": "memory", "class": "portable", "adapter": "criu" },
    { "name": "gpu_state", "class": "machine_specific", "adapter": "vendor-checkpoint" }
  ],
  "security": {
    "algorithm": "Ed25519+SHA-256",
    "key_version": 1
  },
  "encrypted": true
}`}</code>
              </pre>
            </div>
          </div>
        </div>

        {/* Creating checkpoints */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Creating checkpoints</h2>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-sm text-text-secondary mb-4">
              Create a checkpoint of a running workload. Incremental capture is
              explicit: pass the parent checkpoint ID so CRIU can use retained
              parent images. The CLI infers incremental mode from that parent.
            </p>
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate checkpoint create demo --leave-running
shiftgate checkpoint create demo --parent ckpt_a1b2c3d4`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Checkpointing workload wl_k8m2p4q7...
  ✓ Type:          full
  ✓ Kind:          incremental
  ✓ Parent:        ckpt_a1b2c3d4
  ✓ Encrypted:     yes (AES-256-GCM)

The CLI reports this workload's actual bytes and duration.`}</code>
            </pre>
          </div>
        </div>

        {/* Restoring */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Restoring from checkpoints</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Restore a checkpoint through an agent that has its chunks and parent
            chain (for incremental captures). The agent validates integrity and
            materializes ancestors oldest-first before restoring the process.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">shiftgate restore ckpt_a1b2c3d4</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Restoring checkpoint ckpt_a1b2c3d4...
  ✓ Assets validated
  ✓ Filesystem switched
  ✓ Process restored and health-checked
  ✓ Restore committed`}</code>
            </pre>
          </div>
        </div>

        {/* Storage and deduplication */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Storage and deduplication</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Checkpoints are stored as encrypted, content-addressed chunks.
            Repeated chunk content is deduplicated within a workload. Incremental
            process images use retained CRIU parents; filesystem capture remains
            explicitly scoped to the workload root.
          </p>
          <div className="grid gap-3 sm:grid-cols-3">
            {[
              { label: "Default chunk", value: "4 MiB" },
              { label: "Dedup scope", value: "Per workload" },
              { label: "Encryption", value: "AES-256-GCM" },
            ].map((item) => (
              <div key={item.label} className="rounded-lg border border-border bg-bg-elevated p-4 text-center">
                <p className="text-xs text-text-muted mb-1">{item.label}</p>
                <p className="text-lg font-mono font-semibold">{item.value}</p>
              </div>
            ))}
          </div>
        </div>

        {/* Restore boundaries */}
        <div>
          <h2 className="text-xl font-semibold mb-4">Restore boundaries</h2>
          <p className="text-sm text-text-secondary leading-relaxed">
            A successful checkpoint does not make every resource portable.
            Network repair, GPU context state, terminal emulators, and external
            services depend on host capabilities, vendor adapters, and workload
            policy. The manifest&apos;s state inventory reports those boundaries
            instead of silently dropping unsupported state. That inventory is the
            contract for what can be restored on a given destination.
          </p>
        </div>
      </Container>
    </Section>
  );
}
