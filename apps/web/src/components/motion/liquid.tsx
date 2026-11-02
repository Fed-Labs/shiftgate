"use client";

import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from "react";

export function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(false);
  useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)");
    const sync = () => setReduced(query.matches);
    sync();
    query.addEventListener?.("change", sync);
    return () => query.removeEventListener?.("change", sync);
  }, []);
  return reduced;
}

export function Reveal({
  children,
  className = "",
  delay = 0,
  variant = "rise",
}: {
  children?: ReactNode;
  className?: string;
  delay?: number;
  variant?: "rise" | "pour" | "settle" | "fade";
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const node = ref.current;
    if (!node) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      node.classList.add("is-in");
      return;
    }
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            entry.target.classList.add("is-in");
            observer.unobserve(entry.target);
          }
        }
      },
      { threshold: 0.12, rootMargin: "0px 0px -8% 0px" },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, []);
  return (
    <div
      ref={ref}
      data-reveal={variant}
      className={`liquid-reveal ${className}`}
      style={{ "--reveal-delay": `${Math.min(delay, 600)}ms` } as CSSProperties}
    >
      {children}
    </div>
  );
}

export function PourLine({
  children,
  delay = 0,
  className = "",
}: {
  children: ReactNode;
  delay?: number;
  className?: string;
}) {
  return (
    <span className={`pour-mask ${className}`} style={{ "--reveal-delay": `${delay}ms` } as CSSProperties}>
      <span className="pour-line">{children}</span>
    </span>
  );
}

export function MountReady({ children, className = "" }: { children: ReactNode; className?: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const node = ref.current;
    if (!node) return;
    const frame = window.requestAnimationFrame(() => node.classList.add("is-ready"));
    return () => window.cancelAnimationFrame(frame);
  }, []);
  return (
    <div ref={ref} className={`liquid-mount ${className}`}>
      {children}
    </div>
  );
}

export function MercuryProgress() {
  const reduced = usePrefersReducedMotion();
  const fillRef = useRef<HTMLDivElement>(null);
  const dropRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const fill = fillRef.current;
    const drop = dropRef.current;
    if (!fill || !drop) return;
    let target = 0;
    let current = 0;
    let frame = 0;
    let running = false;
    let viewport = document.documentElement.clientWidth;
    const readTarget = () => {
      viewport = document.documentElement.clientWidth;
      const max = document.documentElement.scrollHeight - window.innerHeight;
      target = max > 0 ? Math.min(1, Math.max(0, window.scrollY / max)) : 0;
    };
    const tick = () => {
      current = reduced ? target : current + (target - current) * 0.14;
      if (Math.abs(target - current) < 0.0004) current = target;
      fill.style.transform = `scaleX(${current})`;
      drop.style.transform = `translate3d(${current * (viewport - 7)}px, 0, 0)`;
      drop.style.opacity = current > 0.002 ? "1" : "0";
      if (current === target) {
        // Converged: idle instead of rescheduling a frame forever. The scroll
        // and resize listeners below restart the loop when the target moves.
        running = false;
        return;
      }
      frame = window.requestAnimationFrame(tick);
    };
    const kick = () => {
      readTarget();
      if (!running) {
        running = true;
        frame = window.requestAnimationFrame(tick);
      }
    };
    readTarget();
    current = target;
    kick();
    window.addEventListener("scroll", kick, { passive: true });
    window.addEventListener("resize", kick, { passive: true });
    return () => {
      window.cancelAnimationFrame(frame);
      window.removeEventListener("scroll", kick);
      window.removeEventListener("resize", kick);
    };
  }, [reduced]);
  return (
    <div className="mercury-track" aria-hidden="true">
      <div ref={fillRef} className="mercury-fill" />
      <div ref={dropRef} className="mercury-drop" />
    </div>
  );
}

export function LiquidBlobs() {
  return (
    <div className="blob-field" aria-hidden="true">
      <svg width="0" height="0" className="blob-goo-defs">
        <defs>
          <filter id="liquid-goo">
            <feGaussianBlur in="SourceGraphic" stdDeviation="14" result="blur" />
            <feColorMatrix
              in="blur"
              mode="matrix"
              values="1 0 0 0 0  0 1 0 0 0  0 0 1 0 0  0 0 0 22 -10"
              result="goo"
            />
            <feBlend in="SourceGraphic" in2="goo" />
          </filter>
        </defs>
      </svg>
      <div className="blob-goo">
        <i className="blob blob-a" />
        <i className="blob blob-b" />
        <i className="blob blob-c" />
      </div>
    </div>
  );
}
