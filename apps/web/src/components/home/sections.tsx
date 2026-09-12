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

/* Honest boundaries */
const LIMITS = [
  "Linux x86_64 is the supported platform.",
  "Live migration is not yet enabled — cold migration is the production path.",
  "GPU migration requires a matching, checkpoint-capable device.",
  "Transparent socket migration only where CRIU TCP repair is permitted.",
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
