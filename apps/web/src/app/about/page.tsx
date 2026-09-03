import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Divider } from "@/components/ui";
import { ArrowRight } from "lucide-react";

export const metadata: Metadata = {
  title: "About",
  description:
    "SHIFTGATE moves running computation between machines. Here is why we built it.",
};

export default function AboutPage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <div className="space-y-8">
              <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                About SHIFTGATE
              </h1>

              <div className="space-y-5 text-text-secondary leading-relaxed">
                <p className="text-lg text-text">
                  Computation should not be permanently tied to the machine it
                  happens to be running on.
                </p>

                <p>
                  We built SHIFTGATE because moving workloads between machines
                  is still unnecessarily painful. Processes accumulate state —
                  memory, file handles, network connections — and that state
                  makes migration feel like a hard problem. It is not as hard as
                  people think.
                </p>

                <p>
                  SHIFTGATE checkpoints running processes with CRIU, transfers encrypted
                  chunks between agents, and restores them after validation. Live mode uses
                  real pre-copy before its final stop; cold mode makes that stop explicit.
                </p>

                <p>
                  The system is designed for operators who manage real
                  infrastructure — not demo workloads. It handles the edge
                  cases through explicit portable, machine-specific, and external state
                  classes. The goal is to make supported moves routine—and unsupported
                  boundaries obvious.
                </p>
              </div>
            </div>
          </Container>
        </Section>

        <Divider />

        <Section>
          <Container narrow>
            <div className="space-y-6">
              <h2 className="text-xl font-semibold">Team</h2>
              <p className="text-text-secondary leading-relaxed">
                SHIFTGATE is built by a small team of engineers with backgrounds
                in systems programming, distributed infrastructure, and Linux
                kernel development. We care about getting the details right
                rather than shipping fast and breaking things.
              </p>
              <p className="text-text-secondary leading-relaxed">
                We are a remote team. No office, no headquarters. We work
                asynchronously and ship when the work is ready.
              </p>
            </div>
          </Container>
        </Section>

        <Divider />

        <Section>
          <Container narrow>
            <div className="space-y-6">
              <h2 className="text-xl font-semibold">Get in touch</h2>
              <p className="text-text-secondary leading-relaxed">
                Questions, feedback, or want to talk about what we&apos;re
                building? We read everything.
              </p>
              <a
                href="/contact"
                className="inline-flex items-center gap-2 text-sm text-accent hover:text-accent-dim transition-colors"
              >
                Contact us
                <ArrowRight className="w-4 h-4" />
              </a>
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
