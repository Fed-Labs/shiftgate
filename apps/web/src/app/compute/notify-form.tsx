"use client";

import { useState } from "react";
import { Button } from "@/components/ui";

export function ComputeNotifyForm() {
  const [email, setEmail] = useState("");
  const [notified, setNotified] = useState(false);

  if (notified) {
    return (
      <p className="text-sm text-accent">
        You&apos;re on the list. We&apos;ll be in touch.
      </p>
    );
  }

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (email) setNotified(true);
      }}
      className="flex gap-2"
    >
      <input
        type="email"
        required
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        placeholder="you@company.com"
        className="flex-1 rounded-md border border-border bg-bg-surface px-3 py-2 text-sm text-text placeholder:text-text-muted focus:border-accent focus:outline-none focus:ring-1 focus:ring-accent/30"
      />
      <Button type="submit" variant="primary" size="sm">
        Notify me
      </Button>
    </form>
  );
}
