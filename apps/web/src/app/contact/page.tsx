"use client";

import { useState, type FormEvent } from "react";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card, Button } from "@/components/ui";
import { Shield, ExternalLink, Send } from "lucide-react";

export default function ContactPage() {
  const [email, setEmail] = useState("");
  const [message, setMessage] = useState("");
  const [status, setStatus] = useState<"idle" | "sending" | "sent" | "error">(
    "idle"
  );

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (!email.trim() || !message.trim()) return;

    setStatus("sending");
    // Placeholder — replace with actual API call
    await new Promise((r) => setTimeout(r, 1000));
    setStatus("sent");
    setEmail("");
    setMessage("");
  };

  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <div className="text-center space-y-4 mb-12">
              <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                Contact
              </h1>
              <p className="text-text-secondary text-lg">
                Send us a message or reach out directly.
              </p>
            </div>

            {/* Form */}
            <Card className="mb-8">
              <form onSubmit={handleSubmit} className="space-y-5">
                <div className="space-y-2">
                  <label
                    htmlFor="email"
                    className="block text-sm font-medium text-text-secondary"
                  >
                    Email
                  </label>
                  <input
                    id="email"
                    type="email"
                    required
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    placeholder="you@example.com"
                    className="w-full bg-bg-surface border border-border rounded-lg px-3.5 py-2.5 text-sm text-text placeholder:text-text-muted focus:outline-none focus:ring-1 focus:ring-accent/50 focus:border-accent/50 transition-colors"
                  />
                </div>

                <div className="space-y-2">
                  <label
                    htmlFor="message"
                    className="block text-sm font-medium text-text-secondary"
                  >
                    Message
                  </label>
                  <textarea
                    id="message"
                    required
                    rows={5}
                    value={message}
                    onChange={(e) => setMessage(e.target.value)}
                    placeholder="What can we help with?"
                    className="w-full bg-bg-surface border border-border rounded-lg px-3.5 py-2.5 text-sm text-text placeholder:text-text-muted focus:outline-none focus:ring-1 focus:ring-accent/50 focus:border-accent/50 transition-colors resize-y"
                  />
                </div>

                <div className="flex items-center gap-3">
                  <Button
                    type="submit"
                    disabled={status === "sending" || status === "sent"}
                  >
                    {status === "sending" ? (
                      "Sending..."
                    ) : status === "sent" ? (
                      "Message sent"
                    ) : (
                      <>
                        <Send className="w-4 h-4 mr-2" />
                        Send message
                      </>
                    )}
                  </Button>
                  {status === "sent" && (
                    <span className="text-sm text-accent">
                      Thanks — we&apos;ll get back to you.
                    </span>
                  )}
                  {status === "error" && (
                    <span className="text-sm text-status-error">
                      Something went wrong. Try again or email us directly.
                    </span>
                  )}
                </div>
              </form>
            </Card>

            {/* Other channels */}
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-5">
              <Card>
                <div className="flex items-start gap-3">
                  <Shield className="w-5 h-5 text-text-muted mt-0.5 shrink-0" />
                  <div className="space-y-1">
                    <div className="text-sm font-medium">
                      Security issues
                    </div>
                    <p className="text-sm text-text-secondary">
                      Report vulnerabilities privately.
                    </p>
                    <a
                      href="mailto:security@shiftgate.dev"
                      className="text-sm text-accent hover:text-accent-dim transition-colors"
                    >
                      security@shiftgate.dev
                    </a>
                  </div>
                </div>
              </Card>

              <Card>
                <div className="flex items-start gap-3">
                  <ExternalLink className="w-5 h-5 text-text-muted mt-0.5 shrink-0" />
                  <div className="space-y-1">
                    <div className="text-sm font-medium">GitHub</div>
                    <p className="text-sm text-text-secondary">
                      File issues, browse the source, contribute.
                    </p>
                    <a
                      href="https://github.com/Fed-Labs/shiftgate"
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-sm text-accent hover:text-accent-dim transition-colors"
                    >
                      github.com/Fed-Labs/shiftgate
                    </a>
                  </div>
                </div>
              </Card>
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
