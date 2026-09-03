// Shared UI primitives — the instrument palette from the web dashboard,
// expressed as a small set of building blocks the screens compose.

import React from "react";
import clsx from "clsx";

export function Card({
  className,
  children,
  ...rest
}: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={clsx(
        "bg-bg-surface border border-border rounded-lg",
        className
      )}
      {...rest}
    >
      {children}
    </div>
  );
}

export function SectionLabel({ children }: { children: React.ReactNode }) {
  return <div className="tlabel mb-3">{children}</div>;
}

type BadgeTone = "accent" | "healthy" | "warning" | "error" | "offline" | "neutral" | "info";

const badgeToneClasses: Record<BadgeTone, string> = {
  accent: "text-accent border-accent/30 bg-accent/10",
  healthy: "text-status-healthy border-status-healthy/30 bg-status-healthy/10",
  warning: "text-status-warning border-status-warning/30 bg-status-warning/10",
  error: "text-status-error border-status-error/30 bg-status-error/10",
  offline: "text-text-muted border-border bg-bg",
  neutral: "text-text-secondary border-border bg-bg",
  info: "text-status-info border-status-info/30 bg-status-info/10",
};

export function Badge({
  tone = "neutral",
  children,
  className,
}: {
  tone?: BadgeTone;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1.5 px-2 py-0.5 rounded-full border text-[11px] font-medium leading-4",
        badgeToneClasses[tone],
        className
      )}
    >
      {children}
    </span>
  );
}

type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";

const buttonVariants: Record<ButtonVariant, string> = {
  primary: "bg-accent text-bg hover:bg-accent-dim font-medium",
  secondary: "bg-bg-hover text-text hover:bg-bg-active border border-border",
  ghost: "text-text-secondary hover:text-text hover:bg-bg-hover",
  danger: "text-status-error hover:bg-status-error/10 border border-status-error/30",
};

export function Button({
  variant = "secondary",
  className,
  children,
  ...rest
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { variant?: ButtonVariant }) {
  return (
    <button
      className={clsx(
        "inline-flex items-center justify-center gap-1.5 px-3 h-8 rounded-md text-[13px] transition-colors disabled:opacity-40 disabled:pointer-events-none",
        buttonVariants[variant],
        className
      )}
      {...rest}
    >
      {children}
    </button>
  );
}

export function Spinner({ className }: { className?: string }) {
  return (
    <svg
      className={clsx("animate-spin w-4 h-4 text-accent", className)}
      viewBox="0 0 24 24"
      fill="none"
    >
      <circle className="opacity-20" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="3" />
      <path
        className="opacity-90"
        d="M12 2a10 10 0 0 1 10 10"
        stroke="currentColor"
        strokeWidth="3"
        strokeLinecap="round"
      />
    </svg>
  );
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center py-16 px-6 text-center">
      <div className="tlabel mb-2">∅</div>
      <h3 className="text-sm font-medium text-text">{title}</h3>
      {description && (
        <p className="text-[13px] text-text-muted mt-1.5 max-w-sm leading-relaxed">{description}</p>
      )}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

export function ProgressBar({
  value,
  tone = "accent",
  className,
}: {
  value: number;
  tone?: "accent" | "error" | "warning";
  className?: string;
}) {
  const clamped = Math.max(0, Math.min(100, value));
  const toneClass =
    tone === "error" ? "bg-status-error" : tone === "warning" ? "bg-status-warning" : "bg-accent";
  return (
    <div className={clsx("h-1.5 bg-bg rounded-full overflow-hidden", className)}>
      <div
        className={clsx("h-full rounded-full transition-[width] duration-500", toneClass)}
        style={{ width: `${clamped}%` }}
      />
    </div>
  );
}

export function Stat({
  label,
  value,
  sub,
  tone,
}: {
  label: string;
  value: React.ReactNode;
  sub?: React.ReactNode;
  tone?: BadgeTone;
}) {
  return (
    <Card className="p-4">
      <div className="tlabel mb-2">{label}</div>
      <div
        className={clsx(
          "readout text-xl",
          tone === "error" && "text-status-error",
          tone === "warning" && "text-status-warning",
          tone === "healthy" && "text-status-healthy"
        )}
      >
        {value}
      </div>
      {sub && <div className="text-xs text-text-muted mt-1">{sub}</div>}
    </Card>
  );
}

export function Dot({ tone }: { tone: "healthy" | "warning" | "error" | "offline" | "accent" }) {
  const color =
    tone === "healthy"
      ? "bg-status-healthy"
      : tone === "warning"
        ? "bg-status-warning"
        : tone === "error"
          ? "bg-status-error"
          : tone === "accent"
            ? "bg-accent"
            : "bg-status-offline";
  return <span className={clsx("w-1.5 h-1.5 rounded-full shrink-0", color)} />;
}

export function ErrorBanner({ message }: { message: string | null }) {
  if (!message) return null;
  return (
    <div className="mb-4 rounded-lg border border-status-error/40 bg-status-error/10 px-4 py-2.5 text-[13px] text-status-error">
      {message}
    </div>
  );
}

export function KeyValue({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-4 py-1.5">
      <span className="text-[13px] text-text-muted shrink-0">{label}</span>
      <span className="readout text-[13px] text-right break-all">{value}</span>
    </div>
  );
}
