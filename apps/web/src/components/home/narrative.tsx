"use client";

import { motion, useScroll, useTransform, type MotionValue } from "framer-motion";
import { useEffect, useRef } from "react";
import { StateObject } from "@/components/state";

const BEATS = [
  {
    label: "01 / START",
    title: "A computation begins.",
    body: "A process runs. It accumulates memory, open files, sockets, device handles.",
    layers: { process: 0.3, memory: 0.2, filesystem: 0.1, network: 0.1, device: 0.1, environment: 0.1 },
  },
  {
    label: "02 / ACCUMULATE",
    title: "It becomes large.",
    body: "Hours of state. Gigabytes of memory. The machine it started on becomes a constraint.",
    layers: { process: 0.6, memory: 0.7, filesystem: 0.5, network: 0.4, device: 0.5, environment: 0.2 },
  },
  {
    label: "03 / FREEZE",
    title: "SHIFT freezes the state.",
    body: "A checkpoint captures the running process — memory pages, file descriptors, device state — as an encrypted, content-addressed object.",
    layers: { process: 0.6, memory: 0.7, filesystem: 0.5, network: 0.4, device: 0.5, environment: 0.2 },
  },
  {
    label: "04 / SEPARATE",
    title: "The state separates from the machine.",
    body: "It is no longer bound to hardware. It is a portable object with weight, structure, and identity.",
    layers: { process: 0.6, memory: 0.7, filesystem: 0.5, network: 0.4, device: 0.5, environment: 0.2 },
  },
  {
    label: "05 / TRAVEL",
    title: "It travels.",
    body: "Deduplicated, encrypted chunks move over a mutually-authenticated channel. Only what is missing is sent.",
    layers: { process: 0.6, memory: 0.7, filesystem: 0.5, network: 0.8, device: 0.5, environment: 0.2 },
  },
  {
    label: "06 / RECONSTRUCT",
    title: "Another machine reconstructs it.",
    body: "The destination restores the process. Connections reconnect. The workload resumes from the exact state.",
    layers: { process: 0.8, memory: 0.7, filesystem: 0.5, network: 0.4, device: 0.6, environment: 0.2 },
  },
  {
    label: "07 / CONTINUE",
    title: "The machine is no longer the boundary.",
    body: "Computation moves to where it is needed. Nothing restarts. Nothing is lost.",
    layers: { process: 0.9, memory: 0.8, filesystem: 0.6, network: 0.5, device: 0.7, environment: 0.3 },
  },
];

export function Narrative() {
  return (
    <section className="relative border-b border-border-subtle">
      <div className="max-w-6xl mx-auto px-5 md:px-10 py-24">
        <div className="flex items-baseline justify-between mb-16">
          <span className="tlabel">the migration, as a timeline</span>
          <span className="font-mono text-[10px] text-text-faint">scroll</span>
        </div>

        <div className="grid md:grid-cols-[320px_1fr] gap-12">
          {/* Sticky state object that evolves */}
          <div className="hidden md:block">
            <div className="sticky top-24">
              <EvolvingState />
            </div>
          </div>

          {/* Beats */}
          <div className="flex flex-col">
            {BEATS.map((beat) => (
              <motion.div
                key={beat.label}
                initial={{ opacity: 0, y: 24 }}
                whileInView={{ opacity: 1, y: 0 }}
                viewport={{ once: true, margin: "-100px" }}
                transition={{ duration: 0.6 }}
                className="py-10 border-b border-border-subtle last:border-0"
              >
                <div className="font-mono text-[10px] text-accent tracking-[0.2em] mb-3">{beat.label}</div>
                <h3 className="font-display text-2xl md:text-3xl font-medium text-text mb-3">{beat.title}</h3>
                <p className="text-text-secondary text-sm md:text-base leading-relaxed max-w-md">{beat.body}</p>
              </motion.div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}

/* The state object grows as you scroll through the narrative */
function EvolvingState() {
  const ref = useRef(null);
  const { scrollYProgress } = useScroll({ target: ref, offset: ["start end", "end start"] });

  // map scroll to a "mass" value
  const mass = useTransform(scrollYProgress, [0, 1], [0.15, 0.9]);

  return (
    <div ref={ref} className="space-y-4">
      <motion.div
        style={{ opacity: useTransform(scrollYProgress, [0, 0.1], [0.4, 1]) }}
      >
        <ScrollState />
      </motion.div>
      <div className="flex items-baseline justify-between">
        <span className="tlabel">state mass</span>
        <motion.span className="readout text-sm" >
          <MassReadout mass={mass} />
        </motion.span>
      </div>
    </div>
  );
}

function MassReadout({ mass }: { mass: MotionValue<number> }) {
  // render a live GB readout from the motion value
  return <MotionGB value={mass} />;
}

function MotionGB({ value }: { value: MotionValue<number> }) {
  const ref = useRef<HTMLSpanElement>(null);
  // Subscribe in an effect: this both satisfies the no-refs-during-render
  // rule and unsubscribes on unmount, which the old .on() call leaked.
  useEffect(() => value.on("change", (v) => {
    if (ref.current) ref.current.textContent = (v * 24).toFixed(2) + " GB";
  }), [value]);
  // The initial text derives from the value's current state rather than a
  // hardcoded "3.60 GB".
  return <span ref={ref}>{(value.get() * 24).toFixed(2)} GB</span>;
}

function ScrollState() {
  return (
    <StateObject
      layers={{ process: 0.7, memory: 0.7, filesystem: 0.5, network: 0.4, device: 0.5, environment: 0.2 }}
      active
      size="lg"
    />
  );
}
