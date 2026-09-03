"use client";

import { cn } from "@/lib/utils";

/* ------------------------------------------------------------------ */
/*  StateObject — a running computation rendered as layered structure  */
/* ------------------------------------------------------------------ */

export interface StateLayers {
  process?: number;   // 0..1 activity
  memory?: number;
  filesystem?: number;
  network?: number;
  device?: number;
  environment?: number;
}

const LAYER_KEYS: Array<keyof StateLayers> = [
  "process", "memory", "filesystem", "network", "device", "environment",
];

const LAYER_LABELS: Record<keyof StateLayers, string> = {
  process: "PROC",
  memory: "MEM",
  filesystem: "FS",
  network: "NET",
  device: "DEV",
  environment: "ENV",
};

export function StateObject({
  layers,
  active = true,
  size = "md",
  className,
}: {
  layers: StateLayers;
  active?: boolean;
  size?: "sm" | "md" | "lg";
  className?: string;
}) {
  const heights = { sm: "h-16", md: "h-24", lg: "h-36" };
  return (
    <div className={cn("relative select-none", heights[size], className)}>
      {/* Outer frame — sharp corners, hairline */}
      <div className="absolute inset-0 border border-border-strong" />
      {/* Corner ticks */}
      <CornerTicks />
      {/* Internal layers */}
      <div className="absolute inset-[6px] flex flex-col gap-[3px]">
        {LAYER_KEYS.map((key) => {
          const activity = layers[key] ?? 0;
          return (
            <div key={key} className="relative flex-1 flex items-center">
              {/* activity bar */}
              <div
                className={cn("h-[2px] transition-all duration-700", active ? "bg-accent" : "bg-border-strong")}
                style={{ width: `${8 + activity * 88}%`, opacity: active ? 0.35 + activity * 0.65 : 0.3 }}
              />
              <span className="absolute right-0 tlabel" style={{ fontSize: 8 }}>
                {LAYER_LABELS[key]}
              </span>
            </div>
          );
        })}
      </div>
      {/* live core */}
      {active && (
        <div className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 w-1 h-1 bg-accent animate-breathe" />
      )}
    </div>
  );
}

function CornerTicks() {
  const t = "absolute w-[5px] h-[5px] border-accent";
  return (
    <>
      <span className={cn(t, "top-[-1px] left-[-1px] border-t border-l")} />
      <span className={cn(t, "top-[-1px] right-[-1px] border-t border-r")} />
      <span className={cn(t, "bottom-[-1px] left-[-1px] border-b border-l")} />
      <span className={cn(t, "bottom-[-1px] right-[-1px] border-b border-r")} />
    </>
  );
}

/* ------------------------------------------------------------------ */
/*  MachineView — a machine as an enclosure holding state              */
/* ------------------------------------------------------------------ */

