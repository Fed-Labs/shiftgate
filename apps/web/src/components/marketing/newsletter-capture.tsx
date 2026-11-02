"use client";

import { useState } from "react";

// Newsletter / proof-drop capture (growth BR-03). No signup transport exists
// yet (POST /v1/waitlist is not implemented), so the form is a mailto
// hand-raise: submitting opens the visitor's mail client with the request
// prefilled. Nothing is stored in the browser or on the server by this
// component — the copy says exactly that.

const WHAT_OPTIONS = [
  "GPU workloads",
  "CI",
  "research computing",
  "homelab",
  "other",
];

export function NewsletterCapture({ sourcePage }: { sourcePage?: string }) {
  const [email, setEmail] = useState("");
  const [moving, setMoving] = useState(WHAT_OPTIONS[1]);
  const [state, setState] = useState<"idle" | "done">("idle");

  if (state === "done") {
    return (
      <div className="rounded-lg border border-border bg-bg-elevated p-6">
        <p className="text-sm text-text">
          Your mail client should have opened with the request — send the
          message to join the list. If nothing opened, email hello@fed-labs.xyz.
        </p>
      </div>
    );
  }

  return (
    <div className="rounded-lg border border-border bg-bg-elevated p-6">
      <h2 className="text-base font-semibold text-text">
        One email a week. Moves, rollbacks, and what broke.
      </h2>
      <p className="text-sm text-text-secondary mt-1 mb-4">
        Proof drops, benchmark notes, and the occasional honest failure. No spam;
        one click to unsubscribe.
      </p>
      <form
        className="flex flex-wrap gap-3"
        onSubmit={(event) => {
          event.preventDefault();
          const subject = encodeURIComponent("Add me to the SHIFT proof list");
          const body = encodeURIComponent(
            `Email: ${email}\nWhat are you moving: ${moving}\nSource page: ${sourcePage ?? "unknown"}\n\nPlease add me to the weekly proof list.`
          );
          window.location.href = `mailto:hello@fed-labs.xyz?subject=${subject}&body=${body}`;
          setState("done");
        }}
      >
        <input
          type="email"
          required
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          placeholder="you@company.com"
          className="min-w-0 flex-1 rounded-md border border-border bg-bg px-3 py-2 text-sm text-text"
          aria-label="Email address"
        />
        <select
          value={moving}
          onChange={(event) => setMoving(event.target.value)}
          className="rounded-md border border-border bg-bg px-3 py-2 text-sm text-text"
          aria-label="What are you moving?"
        >
          {WHAT_OPTIONS.map((option) => (
            <option key={option} value={option}>
              {option}
            </option>
          ))}
        </select>
        <button
          type="submit"
          className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-bg hover:bg-accent-dim transition-colors"
        >
          Get the proof
        </button>
      </form>
      <p className="text-xs text-text-muted mt-3">
        This form opens your mail client — nothing is stored in your browser.
        The list is added manually from your email until a signup endpoint
        ships. See the Privacy Policy.
      </p>
    </div>
  );
}
