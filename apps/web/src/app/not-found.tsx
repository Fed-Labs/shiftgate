import Link from "next/link";

export default function NotFound() {
  return (
    <main className="flex min-h-[70vh] items-center justify-center px-6">
      <div className="text-center">
        <p className="font-mono text-[12px] tracking-[0.16em] text-accent mb-3">404</p>
        <h1 className="text-3xl font-bold tracking-tight mb-4">Page not found.</h1>
        <p className="text-text-secondary text-sm leading-relaxed max-w-md mx-auto mb-8">
          The route you requested does not exist on shiftgate.dev.
        </p>
        <Link
          href="/"
          className="font-mono text-[11px] tracking-[0.16em] text-accent border border-accent/40 px-5 py-2.5 hover:bg-accent hover:text-bg transition-colors"
        >
          RETURN HOME
        </Link>
      </div>
    </main>
  );
}