export function MachineView({
  name,
  specs,
  status,
  children,
  active = false,
  className,
}: {
  name: string;
  specs: string[];
  status: "online" | "offline" | "idle";
  children?: React.ReactNode;
  active?: boolean;
  className?: string;
}) {
  return (
    <div className={cn("relative", className)}>
      {/* Enclosure */}
      <div
        className={cn(
          "relative border transition-colors duration-500 bg-bg-elevated",
          active ? "border-accent/40" : "border-border"
        )}
      >
        {/* Header strip */}
        <div className="flex items-center justify-between px-3 py-2 border-b border-border-subtle">
          <div className="flex items-center gap-2">
            <span
              className={cn(
                "w-[6px] h-[6px]",
                status === "online" ? "bg-status-healthy animate-pulse-dot" : status === "idle" ? "bg-status-warning" : "bg-status-offline"
              )}
            />
            <span className="font-mono text-[11px] tracking-[0.12em] text-text">{name}</span>
          </div>
          <span className="tlabel">{status}</span>
        </div>

        {/* Body */}
        <div className="relative p-4 min-h-[120px] flex items-center justify-center overflow-hidden">
          {/* scanline when active */}
          {active && (
            <div className="absolute inset-x-0 h-8 bg-gradient-to-b from-transparent via-accent/5 to-transparent animate-scan pointer-events-none" />
          )}
          {children}
        </div>

        {/* Spec footer */}
        <div className="px-3 py-2 border-t border-border-subtle flex flex-wrap gap-x-4 gap-y-1">
          {specs.map((s) => (
            <span key={s} className="font-mono text-[10px] text-text-muted">{s}</span>
          ))}
        </div>
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  ResourceReadout — instrumentation-style metric                     */
/* ------------------------------------------------------------------ */

export function ResourceReadout({
  label,
  value,
  unit,
  fraction,
  accent = false,
}: {
  label: string;
  value: string;
  unit?: string;
  fraction?: number; // 0..1 bar fill
  accent?: boolean;
}) {
  return (
    <div className="py-2">
      <div className="flex items-baseline justify-between mb-1.5">
        <span className="tlabel">{label}</span>
        <span className="readout text-sm">
          {value}
          {unit && <span className="text-text-muted text-[11px] ml-1">{unit}</span>}
        </span>
      </div>
      {fraction !== undefined && (
        <div className="h-[2px] bg-border-subtle">
          <div
            className={cn("h-full transition-all duration-700", accent ? "bg-accent" : "bg-text-muted")}
            style={{ width: `${Math.min(100, Math.max(0, fraction * 100))}%` }}
          />
        </div>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  MigrationPath — state travelling across a line                     */
/* ------------------------------------------------------------------ */

export function MigrationPath({
  from,
  to,
  progress, // 0..1
  active = false,
  vertical = false,
  className,
}: {
  from: string;
  to: string;
  progress: number;
  active?: boolean;
  vertical?: boolean;
  className?: string;
}) {
  return (
    <div className={cn("relative", vertical ? "flex flex-col items-center" : "flex items-center", className)}>
      <span className="font-mono text-[10px] text-text-muted tracking-[0.12em]">{from}</span>
      <div className={cn("relative flex-1 mx-3", vertical ? "w-px self-stretch my-2" : "h-px")}>
        <div className={cn("absolute inset-0", vertical ? "w-px bg-border" : "h-px bg-border")} />
        {/* travelling state */}
        <div
          className={cn("absolute", vertical ? "left-[-2px] w-[5px] h-[5px]" : "top-[-2px] w-[5px] h-[5px]", active ? "bg-accent" : "bg-text-muted")}
          style={vertical ? { top: `${progress * 100}%` } : { left: `${progress * 100}%` }}
        />
      </div>
      <span className="font-mono text-[10px] text-text-muted tracking-[0.12em]">{to}</span>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  StateTimeline — checkpoints as moments on a line                   */
/* ------------------------------------------------------------------ */

export interface TimelinePoint {
  id: string;
  time: string;
  label: string;
  branch?: string;
}

export function StateTimeline({
  points,
  selected,
  onSelect,
  className,
}: {
  points: TimelinePoint[];
  selected?: string;
  onSelect?: (id: string) => void;
  className?: string;
}) {
  return (
    <div className={cn("relative", className)}>
      <div className="absolute left-[3px] top-1 bottom-1 w-px bg-border" />
      <div className="flex flex-col gap-4">
        {points.map((p) => (
          <button
            key={p.id}
            onClick={() => onSelect?.(p.id)}
            className="relative flex items-center gap-3 text-left group cursor-pointer pl-0"
          >
            <span
              className={cn(
                "relative z-10 w-[7px] h-[7px] shrink-0 transition-colors",
                selected === p.id ? "bg-accent" : "bg-border-strong group-hover:bg-text-muted"
              )}
            />
            <span className="font-mono text-[11px] text-text-muted tabular-nums">{p.time}</span>
            <span className={cn("font-mono text-[11px]", selected === p.id ? "text-accent" : "text-text-secondary")}>
              {p.label}
            </span>
            {p.branch && (
              <span className="font-mono text-[10px] text-accent-dim">↳ {p.branch}</span>
            )}
          </button>
        ))}
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ */
/*  SystemStatus — service line                                        */
/* ------------------------------------------------------------------ */

export function SystemStatus({
  name,
  state,
  detail,
}: {
  name: string;
  state: "operational" | "degraded" | "down";
  detail?: string;
}) {
  return (
    <div className="flex items-center justify-between py-2.5 border-b border-border-subtle last:border-0">
      <div className="flex items-center gap-3">
        <span
          className={cn(
            "w-[6px] h-[6px]",
            state === "operational" ? "bg-status-healthy" : state === "degraded" ? "bg-status-warning" : "bg-status-error"
          )}
        />
        <span className="text-sm text-text">{name}</span>
      </div>
      <div className="flex items-center gap-4">
        {detail && <span className="font-mono text-[10px] text-text-muted">{detail}</span>}
        <span className="tlabel">{state}</span>
      </div>
    </div>
  );
}
