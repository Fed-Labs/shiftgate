"use client";

// Last-resort boundary for errors thrown inside the root layout itself. It
// replaces the whole document, so it must ship its own <html>/<body> and
// cannot rely on globals.css, next/font, or providers being present.
import Link from "next/link";

export default function GlobalError({
  error,
  retry,
}: {
  error: Error & { digest?: string };
  retry: () => void;
}) {
  return (
    <html lang="en">
      <body
        style={{
          margin: 0,
          minHeight: "100vh",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          background: "#0a0a0b",
          color: "#e9e7e2",
          fontFamily: "ui-sans-serif, system-ui, sans-serif",
        }}
      >
        <div style={{ textAlign: "center", padding: "24px", maxWidth: "480px" }}>
          <p
            style={{
              fontFamily: "ui-monospace, monospace",
              fontSize: "12px",
              letterSpacing: "0.16em",
              color: "#e05252",
              marginBottom: "12px",
            }}
          >
            FATAL ERROR
          </p>
          <h1 style={{ fontSize: "22px", fontWeight: 600, margin: "0 0 12px" }}>
            SHIFT could not render this page.
          </h1>
          <p
            style={{
              fontSize: "14px",
              lineHeight: 1.6,
              color: "#9c9c9f",
              margin: "0 0 24px",
            }}
          >
            {error.digest
              ? `Reference ${error.digest} when reporting this.`
              : "An unexpected error escaped the application shell."}
          </p>
          <button
            type="button"
            onClick={() => retry()}
            style={{
              fontFamily: "ui-monospace, monospace",
              fontSize: "11px",
              letterSpacing: "0.16em",
              color: "#14e0b0",
              background: "transparent",
              border: "1px solid rgba(20, 224, 176, 0.4)",
              padding: "10px 20px",
              cursor: "pointer",
            }}
          >
            RETRY
          </button>
          <p style={{ marginTop: "20px" }}>
            <Link
              href="/"
              style={{
                fontFamily: "ui-monospace, monospace",
                fontSize: "11px",
                letterSpacing: "0.16em",
                color: "#63636a",
                textDecoration: "none",
              }}
            >
              RETURN HOME
            </Link>
          </p>
        </div>
      </body>
    </html>
  );
}
