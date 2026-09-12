"use client";

import { useState } from "react";
import { motion, AnimatePresence } from "framer-motion";
import { StateObject, MachineView } from "@/components/state";

export function Hero() {
  return (
    <section className="relative min-h-screen flex flex-col justify-center overflow-hidden border-b border-border-subtle">
      {/* faint measurement grid */}
      <div
        className="absolute inset-0 opacity-[0.04] pointer-events-none"
        style={{
          backgroundImage:
            "linear-gradient(rgba(233,231,226,0.5) 1px, transparent 1px), linear-gradient(90deg, rgba(233,231,226,0.5) 1px, transparent 1px)",
          backgroundSize: "72px 72px",
        }}
      />

      <div className="relative z-10 max-w-6xl mx-auto w-full px-5 md:px-10 py-24">
        {/* Top line */}
        <div className="flex items-baseline justify-between mb-16">
          <span className="font-mono text-[11px] tracking-[0.2em] text-text">SHIFT</span>
          <span className="tlabel hidden md:block">computation mobility system</span>
        </div>

        {/* Statement */}
        <h1 className="font-display text-[13vw] md:text-[7.5vw] leading-[0.95] font-semibold tracking-tight text-text mb-4">
          MOVE
          <br />
          COMPUTATION
        </h1>

        <div className="grid md:grid-cols-[1fr_auto] gap-8 items-end mb-16">
          <p className="text-text-secondary text-base md:text-lg max-w-md leading-relaxed">
            Running software has a location.
            <br />
            <span className="text-text">SHIFT makes that location movable.</span>
          </p>
          <div className="font-mono text-[11px] text-text-muted text-right leading-relaxed">
            <div>STATE <span className="text-accent">8.31 GB</span></div>
            <div>TRANSFER <span className="text-text">492 MB/s</span></div>
            <div>DOWNTIME <span className="text-text">1.82 s</span></div>
          </div>
        </div>

        <MigrationDemo />
      </div>

      {/* bottom edge */}
      <div className="absolute bottom-0 left-0 right-0 flex items-center justify-between px-5 md:px-10 py-3 border-t border-border-subtle">
        <span className="tlabel">scroll to observe a migration</span>
        <span className="font-mono text-[10px] text-text-faint">v0.2.0</span>
      </div>
    </section>
  );
}

/* ------------------------------------------------------------------ */
/*  Interactive migration: state leaves one machine, enters another    */
/* ------------------------------------------------------------------ */

function MigrationDemo() {
  const [phase, setPhase] = useState<"source" | "moving" | "dest">("source");

  const run = () => {
    if (phase === "moving") return;
    if (phase === "dest") {
      setPhase("source");
      return;
    }
    setPhase("moving");
    setTimeout(() => setPhase("dest"), 2200);
  };

  const atSource = phase === "source";
  const atDest = phase === "dest";
  const moving = phase === "moving";

  return (
    <div className="relative">
      <div className="grid grid-cols-1 md:grid-cols-[1fr_auto_1fr] gap-6 md:gap-10 items-stretch">
        {/* Source */}
        <MachineView
          name="WORKSTATION"
          specs={["AMD RYZEN 9", "128 GB", "RTX 5090", "UBUNTU 24.04"]}
          status={atSource ? "online" : "idle"}
          active={atSource}
        >
          <AnimatePresence>
            {atSource && (
              <motion.div
                key="state-src"
                initial={{ opacity: 0, scale: 0.9 }}
                animate={{ opacity: 1, scale: 1 }}
                exit={{ opacity: 0, scale: 0.9, y: 24 }}
                transition={{ duration: 0.5 }}
                className="w-full max-w-[220px]"
              >
                <StateObject
                  layers={{ process: 0.73, memory: 0.62, filesystem: 0.4, network: 0.3, device: 0.55, environment: 0.2 }}
                  active
                  size="lg"
                />
                <div className="mt-2 text-center font-mono text-[10px] text-text-muted">neural-render</div>
              </motion.div>
            )}
            {!atSource && !moving && (
              <motion.span key="empty-src" initial={{ opacity: 0 }} animate={{ opacity: 1 }} className="tlabel">
                —
              </motion.span>
            )}
          </AnimatePresence>
        </MachineView>

        {/* Transfer channel */}
        <div className="flex md:flex-col items-center justify-center gap-3 py-2 md:py-0 md:px-2">
          <button
            onClick={run}
            className="group relative cursor-pointer"
            aria-label="Move computation"
          >
            <span className="block font-mono text-[11px] tracking-[0.2em] text-text-muted group-hover:text-accent transition-colors">
              {atDest ? "RESET" : moving ? "MOVING" : "MOVE →"}
            </span>
          </button>

          {/* channel line */}
          <div className="relative w-24 md:w-28 h-px bg-border hidden md:block">
            {moving && (
              <motion.div
                className="absolute top-[-2px] w-[5px] h-[5px] bg-accent"
                initial={{ left: "0%" }}
                animate={{ left: "100%" }}
                transition={{ duration: 2, ease: [0.4, 0, 0.2, 1] }}
              />
            )}
          </div>
        </div>

        {/* Destination */}
        <MachineView
          name="LAPTOP"
          specs={["INTEL ULTRA 9", "32 GB", "ARC 8 GB", "FEDORA 41"]}
          status={atDest ? "online" : "offline"}
          active={atDest}
        >
          <AnimatePresence>
            {atDest && (
              <motion.div
                key="state-dest"
                initial={{ opacity: 0, scale: 0.9, y: -24 }}
                animate={{ opacity: 1, scale: 1, y: 0 }}
                transition={{ duration: 0.5 }}
                className="w-full max-w-[220px]"
              >
                <StateObject
                  layers={{ process: 0.73, memory: 0.62, filesystem: 0.4, network: 0.3, device: 0.55, environment: 0.2 }}
                  active
                  size="lg"
                />
                <div className="mt-2 text-center font-mono text-[10px] text-text-muted">neural-render</div>
              </motion.div>
            )}
            {!atDest && !moving && (
              <motion.span key="empty-dest" initial={{ opacity: 0 }} animate={{ opacity: 1 }} className="tlabel">
                —
              </motion.span>
            )}
          </AnimatePresence>
        </MachineView>
      </div>

      {/* Readout strip */}
      <div className="mt-6 grid grid-cols-2 md:grid-cols-4 gap-px bg-border-subtle border border-border-subtle">
        <ReadoutCell label="STATE" value="8.31 GB" />
        <ReadoutCell label="TRANSFER" value="492 MB/s" />
        <ReadoutCell label="DEDUP" value="5.10 GB" />
        <ReadoutCell label="DOWNTIME" value="1.82 s" />
      </div>

      <div className="mt-3 text-center">
        <span className="font-mono text-[10px] text-text-faint">
          {moving
            ? "checkpoint → transfer → restore"
            : atDest
            ? "the computation continued. nothing restarted."
            : "click MOVE. the state travels; the machine is only its location."}
        </span>
      </div>
    </div>
  );
}

function ReadoutCell({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-bg px-4 py-3">
      <div className="tlabel mb-1">{label}</div>
      <div className="readout text-sm">{value}</div>
    </div>
  );
}
