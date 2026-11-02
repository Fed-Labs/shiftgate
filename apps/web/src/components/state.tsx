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
          {specs.map((s, i) => (
            <span key={`${s}-${i}`} className="font-mono text-[10px] text-text-muted">{s}</span>
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
/*  Query states — loading, error-with-retry, and status messages      */
/* ------------------------------------------------------------------ */

export function LoadingState({ label }: { label: string }) {
  return (
    <div className="flex items-center gap-3 py-10" role="status" aria-live="polite">
      <span className="inline-block w-3 h-3 border border-accent border-t-transparent rounded-full animate-spin" aria-hidden="true" />
      <span className="font-mono text-[11px] text-text-muted">{label}</span>
    </div>
  );
}

export function ErrorState({
  title,
  message,
  onRetry,
}: {
  title: string;
  message?: string;
  onRetry?: () => void;
}) {
  return (
    <div className="border border-status-error/30 bg-status-error/5 py-14 px-6 text-center" role="alert">
      <p className="font-mono text-[12px] text-status-error mb-2">{title}</p>
      {message && <p className="text-text-muted text-sm mb-6 max-w-lg mx-auto leading-relaxed">{message}</p>}
      {onRetry && (
        <button
          type="button"
          onClick={onRetry}
          className="font-mono text-[11px] tracking-[0.16em] text-accent border border-accent/40 px-5 py-2.5 hover:bg-accent hover:text-bg transition-colors"
        >
          RETRY
        </button>
      )}
    </div>
  );
}

// FEATURE_PLAN_REQUIRED (402) is a plan gate, not an outage: name the feature,
// the plan that unlocks it, and link the upgrade path.
export function PlanRequiredState({ feature, plan }: { feature: string; plan?: string }) {
  return (
    <div className="border border-accent/30 bg-accent/5 py-14 px-6 text-center" role="status">
      <p className="font-mono text-[12px] text-accent mb-2">{feature.toUpperCase()} IS A { (plan || "PAID").toUpperCase() }-PLAN FEATURE</p>
      <p className="text-text-muted text-sm mb-6 max-w-lg mx-auto leading-relaxed">
        Your current plan does not include {feature.toLowerCase()}. Upgrade to unlock it —
        this is a plan limit, not a service problem.
      </p>
      <a
        href="/app/billing"
        className="inline-block font-mono text-[11px] tracking-[0.16em] text-bg bg-accent px-5 py-2.5 hover:bg-accent-dim transition-colors"
      >
        UPGRADE PLAN
      </a>
    </div>
  );
}

// Mutation outcome lines carry their own success/error flag — coloring never
// depends on matching substrings of the message.
export function MessageLine({
  pendingLabel,
  message,
  pending,
  success,
}: {
  pendingLabel: string;
  message: string | null;
  pending: boolean;
  success: boolean;
}) {
  if (!pending && !message) return null;
  return (
    <p
      role="status"
      aria-live="polite"
      className={cn("font-mono text-[11px]", pending ? "text-accent" : success ? "text-status-healthy" : "text-status-error")}
    >
      {pending ? pendingLabel : message}
    </p>
  );
}
