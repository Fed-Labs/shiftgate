"use client";

import Link from "next/link";
import { motion } from "framer-motion";

/* What actually moves — dense technical list, not cards */
const MOVES = [
  ["PROCESS", "pid trees, threads, signals, timers"],
  ["MEMORY", "pages, heaps, stacks — byte for byte"],
  ["FILESYSTEM", "open files, descriptors, the workload root"],
  ["NETWORK", "sockets where CRIU permits; reconnect otherwise"],
  ["DEVICE", "GPU contexts on compatible hardware"],
  ["ENVIRONMENT", "variables, working directory, identity"],
];

export function WhatMoves() {
  return (
    <section className="border-b border-border-subtle">
      <div className="max-w-6xl mx-auto px-5 md:px-10 py-24">
        <div className="grid md:grid-cols-[1fr_1.4fr] gap-12">
          <div>
            <span className="tlabel block mb-4">state inventory</span>
            <h2 className="font-display text-3xl md:text-4xl font-medium text-text leading-tight">
              What moves
              <br />
              is the whole process.
            </h2>
            <p className="text-text-secondary text-sm leading-relaxed mt-6 max-w-xs">
              Not a file. Not a container image. The running thing itself —
              captured, transferred, and reconstructed.
            </p>
          </div>

          <div className="divide-y divide-border-subtle border-y border-border-subtle">
            {MOVES.map(([k, v], i) => (
              <motion.div
                key={k}
                initial={{ opacity: 0, x: 12 }}
                whileInView={{ opacity: 1, x: 0 }}
                viewport={{ once: true }}
                transition={{ delay: i * 0.05 }}
                className="grid grid-cols-[110px_1fr] gap-4 py-4 items-baseline"
              >
                <span className="font-mono text-[11px] text-accent tracking-[0.14em]">{k}</span>
                <span className="text-text-secondary text-sm">{v}</span>
              </motion.div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}

/* Beyond moving — the checkpoint is a primitive, not just a transfer step */
const USES = [
  {
    k: "CLONE",
    title: "One warm checkpoint, fifty running workers",
    body: "Derive any number of independent workloads from a single checkpoint on the same machine — each with its own root, its own process, its own identity. Filesystem copies are reflink clone-on-write, so scaling out costs restores, not copies.",
    cmd: "shiftgate clone CKPT --count 50 --prefix worker",
  },
  {
    k: "PARK",
    title: "Park a workload. Resume it later.",
    body: "Freeze a long-running job into an encrypted checkpoint and restore it — on this machine or another — without redoing hours of work. Lazy restore starts the process before its memory is fully loaded; pages stream in on demand.",
    cmd: "shiftgate restore CKPT --lazy",
  },
  {
    k: "FAILOVER",
    title: "A warm standby that acts when the source dies",
    body: "Every checkpoint replicates to a standby agent that watches the source and restores the newest copy there — automatically when the source is confirmed gone, or on your command.",
    cmd: "shiftgate workload failover NAME --to https://standby:8443",
  },
  {
    k: "STORE",
    title: "Encrypted checkpoint storage, metered",
    body: "Checkpoints mirror to platform-hosted, per-organization encrypted storage with real quota accounting — or to your own S3-compatible bucket. Keys never leave the agent; only ciphertext travels.",
    cmd: "shiftgate storage",
  },
];

export function MoreThanMoving() {
  return (
    <section className="border-b border-border-subtle">
      <div className="max-w-6xl mx-auto px-5 md:px-10 py-24">
        <div className="grid md:grid-cols-[1fr_1.4fr] gap-12">
          <div>
            <span className="tlabel block mb-4">beyond moving</span>
            <h2 className="font-display text-3xl md:text-4xl font-medium text-text leading-tight">
              Moving is only
              <br />
              the first verb.
            </h2>
            <p className="text-text-secondary text-sm leading-relaxed mt-6 max-w-xs">
              A checkpoint is a portable object. Once state can be captured,
              it can be multiplied, shelved, replicated, and stored.
            </p>
          </div>

          <div className="divide-y divide-border-subtle border-y border-border-subtle">
            {USES.map((u, i) => (
              <motion.div
                key={u.k}
                initial={{ opacity: 0, y: 12 }}
                whileInView={{ opacity: 1, y: 0 }}
                viewport={{ once: true }}
                transition={{ delay: i * 0.05 }}
                className="py-5"
              >
                <div className="grid grid-cols-[110px_1fr] gap-4">
                  <span className="font-mono text-[11px] text-accent tracking-[0.14em]">{u.k}</span>
                  <div>
                    <h3 className="text-sm font-medium text-text mb-1.5">{u.title}</h3>
                    <p className="text-text-secondary text-sm leading-relaxed mb-3">{u.body}</p>
                    <code className="font-mono text-[11px] text-text-muted bg-bg-surface border border-border-subtle rounded px-2 py-1 inline-block">
                      {u.cmd}
                    </code>
                  </div>
                </div>
              </motion.div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}

/* Honest boundaries */
const LIMITS = [
  "Linux x86_64 is the supported platform.",
  "Live migration is CRIU pre-copy assisted; the final checkpoint still pauses the workload briefly.",
  "GPU migration requires a matching, checkpoint-capable device.",
  "Transparent socket migration only where CRIU TCP repair is permitted.",
  "Automatic failover has no fencing — a partitioned source can return to a second live copy.",
];

export function Boundaries() {
  return (
    <section className="border-b border-border-subtle bg-bg-elevated/40">
      <div className="max-w-6xl mx-auto px-5 md:px-10 py-24">
        <div className="grid md:grid-cols-[1fr_1.4fr] gap-12">
          <div>
            <span className="tlabel block mb-4">explicit boundaries</span>
            <h2 className="font-display text-3xl md:text-4xl font-medium text-text leading-tight">
              We say what it
              <br />
              does not do.
            </h2>
          </div>
          <div className="space-y-0 divide-y divide-border-subtle border-y border-border-subtle">
            {LIMITS.map((l, i) => (
              <div key={i} className="flex gap-4 py-4 items-baseline">
                <span className="font-mono text-[10px] text-text-faint">{"0" + (i + 1)}</span>
                <span className="text-text-secondary text-sm leading-relaxed">{l}</span>
              </div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}

/* Final CTA — quiet */
export function FinalCTA() {
  return (
    <section className="relative overflow-hidden">
      <div className="max-w-6xl mx-auto px-5 md:px-10 py-32">
        <div className="grid md:grid-cols-[1fr_auto] gap-10 items-end">
          <div>
            <h2 className="font-display text-4xl md:text-6xl font-semibold tracking-tight text-text leading-[1.02] mb-6">
              The machine is no
              <br />
              longer the boundary.
            </h2>
            <p className="text-text-secondary max-w-sm leading-relaxed">
              Install the agent. Connect two machines. Move a running workload.
            </p>
          </div>

          <div className="flex flex-col gap-3 items-start md:items-end">
            <Link
              href="/download"
              className="inline-flex items-center gap-3 border border-accent/50 px-6 py-3 font-mono text-[12px] tracking-[0.16em] text-accent hover:bg-accent hover:text-bg transition-colors"
            >
              DOWNLOAD SHIFT
            </Link>
            <Link
              href="/docs/quickstart"
              className="font-mono text-[11px] text-text-muted hover:text-text transition-colors tracking-[0.12em]"
            >
              QUICKSTART →
            </Link>
          </div>
        </div>

        <div className="mt-16 code-block max-w-xl">
          <span className="text-text-muted">$ </span>
          <span className="text-accent">curl -fsSL https://github.com/Fed-Labs/shiftgate/releases/latest/download/install.sh | bash</span>
        </div>
      </div>
    </section>
  );
}
