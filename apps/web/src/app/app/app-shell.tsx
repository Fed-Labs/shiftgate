"use client";

import { useEffect, useRef, useState } from "react";
import { useRouter, usePathname } from "next/navigation";
import Link from "next/link";
import Image from "next/image";
import { useAuth } from "@/lib/store";
import type { Organization } from "@/lib/types";
import { cn } from "@/lib/utils";
import { ErrorState } from "@/components/state";

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
      { href: "/app/security", label: "Security" },
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
  organizations,
  currentOrg,
  onSwitchOrg,
}: {
  pathname: string;
  onNavigate?: () => void;
  onLogout: () => void;
  userName?: string;
  organizations: Organization[];
  currentOrg: Organization | null;
  onSwitchOrg: (org: Organization) => void;
}) {
  return (
    <div className="flex flex-col h-full">
      {/* Wordmark */}
      <div className="px-5 h-14 flex items-center justify-between border-b border-border-subtle">
        <Link href="/app" className="flex items-center" onClick={onNavigate}>
          <Image src="/logo.png" alt="SHIFT" width={100} height={24} className="h-5 w-auto" />
        </Link>
        <span className="tlabel">control</span>
      </div>

      {/* Org switcher — multi-org accounts can reach every organization they
          belong to instead of being pinned to the oldest one. */}
      {organizations.length > 0 && (
        <div className="px-5 pt-4 pb-1">
          <p className="tlabel mb-1.5">organization</p>
          <select
            aria-label="Switch organization"
            value={currentOrg?.id || organizations[0].id}
            onChange={(event) => {
              const next = organizations.find((org) => org.id === event.target.value);
              if (next && next.id !== currentOrg?.id) onSwitchOrg(next);
            }}
            className="w-full bg-bg-surface border border-border rounded-md px-2 py-1.5 font-mono text-[11px] text-text focus:outline-none focus:border-accent transition-colors"
          >
            {organizations.map((org) => (
              <option key={org.id} value={org.id}>
                {org.name}
              </option>
            ))}
          </select>
        </div>
      )}

      {/* Nav */}
      <nav className="flex-1 overflow-y-auto py-4" aria-label="Dashboard">
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
                  aria-current={active ? "page" : undefined}
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
          type="button"
          onClick={onLogout}
          className="font-mono text-[10px] tracking-[0.14em] text-text-muted hover:text-text transition-colors"
        >
          SIGN OUT →
        </button>
      </div>
    </div>
  );
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const {
    isAuthenticated,
    isLoading,
    hydrate,
    logout,
    user,
    organization,
    organizations,
    orgError,
    setOrganization,
    restoreOrganization,
  } = useAuth();
  const [open, setOpen] = useState(false);
  const drawerRef = useRef<HTMLElement>(null);
  const restoreFocusRef = useRef<HTMLElement | null>(null);

  // Focus management for the mobile navigation drawer: focus in on open, trap
  // Tab, close on Escape, restore focus to the MENU trigger on close.
  useEffect(() => {
    if (!open) return;
    restoreFocusRef.current = (document.activeElement as HTMLElement) || null;
    drawerRef.current?.focus();
    return () => restoreFocusRef.current?.focus?.();
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
      if (event.key === "Tab" && drawerRef.current) {
        const focusable = drawerRef.current.querySelectorAll<HTMLElement>(
          'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), audio[controls], video[controls], [href], [tabindex]:not([tabindex="-1"])'
        );
        if (focusable.length === 0) return;
        const first = focusable[0];
        const last = focusable[focusable.length - 1];
        const active = document.activeElement;
        if (event.shiftKey && active === first) {
          event.preventDefault();
          last.focus();
        } else if (!event.shiftKey && active === last) {
          event.preventDefault();
          first.focus();
        }
      }
    };
    const onFocusIn = (event: FocusEvent) => {
      const drawer = drawerRef.current;
      if (!drawer) return;
      if (event.target instanceof Node && !drawer.contains(event.target)) {
        drawer.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    document.addEventListener("focusin", onFocusIn);
    return () => {
      window.removeEventListener("keydown", onKey);
      document.removeEventListener("focusin", onFocusIn);
    };
  }, [open]);

  useEffect(() => {
    void hydrate();
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

  const railProps = {
    pathname,
    onLogout: handleLogout,
    userName: user?.display_name,
    organizations,
    currentOrg: organization,
    onSwitchOrg: (org: Organization) => setOrganization(org),
  };

  return (
    <div className="min-h-screen bg-bg flex">
      {/* Desktop rail */}
      <aside className="hidden lg:block w-56 shrink-0 border-r border-border-subtle sticky top-0 h-screen">
        <Rail {...railProps} />
      </aside>

      {/* Mobile drawer */}
      {open && (
        <>
          <div className="fixed inset-0 bg-black/60 z-40 lg:hidden" onClick={() => setOpen(false)} />
          <aside
            ref={drawerRef}
            id="mobile-nav"
            tabIndex={-1}
            className="fixed inset-y-0 left-0 w-60 z-50 bg-bg border-r border-border-subtle lg:hidden outline-none"
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
          >
            <Rail {...railProps} onNavigate={() => setOpen(false)} />
          </aside>
        </>
      )}

      <div className="flex-1 min-w-0 flex flex-col">
        {/* Mobile top bar */}
        <header className="lg:hidden h-14 border-b border-border-subtle flex items-center justify-between px-5 sticky top-0 bg-bg z-30">
          <Link href="/app" className="flex items-center">
            <Image src="/logo.png" alt="SHIFT" width={100} height={24} className="h-5 w-auto" />
          </Link>
          <button
            type="button"
            className="font-mono text-[11px] text-text-muted"
            onClick={() => setOpen(true)}
            aria-expanded={open}
            aria-controls="mobile-nav"
            aria-label="Open navigation menu"
          >
            MENU
          </button>
        </header>

        <main className="flex-1">
          {isAuthenticated && !organization && orgError ? (
            <div className="p-8">
              <ErrorState
                title="Could not load your organization"
                message={`${orgError} Every dashboard page needs an organization to query — retry, or sign out and back in.`}
                onRetry={() => void restoreOrganization()}
              />
            </div>
          ) : (
            children
          )}
        </main>
      </div>
    </div>
  );
}
