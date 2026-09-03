"use client";

import Link from "next/link";
import Image from "next/image";
import { usePathname } from "next/navigation";
import { cn } from "@/lib/utils";
import { useState } from "react";

const nav = [
  { href: "/technology", label: "Technology" },
  { href: "/developers", label: "Developers" },
  { href: "/pricing", label: "Pricing" },
  { href: "/docs", label: "Docs" },
];

export function SiteHeader() {
  const pathname = usePathname();
  const [mobileOpen, setMobileOpen] = useState(false);

  return (
    <header className="fixed top-0 left-0 right-0 z-50 border-b border-border-subtle bg-bg/85 backdrop-blur-md">
      <div className="max-w-6xl mx-auto px-5 md:px-10 h-14 flex items-center justify-between">
        <Link href="/" className="flex items-center shrink-0 group">
          <Image
            src="/logo.png"
            alt="SHIFTGATE"
            width={120}
            height={28}
            className="h-6 w-auto"
            priority
          />
        </Link>

        <nav className="hidden md:flex items-center gap-8">
          {nav.map((item) => (
            <Link
              key={item.href}
              href={item.href}
              className={cn(
                "font-mono text-[11px] tracking-[0.14em] transition-colors",
                pathname === item.href ? "text-text" : "text-text-muted hover:text-text"
              )}
            >
              {item.label.toUpperCase()}
            </Link>
          ))}
        </nav>

        <div className="hidden md:flex items-center gap-6">
          <Link href="/login" className="font-mono text-[11px] tracking-[0.14em] text-text-muted hover:text-text transition-colors">
            SIGN IN
          </Link>
          <Link
            href="/download"
            className="font-mono text-[11px] tracking-[0.14em] text-accent border border-accent/40 px-3 py-1.5 hover:bg-accent hover:text-bg transition-colors"
          >
            DOWNLOAD
          </Link>
        </div>

        <button
          className="md:hidden font-mono text-[11px] text-text-muted"
          onClick={() => setMobileOpen(!mobileOpen)}
          aria-label="Toggle menu"
        >
          {mobileOpen ? "CLOSE" : "MENU"}
        </button>
      </div>

      {mobileOpen && (
        <div className="md:hidden border-t border-border-subtle bg-bg">
          <nav className="flex flex-col px-5 py-4 gap-4">
            {nav.map((item) => (
              <Link
                key={item.href}
                href={item.href}
                className="font-mono text-[12px] tracking-[0.14em] text-text-secondary"
                onClick={() => setMobileOpen(false)}
              >
                {item.label.toUpperCase()}
              </Link>
            ))}
            <div className="h-px bg-border-subtle my-1" />
            <Link href="/login" className="font-mono text-[12px] text-text-secondary" onClick={() => setMobileOpen(false)}>
              SIGN IN
            </Link>
            <Link href="/download" className="font-mono text-[12px] text-accent" onClick={() => setMobileOpen(false)}>
              DOWNLOAD
            </Link>
          </nav>
        </div>
      )}
    </header>
  );
}

export function SiteFooter() {
  return (
    <footer className="border-t border-border-subtle">
      <div className="max-w-6xl mx-auto px-5 md:px-10 py-14">
        <div className="grid grid-cols-2 md:grid-cols-4 gap-10 mb-14">
          <FooterCol title="PRODUCT" links={[["/technology", "Technology"], ["/pricing", "Pricing"], ["/download", "Download"], ["/status", "Status"]]} />
          <FooterCol title="DEVELOPERS" links={[["/docs", "Documentation"], ["/developers", "API"], ["/docs/cli", "CLI"], ["/changelog", "Changelog"]]} />
          <FooterCol title="COMPANY" links={[["/about", "About"], ["/contact", "Contact"], ["/enterprise", "Enterprise"]]} />
          <div>
            <p className="tlabel mb-4">SYSTEM</p>
            <div className="font-mono text-[11px] text-text-muted leading-relaxed">
              <div className="flex items-center gap-2 mb-1">
                <span className="w-[5px] h-[5px] bg-status-healthy" /> CONTROL PLANE
              </div>
              <div className="flex items-center gap-2 mb-1">
                <span className="w-[5px] h-[5px] bg-status-healthy" /> API
              </div>
              <div className="flex items-center gap-2">
                <span className="w-[5px] h-[5px] bg-status-healthy" /> STORAGE
              </div>
            </div>
          </div>
        </div>

        <div className="flex flex-col md:flex-row items-start md:items-center justify-between gap-4 pt-8 border-t border-border-subtle">
          <div className="flex items-center gap-3">
            <Image src="/icon.png" alt="SHIFTGATE" width={20} height={20} className="h-5 w-5" />
            <span className="font-mono text-[12px] tracking-[0.2em] text-text">SHIFTGATE</span>
          </div>
          <p className="font-mono text-[10px] text-text-faint">
            © {new Date().getFullYear()} — COMPUTATION SHOULD MOVE FREELY
          </p>
        </div>
      </div>
    </footer>
  );
}

function FooterCol({ title, links }: { title: string; links: [string, string][] }) {
  return (
    <div>
      <p className="tlabel mb-4">{title}</p>
      <div className="flex flex-col gap-2">
        {links.map(([href, label]) => (
          <Link key={href} href={href} className="text-sm text-text-secondary hover:text-text transition-colors">
            {label}
          </Link>
        ))}
      </div>
    </div>
  );
}
