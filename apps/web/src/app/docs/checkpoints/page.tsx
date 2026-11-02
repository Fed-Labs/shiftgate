import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Checkpoints",
  description: "Full and incremental checkpoints, manifest structure, storage, deduplication, and restore behavior in SHIFT.",
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
  "format": "shift-state",
  "format_version": "1.0.0",
  "id": "ckpt_a1b2c3d4",
  "parent_id": "ckpt_z9y8x7w6",
  "kind": "incremental",
  "created_at": "2026-08-20T14:30:00Z",
  "assets": [
    {
      "name": "process-state",
      "chunks": ["..."]
    }
  ],
  "state_inventory": [
    { "name": "memory", "class": "portable", "adapter": "criu", "required": true },
    { "name": "gpu_state", "class": "machine_specific", "adapter": "vendor-checkpoint" }
  ],
  "security": {
    "key_version": 1,
    "manifest_signature": "base64-ed25519-signature",
    "signer_machine_id": "mach_7f3a2b1c",
    "algorithm": "Ed25519+SHA-256"
  },
  "metrics": { "plain_bytes": 0, "stored_bytes": 0, "chunk_count": 0 }
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
              The workload keeps running by default ({`--leave-running`});
              {` --stop-when-done`} is the explicit way to ask for the opposite.
            </p>
            <pre className="text-sm">
              <code className="text-accent">{`shiftgate checkpoint create demo --leave-running
shiftgate checkpoint create demo --parent ckpt_a1b2c3d4
shiftgate checkpoint list          # KIND full|incremental · SIZE · STORED`}</code>
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

        {/* Cloning */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Cloning from checkpoints</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            A clone set derives many independent running workloads from one
            checkpoint on the same machine. Every member keeps its own workload
            identity, its own root directory, and its own process — each a live
            continuation of the checkpointed state. The checkpoint&apos;s chunks
            are read from the store once, and every filesystem copy is a
            reflink clone where the filesystem supports it, so scaling out
            costs N restores, not N full copies.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">shiftgate clone ckpt_a1b2c3d4 --count 50 --prefix worker</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`  Cloned 50 workload(s) from checkpoint ckpt_a1b2c3d4
    clone set:   set_9f3e...
    duration:    18231ms
  worker-1   pid 4231  root /srv/app-clone-9f3e-1
  worker-2   pid 4232  root /srv/app-clone-9f3e-2
  ...
  All-or-nothing: any member failure rolls back every member.`}</code>
            </pre>
          </div>
          <p className="text-sm text-text-muted leading-relaxed mt-4">
            Members restore in parallel (default 4, capped at 16 concurrent
            CRIU restores). A workload that declares TCP ports can only be
            cloned one member at a time, because every member would rebind the
            same host port. A committed set is a fleet of ordinary workloads —
            teardown goes through the workload API.
          </p>
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

        {/* Restore-target policy */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Restore-target policy</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            A checkpoint is untrusted input — its manifest is signed by the
            source, and a compromised source signs hostile manifests
            legitimately — so the destination enforces admission rules before
            anything is materialized (for restore, fork, and clone targets).
            The zero policy fails closed: no target outside a workload root
            already registered on the machine is ever replaced.
          </p>
          <div className="rounded-lg border border-status-warning/20 bg-status-warning/5 p-5 mb-4">
            <p className="text-sm text-status-warning font-medium mb-2">
              Absolute system-path denylist
            </p>
            <p className="text-sm text-text-secondary leading-relaxed">
              Host system trees —{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">/bin /boot /dev /etc /lib /proc /root /run /sbin /sys /usr /var</code>{" "}
              and their lib variants — may never be a checkpoint target,
              whatever any allowlist says. Replacing one as root would be total
              compromise of the destination, so the refusal is absolute and
              touches nothing.
            </p>
          </div>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <p className="text-sm text-text-secondary leading-relaxed mb-4">
              Operators pre-register additional targets through{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">{"<state-dir>/metadata/restore-policy.json"}</code>{" "}
              (a missing file means the fail-closed zero policy). The file must
              be owned by root or the agent user and must not be group- or
              world-writable; anything else is refused rather than trusted.
            </p>
            <pre className="text-sm text-text-secondary whitespace-pre-wrap">
              <code>{`{
  "allowed_root_prefixes": ["/srv/workloads"],
  "allow_root_workload": false,
  "allow_device_nodes": false,
  "allow_setuid": false
}`}</code>
            </pre>
            <div className="my-3 h-px bg-border-subtle" />
            <ul className="text-sm text-text-secondary space-y-1.5">
              <li>• <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">allowed_root_prefixes</code> — absolute prefixes (never the filesystem root, never inside a system directory) that checkpoint targets may live under when they are not already a registered workload root.</li>
              <li>• <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">allow_root_workload</code> — without it a manifest declaring UID 0 is refused outright: the restored process would otherwise run the manifest&apos;s command with full privileges.</li>
              <li>• <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">allow_device_nodes</code> / <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">allow_setuid</code> — without them, device nodes in an archive are refused during extraction and setuid/setgid bits are stripped.</li>
            </ul>
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
