"use client";

import Link from "next/link";
import type { CSSProperties } from "react";
import { LiquidBlobs, MountReady, PourLine, Reveal } from "@/components/motion/liquid";
import { TerminalFlow, type TermLine } from "@/components/home/terminal";

const HERO_LINES: TermLine[] = [
  { kind: "cmd", text: "shiftgate doctor" },
  { kind: "ok", text: "checkpoint capable — kernel probe passed" },
  { kind: "ok", text: "state store encrypted" },
  { kind: "ok", text: "peer channel mTLS" },
  { kind: "cmd", text: "shiftgate workload create runner --path \"$PWD\" -- python3 -m http.server 8080" },
  { kind: "cmd", text: "shiftgate workload start runner" },
  { kind: "cmd", text: "shiftgate checkpoint create runner --leave-running" },
  { kind: "out", text: "warm state captured · process resumed · 959 ms" },
  { kind: "cmd", text: "shiftgate clone CHECKPOINT_ID --count 10 --prefix ci" },
  { kind: "out", text: "one warm checkpoint · ten independent workers · no cold start" },
  { kind: "cmd", text: "shiftgate migrate runner --to ci-2 --mode live --pre-copy-passes 4" },
  { kind: "out", text: "pre-copy · final freeze · restore · health check · commit" },
  { kind: "cmd", text: "shiftgate migrate runner --to https://127.0.0.1:1" },
  { kind: "out", text: "move refused · rolled back · source still running" },
];

export function LandingHero() {
  return (
    <section className="landing-hero">
      <div className="hero-grid" aria-hidden="true" />
      <LiquidBlobs />
      <div className="hero-inner shell">
        <MountReady>
          <div className="hero-eyebrow hero-step" style={{ "--reveal-delay": "80ms" } as CSSProperties}>
            <span>SHIFT / CONTINUITY INFRASTRUCTURE</span>
          </div>
          <div className="hero-copy">
            <h1>
              <PourLine delay={120}>CI without cold starts,</PourLine>
              <PourLine delay={260}>
                <em>on hardware you own.</em>
              </PourLine>
            </h1>
            <p className="hero-lede hero-step" style={{ "--reveal-delay": "520ms" } as CSSProperties}>
              SHIFT captures a warmed runner state with CRIU — process tree, memory, open
              files — and turns it into burst capacity: N independent workers on your own
              machines, or one measured move to another host. No re-warm, no rebuild.
            </p>
            <p className="hero-promise hero-step" style={{ "--reveal-delay": "640ms" } as CSSProperties}>
              <span className="status-dot" />
              STATE MOVES — AND A FAILED MOVE NEVER DESTROYS THE SOURCE.
            </p>
            <div className="hero-actions hero-step" style={{ "--reveal-delay": "760ms" } as CSSProperties}>
              <Link href="/download" className="button button-primary">
                Install SHIFT <span aria-hidden="true">↗</span>
              </Link>
              <Link href="/docs/ci-burst" className="button button-quiet">
                Run the CI-burst proof <span aria-hidden="true">→</span>
              </Link>
            </div>
          </div>
          <div className="hero-terminal hero-step" style={{ "--reveal-delay": "900ms" } as CSSProperties}>
            <TerminalFlow
              lines={HERO_LINES}
              label="shiftgate — real command surface"
              caption="sequence from the CI-burst proof · output abbreviated"
            />
          </div>
        </MountReady>
      </div>
    </section>
  );
}

export function ProofStrip() {
  const steps = [
    ["01", "CAPTURE", "CRIU freezes a coherent process image — memory, descriptors, device state."],
    ["02", "PACKAGE", "State compresses, encrypts, and splits into content-addressed chunks."],
    ["03", "MOVE", "Chunks travel over an mTLS peer channel — only what the destination misses."],
    ["04", "COMMIT", "Restore, health-check, then commit. A failed move leaves the source intact."],
  ];
  return (
    <section className="proof-strip">
      <div className="shell proof-grid">
        {steps.map(([number, title, body], index) => (
          <Reveal key={number} delay={index * 70} variant="settle" className="proof-reveal">
            <div className="proof-step">
              <span className="proof-number">{number}</span>
              <div>
                <strong>{title}</strong>
                <p>{body}</p>
              </div>
            </div>
          </Reveal>
        ))}
      </div>
    </section>
  );
}
