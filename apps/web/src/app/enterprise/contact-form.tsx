"use client";

import { useState } from "react";
import { Button } from "@/components/ui";
import { Send } from "lucide-react";

export function EnterpriseContactForm() {
  const [submitted, setSubmitted] = useState(false);

  if (submitted) {
    return (
      <div className="rounded-lg border border-accent/20 bg-accent/5 p-6 text-center">
        <p className="text-sm text-accent font-medium">
          Thank you. We&apos;ll be in touch.
        </p>
      </div>
    );
  }

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        setSubmitted(true);
      }}
      className="space-y-4"
    >
      <div>
        <label className="block text-xs font-mono uppercase tracking-wider text-text-muted mb-1.5">
          Name
        </label>
        <input
          type="text"
          required
          className="w-full rounded-md border border-border bg-bg-surface px-3 py-2.5 text-sm text-text placeholder:text-text-muted focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30"
          placeholder="Your name"
        />
      </div>
      <div>
        <label className="block text-xs font-mono uppercase tracking-wider text-text-muted mb-1.5">
          Email
        </label>
        <input
          type="email"
          required
          className="w-full rounded-md border border-border bg-bg-surface px-3 py-2.5 text-sm text-text placeholder:text-text-muted focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30"
          placeholder="you@company.com"
        />
      </div>
      <div>
        <label className="block text-xs font-mono uppercase tracking-wider text-text-muted mb-1.5">
          Company
        </label>
        <input
          type="text"
          required
          className="w-full rounded-md border border-border bg-bg-surface px-3 py-2.5 text-sm text-text placeholder:text-text-muted focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30"
          placeholder="Company name"
        />
      </div>
      <div>
        <label className="block text-xs font-mono uppercase tracking-wider text-text-muted mb-1.5">
          Message
        </label>
        <textarea
          rows={4}
          className="w-full rounded-md border border-border bg-bg-surface px-3 py-2.5 text-sm text-text placeholder:text-text-muted focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30 resize-none"
          placeholder="Tell us about your infrastructure and requirements..."
        />
      </div>
      <Button type="submit" variant="primary">
        <Send size={14} className="mr-2" />
        Send message
      </Button>
    </form>
  );
}
