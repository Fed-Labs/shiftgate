// The application shell: sidebar navigation, live agent/control-plane
// connection indicators, and the screen switcher. Screens are plain
// components; navigation is a store field, not a router — five destinations
// do not need URL semantics inside a desktop window.

import React from "react";
import clsx from "clsx";
import {
  History,
  Laptop,
  LayoutDashboard,
  Settings,
  Workflow,
} from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import { backend } from "@/lib/backend";
import { useAuth } from "@/lib/store";
import { Dot } from "@/components/ui";

export type Screen = "dashboard" | "machines" | "workloads" | "history" | "settings";

const NAV: Array<{ id: Screen; label: string; icon: React.ElementType }> = [
  { id: "dashboard", label: "Dashboard", icon: LayoutDashboard },
  { id: "machines", label: "Machines", icon: Laptop },
  { id: "workloads", label: "Workloads", icon: Workflow },
  { id: "history", label: "History", icon: History },
  { id: "settings", label: "Settings", icon: Settings },
];

export function Shell({
  screen,
  onNavigate,
  children,
}: {
  screen: Screen;
  onNavigate: (screen: Screen) => void;
  children: React.ReactNode;
}) {
  const agentHealth = useQuery({
    queryKey: ["agent-health"],
    queryFn: backend.health,
    refetchInterval: 10_000,
    retry: false,
  });

  const { user, organization, controlPlaneURL } = useAuth();
  const agentOnline = agentHealth.isSuccess;

  return (
    <div className="flex h-full">
      {/* Sidebar */}
      <aside className="w-52 shrink-0 border-r border-border flex flex-col bg-bg-elevated">
        <div className="px-5 h-14 flex items-center gap-2.5 border-b border-border">
          <svg viewBox="0 0 24 24" className="w-5 h-5 text-accent" fill="none">
            <path d="M6 4l7 8-7 8" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" />
            <path d="M13 4l7 8-7 8" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round" opacity="0.45" />
          </svg>
          <span className="text-[15px] font-semibold tracking-wide">SHIFT</span>
        </div>

        <nav className="flex-1 py-3 px-2.5 space-y-0.5">
          {NAV.map(({ id, label, icon: Icon }) => (
            <button
              key={id}
              onClick={() => onNavigate(id)}
              className={clsx(
                "w-full flex items-center gap-2.5 px-2.5 h-9 rounded-md text-[13px] transition-colors",
                screen === id
                  ? "bg-bg-active text-text"
                  : "text-text-secondary hover:text-text hover:bg-bg-hover"
              )}
            >
              <Icon className="w-4 h-4 shrink-0" />
              {label}
              {screen === id && <span className="ml-auto w-1 h-1 rounded-full bg-accent" />}
            </button>
          ))}
        </nav>

        {/* Connection status — the honest state of both endpoints */}
        <div className="px-4 py-3 border-t border-border space-y-2">
          <div className="tlabel">Connections</div>
          <div className="flex items-center gap-2 text-[12px] text-text-secondary">
            <Dot tone={agentOnline ? "healthy" : "error"} />
            <span className="truncate">
              Agent {agentOnline ? `· ${agentHealth.data?.version ?? ""}` : "offline"}
            </span>
          </div>
          <div className="flex items-center gap-2 text-[12px] text-text-secondary">
            <Dot tone={controlPlaneURL ? "accent" : "offline"} />
            <span className="truncate">
              {controlPlaneURL ? (organization?.name ?? "Control plane") : "Control plane unset"}
            </span>
          </div>
          {user && (
            <div className="text-[11px] text-text-faint truncate pt-1">{user.email}</div>
          )}
        </div>
      </aside>

      {/* Main column */}
      <div className="flex-1 flex flex-col min-w-0">
        <main className="flex-1 overflow-y-auto">
          <div className="max-w-5xl mx-auto px-8 py-6">{children}</div>
        </main>
      </div>
    </div>
  );
}
