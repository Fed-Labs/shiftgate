"use client";

// Root error boundary (report 23): catches unexpected render errors on every
// route segment below the root layout — marketing pages, docs, and dashboard
// alike — instead of dropping users on Next's unbranded production crash page.
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
    <main className="flex min-h-[70vh] items-center justify-center px-6">
      <div className="w-full max-w-xl">
        <ErrorState
          title="UNEXPECTED ERROR"
          message={
            error.digest
              ? `The page failed to render. Reference ${error.digest} when reporting this.`
              : "The page failed to render. Retrying usually resolves a transient fault."
          }
          onRetry={retry}
        />
        <div className="mt-6 text-center">
          <Link
            href="/"
            className="font-mono text-[11px] tracking-[0.16em] text-text-muted hover:text-text transition-colors"
          >
            RETURN HOME
          </Link>
        </div>
      </div>
    </main>
  );
}
