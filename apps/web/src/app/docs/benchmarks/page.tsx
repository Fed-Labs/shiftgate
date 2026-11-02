import type { Metadata } from "next";
import Link from "next/link";
import { Container, Section } from "@/components/ui";
import { SiteHeader, SiteFooter } from "@/components/site-header";

export const metadata: Metadata = {
  title: "Benchmarks",
  description: "Measured SHIFT checkpoint, restore, and migration results with methodology and limitations.",
};

export default function BenchmarksPage() {
  return <><SiteHeader /><main className="pt-14"><Section><Container narrow><p className="text-xs font-mono uppercase tracking-widest text-accent mb-4">Evidence</p><h1 className="text-4xl md:text-5xl font-bold tracking-tight mb-6">Benchmarks with the caveats included.</h1><p className="text-lg text-text-secondary leading-relaxed mb-10">Results depend on workload shape, filesystem, hardware, and network. The benchmark document records the environment, measured windows, and what the numbers do not prove.</p><div className="rounded-lg border border-border bg-bg-elevated p-6"><p className="text-sm text-text-secondary leading-relaxed mb-5">Review the reproducible methodology and the measured checkpoint, restore, cold migration, live pre-copy, and lazy restore ranges.</p><Link href="https://github.com/Fed-Labs/shiftgate/blob/main/docs/benchmarks.md" className="text-accent text-sm">Open the benchmark methodology on GitHub →</Link></div></Container></Section></main><SiteFooter /></>;
}
