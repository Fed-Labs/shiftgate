"use client";

import { cn } from "@/lib/utils";
import { type ButtonHTMLAttributes, type ReactNode, forwardRef } from "react";

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: "primary" | "secondary" | "ghost" | "danger";
  size?: "sm" | "md" | "lg";
  children: ReactNode;
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  ({ variant = "primary", size = "md", className, children, ...props }, ref) => {
    return (
      <button
        ref={ref}
        className={cn(
          "inline-flex items-center justify-center font-mono font-medium transition-colors duration-150 cursor-pointer tracking-[0.08em]",
          "focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-accent",
          "disabled:opacity-40 disabled:pointer-events-none",
          {
            "bg-accent text-bg hover:bg-accent-dim": variant === "primary",
            "bg-bg-surface text-text border border-border hover:bg-bg-hover": variant === "secondary",
            "text-text-secondary hover:text-text hover:bg-bg-hover": variant === "ghost",
            "bg-status-error/10 text-status-error border border-status-error/20 hover:bg-status-error/20": variant === "danger",
          },
          {
            "px-3 py-1.5 text-xs": size === "sm",
            "px-4 py-2 text-sm": size === "md",
            "px-6 py-3 text-base": size === "lg",
          },
          className
        )}
        {...props}
      >
        {children}
      </button>
    );
  }
);
Button.displayName = "Button";

export function Badge({
  children,
  variant = "default",
  className,
}: {
  children: ReactNode;
  variant?: "default" | "success" | "warning" | "error" | "info" | "accent";
  className?: string;
}) {
  return (
    <span
      className={cn(
        "inline-flex items-center px-2 py-0.5 font-mono text-[10px] tracking-[0.12em] uppercase",
        {
          "bg-bg-active text-text-secondary": variant === "default",
          "bg-status-healthy/10 text-status-healthy": variant === "success",
          "bg-status-warning/10 text-status-warning": variant === "warning",
          "bg-status-error/10 text-status-error": variant === "error",
          "bg-status-info/10 text-status-info": variant === "info",
          "bg-accent/10 text-accent": variant === "accent",
        },
        className
      )}
    >
      {children}
    </span>
  );
}

export function StatusDot({ status, className }: { status: "online" | "offline" | "running" | "error" | "warning"; className?: string }) {
  const colors = {
    online: "bg-status-healthy",
    running: "bg-status-healthy",
    offline: "bg-status-offline",
    error: "bg-status-error",
    warning: "bg-status-warning",
  };
  return (
    <span
      className={cn(
        "inline-block w-2 h-2 rounded-full",
        colors[status],
        (status === "online" || status === "running") && "animate-pulse-dot",
        className
      )}
    />
  );
}

export function Card({
  children,
  className,
  hover,
}: {
  children: ReactNode;
  className?: string;
  hover?: boolean;
}) {
  return (
    <div
      className={cn(
        "group relative overflow-hidden border border-border-subtle bg-bg-elevated transition-colors duration-200",
        hover && "hover:border-border-strong",
        className
      )}
    >
      {children}
    </div>
  );
}

export function Section({
  children,
  className,
  id,
}: {
  children: ReactNode;
  className?: string;
  id?: string;
}) {
  return (
    <section id={id} className={cn("py-20 md:py-28", className)}>
      {children}
    </section>
  );
}

export function Container({
  children,
  className,
  narrow,
}: {
  children: ReactNode;
  className?: string;
  narrow?: boolean;
}) {
  return (
    <div className={cn("mx-auto px-5 md:px-8", narrow ? "max-w-3xl" : "max-w-6xl", className)}>
      {children}
    </div>
  );
}

export function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span className={cn("font-mono tabular-nums", className)}>
      {children}
    </span>
  );
}

export function Divider({ className }: { className?: string }) {
  return <div className={cn("h-px bg-border", className)} />;
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center py-16 text-center">
      <p className="text-text font-medium mb-1">{title}</p>
      <p className="text-text-secondary text-sm mb-4 max-w-sm">{description}</p>
      {action}
    </div>
  );
}

export function Stat({
  label,
  value,
  mono = true,
}: {
  label: string;
  value: string | number;
  mono?: boolean;
}) {
  return (
    <div className="relative">
      <p className="text-text-muted text-[11px] font-medium uppercase tracking-widest mb-2">{label}</p>
      <p className={cn("text-3xl font-semibold tracking-tight text-text", mono && "font-mono tabular-nums")}>
        {value}
      </p>
      <span aria-hidden className="absolute bottom-0 left-0 h-px w-12 bg-gradient-to-r from-accent/60 to-transparent" />
    </div>
  );
}
