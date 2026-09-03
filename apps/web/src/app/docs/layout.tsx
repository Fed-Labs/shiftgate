"use client";

import { useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { cn } from "@/lib/utils";
import {
  BookOpen,
  Rocket,
  Lightbulb,
  Monitor,
  Box,
  Camera,
  ArrowLeftRight,
  Braces,
  Terminal,
  Shield,
  Wrench,
  Menu,
  X,
  Search,
} from "lucide-react";

const NAV_SECTIONS = [
  { title: "Quickstart", slug: "quickstart", icon: Rocket },
  { title: "Concepts", slug: "concepts", icon: Lightbulb },
  { title: "Machines", slug: "machines", icon: Monitor },
  { title: "Workloads", slug: "workloads", icon: Box },
  { title: "Checkpoints", slug: "checkpoints", icon: Camera },
  { title: "Migration", slug: "migration", icon: ArrowLeftRight },
  { title: "API", slug: "api", icon: Braces },
  { title: "CLI", slug: "cli", icon: Terminal },
  { title: "Security", slug: "security", icon: Shield },
  { title: "Troubleshooting", slug: "troubleshooting", icon: Wrench },
];

export default function DocsLayout({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const [sidebarOpen, setSidebarOpen] = useState(false);

  return (
    <>
      <SiteHeader />
      <main className="pt-14">
        <div className="flex">
          {/* Mobile toggle */}
          <button
            onClick={() => setSidebarOpen(!sidebarOpen)}
            className="fixed bottom-6 right-6 z-50 flex h-12 w-12 items-center justify-center rounded-full bg-accent text-bg shadow-lg md:hidden"
            aria-label="Toggle navigation"
          >
            {sidebarOpen ? <X size={20} /> : <Menu size={20} />}
          </button>

          {/* Backdrop */}
          {sidebarOpen && (
            <div
              className="fixed inset-0 z-30 bg-black/60 md:hidden"
              onClick={() => setSidebarOpen(false)}
            />
          )}

          {/* Sidebar */}
          <aside
            className={cn(
              "fixed top-14 bottom-0 z-40 w-72 shrink-0 border-r border-border-subtle bg-bg overflow-y-auto transition-transform duration-200 md:sticky md:top-14 md:h-[calc(100vh-3.5rem)] md:translate-x-0",
              sidebarOpen ? "translate-x-0" : "-translate-x-full"
            )}
          >
            <div className="p-5">
              <div className="mb-6">
                <div className="flex items-center gap-2 mb-4">
                  <BookOpen size={16} className="text-accent" />
                  <span className="text-sm font-semibold tracking-tight">
                    Documentation
                  </span>
                </div>
                <div className="flex items-center gap-2 rounded-md border border-border bg-bg-surface px-3 py-2 text-sm text-text-muted">
                  <Search size={14} />
                  <span>Search docs…</span>
                </div>
              </div>

              <nav className="space-y-0.5">
                {NAV_SECTIONS.map((item) => {
                  const Icon = item.icon;
                  const isActive = pathname === `/docs/${item.slug}`;
                  return (
                    <Link
                      key={item.slug}
                      href={`/docs/${item.slug}`}
                      onClick={() => setSidebarOpen(false)}
                      className={cn(
                        "flex items-center gap-3 rounded-md px-3 py-2 text-sm transition-colors",
                        isActive
                          ? "bg-bg-surface text-text font-medium"
                          : "text-text-secondary hover:bg-bg-hover hover:text-text"
                      )}
                    >
                      <Icon size={15} className={isActive ? "text-accent" : "text-text-muted"} />
                      {item.title}
                    </Link>
                  );
                })}
              </nav>
            </div>
          </aside>

          {/* Content */}
          <div className="min-w-0 flex-1">{children}</div>
        </div>
      </main>
      <SiteFooter />
    </>
  );
}
