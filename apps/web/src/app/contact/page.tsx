import type { Metadata } from "next";
import Link from "next/link";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card } from "@/components/ui";
import { Shield, ExternalLink, Mail } from "lucide-react";

export const metadata: Metadata = {
  title: "Contact",
  description: "Contact the SHIFT team about a technical evaluation, security review, or contribution.",
};

export default function ContactPage() {
  return <><SiteHeader /><main className="pt-14 flex-1"><Section><Container narrow><div className="text-center space-y-4 mb-12"><p className="text-xs font-mono uppercase tracking-widest text-accent">Talk to an engineer</p><h1 className="text-3xl md:text-4xl font-semibold tracking-tight">Bring one hard workload.</h1><p className="text-text-secondary text-lg">This site does not submit contact forms. Email us directly and include the workload shape, Linux environment, and the outcome you need to prove.</p></div><Card className="mb-8"><div className="flex flex-col sm:flex-row sm:items-center justify-between gap-6"><div><p className="text-xs font-mono uppercase tracking-widest text-text-muted mb-3">Technical evaluation</p><h2 className="text-xl font-semibold">Start a conversation</h2><p className="text-sm text-text-secondary mt-2">We will reply from the address listed in your mail client.</p></div><a className="inline-flex items-center gap-2 border border-accent bg-accent px-4 py-3 text-sm font-medium text-bg hover:bg-text" href="mailto:hello@fed-labs.xyz?subject=SHIFT%20technical%20evaluation"><Mail className="w-4 h-4" /> Email the team</a></div></Card><div className="grid grid-cols-1 sm:grid-cols-2 gap-5"><Card><div className="flex items-start gap-3"><Shield className="w-5 h-5 text-text-muted mt-0.5 shrink-0" /><div className="space-y-1"><div className="text-sm font-medium">Security issues</div><p className="text-sm text-text-secondary">Report vulnerabilities privately.</p><a href="mailto:hello@fed-labs.xyz" className="text-sm text-accent">hello@fed-labs.xyz</a></div></div></Card><Card><div className="flex items-start gap-3"><ExternalLink className="w-5 h-5 text-text-muted mt-0.5 shrink-0" /><div className="space-y-1"><div className="text-sm font-medium">Source and docs</div><p className="text-sm text-text-secondary">Browse the implementation and open issues.</p><a href="https://github.com/Fed-Labs/shiftgate" target="_blank" rel="noopener noreferrer" className="text-sm text-accent">GitHub repository</a></div></div></Card></div><p className="text-center text-sm text-text-muted mt-10"><Link href="/docs/quickstart" className="text-accent">Prefer to test it yourself? Start with the quickstart →</Link></p></Container></Section></main><SiteFooter /></>;
}
