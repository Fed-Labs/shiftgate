"use client";

// Dashboard segment boundary (report 23): a render throw inside /app must not
// drop the operator out of the nav rail onto the bare production crash page.
import { useEffect } from "react";
import Link from "next/link";
import { ErrorState } from "@/components/state";

export default function Error({
  error,
  retry,
}: {
  error: Error & { digest?: string };
  retry: () => void;
}) {
  useEffect(() => {
    console.error(error);
  }, [error]);

  return (
    <div className="max-w-2xl mx-auto px-6 py-16">
      <ErrorState
        title="DASHBOARD ERROR"
        message={
          error.digest
            ? `This view failed to render. Reference ${error.digest} when reporting this. Retrying keeps you in the dashboard.`
            : "This view failed to render. Retrying keeps you in the dashboard."
        }
        onRetry={retry}
      />
      <div className="mt-6 text-center">
        <Link
          href="/app"
          className="font-mono text-[11px] tracking-[0.16em] text-text-muted hover:text-text transition-colors"
        >
          BACK TO OVERVIEW
        </Link>
      </div>
    </div>
  );
}
