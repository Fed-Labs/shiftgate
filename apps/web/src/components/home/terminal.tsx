"use client";

import { useEffect, useRef, useState, type CSSProperties } from "react";
import { usePrefersReducedMotion } from "@/components/motion/liquid";

type LineKind = "cmd" | "out" | "ok" | "check" | "dim";

interface TermLine {
  kind: LineKind;
  text: string;
}

function renderLine(line: TermLine, typed: string, caret: boolean) {
  if (line.kind === "cmd") {
    return (
      <>
        <span className="term-green">$</span> {typed}
        {caret ? <span className="term-caret" aria-hidden="true" /> : null}
      </>
    );
  }
  return (
    <>
      {line.kind === "ok" ? <span className="term-green">OK</span> : null}
      {line.kind === "check" ? <span className="term-amber">CHECK</span> : null}
      {line.kind === "ok" || line.kind === "check" ? " " : null}
      {line.text}
    </>
  );
}

export function TerminalFlow({
  lines,
  label,
  caption,
  className = "",
}: {
  lines: TermLine[];
  label: string;
  caption: string;
  className?: string;
}) {
  const reduced = usePrefersReducedMotion();
  const rootRef = useRef<HTMLDivElement>(null);
  const [cursor, setCursor] = useState(0);
  const [typed, setTyped] = useState("");
  const started = useRef(false);

  useEffect(() => {
    if (reduced) return;
    started.current = false;
    const root = rootRef.current;
    if (!root) return;
    const timers: number[] = [];
    const later = (fn: () => void, ms: number) => {
      timers.push(window.setTimeout(fn, ms));
    };
    const playFrom = (index: number) => {
      if (index >= lines.length) return;
      const line = lines[index];
      if (line.kind === "cmd") {
        let at = 0;
        const step = () => {
          at += 1;
          setTyped(line.text.slice(0, at));
          if (at < line.text.length) {
            later(step, 16 + Math.round(22 * Math.random()));
          } else {
            later(() => {
              setTyped("");
              setCursor(index + 1);
              later(() => playFrom(index + 1), 300);
            }, 420);
          }
        };
        later(step, 120);
      } else {
        setCursor(index + 1);
        later(() => playFrom(index + 1), 320);
      }
    };
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting && !started.current) {
            started.current = true;
            setCursor(0);
            setTyped("");
            later(() => playFrom(0), 360);
            observer.disconnect();
          }
        }
      },
      { threshold: 0.3 },
    );
    observer.observe(root);
    return () => {
      observer.disconnect();
      timers.forEach((t) => window.clearTimeout(t));
    };
  }, [reduced, lines]);

  const active = !reduced && cursor < lines.length ? lines[cursor] : null;
  const typing = Boolean(active && active.kind === "cmd");
  const visible = reduced ? lines : lines.slice(0, cursor + (typing ? 1 : 0));
  const transcript = lines.map((l) => (l.kind === "cmd" ? `$ ${l.text}` : l.text)).join(". ");

  return (
    <div ref={rootRef} className={`term-shell ${className}`}>
      <div className="term-bar">
        <span>{label}</span>
        <span className="term-bar-dim">{caption}</span>
      </div>
      <div
        className="term-body"
        role="img"
        aria-label={transcript}
        style={{ "--term-lines": lines.length } as CSSProperties}
      >
        {visible.map((line, index) => (
          <div key={index} className="term-line-wrap">
            <div className={`term-line term-${line.kind}`}>
              {/* Only the line being typed shows partial text; every other cmd
                  line — finished or queued, including under reduced motion —
                  renders its full command. */}
              {renderLine(
                line,
                typing && index === cursor ? typed : line.text,
                Boolean(typing && index === cursor)
              )}
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}

export type { TermLine, LineKind };
