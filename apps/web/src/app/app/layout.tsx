"use client";

import { useEffect, useState } from "react";
import { useRouter, usePathname } from "next/navigation";
import Link from "next/link";
import Image from "next/image";
import { useAuth } from "@/lib/store";
import { cn } from "@/lib/utils";

type NavItem = { href: string; label: string };
type NavGroup = { title?: string; items: NavItem[] };

const navGroups: NavGroup[] = [
  {
    items: [
      { href: "/app", label: "Overview" },
      { href: "/app/machines", label: "Machines" },
      { href: "/app/workloads", label: "Workloads" },
      { href: "/app/migrations", label: "Migrations" },
      { href: "/app/checkpoints", label: "Checkpoints" },
    ],
  },
  {
    title: "Resources",
    items: [
      { href: "/app/storage", label: "Storage" },
      { href: "/app/compute", label: "Compute" },
    ],
  },
  {
    title: "Organization",
    items: [
      { href: "/app/activity", label: "Activity" },
      { href: "/app/team", label: "Team" },
      { href: "/app/api-keys", label: "API Keys" },
      { href: "/app/billing", label: "Billing" },
      { href: "/app/settings", label: "Settings" },
    ],
  },
];

function Rail({
  pathname,
  onNavigate,
  onLogout,
  userName,
}: {
  pathname: string;
  onNavigate?: () => void;
  onLogout: () => void;
  userName?: string;
}) {
  return (
    <div className="flex flex-col h-full">
      {/* Wordmark */}
      <div className="px-5 h-14 flex items-center justify-between border-b border-border-subtle">
        <Link href="/app" className="flex items-center" onClick={onNavigate}>
          <Image src="/logo.png" alt="SHIFTGATE" width={100} height={24} className="h-5 w-auto" />
        </Link>
        <span className="tlabel">control</span>
      </div>

      {/* Nav */}
      <nav className="flex-1 overflow-y-auto py-4">
        {navGroups.map((group, gi) => (
          <div key={gi} className="mb-6">
            {group.title && <p className="tlabel px-5 mb-2">{group.title}</p>}
            {group.items.map((item) => {
              const active = pathname === item.href;
              return (
                <Link
                  key={item.href}
                  href={item.href}
                  onClick={onNavigate}
                  className={cn(
                    "relative flex items-center justify-between px-5 py-2 font-mono text-[12px] tracking-[0.08em] transition-colors",
                    active ? "text-text" : "text-text-muted hover:text-text-secondary"
                  )}
                >
                  {/* active tick */}
                  <span
                    className={cn(
                      "absolute left-0 top-1/2 -translate-y-1/2 w-[2px] h-4 transition-colors",
                      active ? "bg-accent" : "bg-transparent"
                    )}
                  />
                  <span>{item.label.toUpperCase()}</span>
                  {active && <span className="text-accent text-[10px]">●</span>}
                </Link>
              );
            })}
          </div>
        ))}
      </nav>

      {/* Footer */}
      <div className="px-5 py-4 border-t border-border-subtle">
        <p className="font-mono text-[11px] text-text-secondary truncate mb-2">{userName}</p>
        <button
          onClick={onLogout}
          className="font-mono text-[10px] tracking-[0.14em] text-text-muted hover:text-text transition-colors"
        >
          SIGN OUT →
        </button>
      </div>
    </div>
  );
}

export default function AppLayout({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const { isAuthenticated, isLoading, hydrate, logout, user } = useAuth();
  const [open, setOpen] = useState(false);

  useEffect(() => {
    hydrate();
  }, [hydrate]);

  useEffect(() => {
    if (!isLoading && !isAuthenticated) router.push("/login");
  }, [isLoading, isAuthenticated, router]);

  const handleLogout = async () => {
    await logout();
    router.push("/login");
  };

  if (isLoading) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-bg">
        <span className="font-mono text-[11px] text-text-muted tracking-[0.2em]">INITIALIZING…</span>
      </div>
    );
  }

  if (!isAuthenticated) return null;

  return (
    <div className="min-h-screen bg-bg flex">
      {/* Desktop rail */}
      <aside className="hidden lg:block w-56 shrink-0 border-r border-border-subtle sticky top-0 h-screen">
        <Rail pathname={pathname} onLogout={handleLogout} userName={user?.display_name} />
      </aside>

      {/* Mobile drawer */}
      {open && (
        <>
          <div className="fixed inset-0 bg-black/60 z-40 lg:hidden" onClick={() => setOpen(false)} />
          <aside className="fixed inset-y-0 left-0 w-60 z-50 bg-bg border-r border-border-subtle lg:hidden">
            <Rail pathname={pathname} onNavigate={() => setOpen(false)} onLogout={handleLogout} userName={user?.display_name} />
          </aside>
        </>
      )}

      <div className="flex-1 min-w-0 flex flex-col">
        {/* Mobile top bar */}
        <header className="lg:hidden h-14 border-b border-border-subtle flex items-center justify-between px-5 sticky top-0 bg-bg z-30">
          <Link href="/app" className="flex items-center">
            <Image src="/logo.png" alt="SHIFTGATE" width={100} height={24} className="h-5 w-auto" />
          </Link>
          <button className="font-mono text-[11px] text-text-muted" onClick={() => setOpen(true)}>
            MENU
          </button>
        </header>

        <main className="flex-1">{children}</main>
      </div>
    </div>
  );
}
