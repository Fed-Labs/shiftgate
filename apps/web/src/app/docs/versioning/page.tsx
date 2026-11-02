import type { Metadata } from "next";
import Link from "next/link";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Versioning and Stability",
  description: "What the pre-1.0 label promises — semver policy, state-format compatibility across versions, the N-1 upgrade window, and deprecations.",
};

export default function VersioningPage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Stability
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Versioning and stability promise
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Current release:{" "}
            <code className="font-mono text-sm bg-bg-surface px-1.5 py-0.5 rounded">v0.2.0 · pre-1.0</code>.
            This page is the promise that label stands behind: what is stable
            today, what may change before 1.0, how on-disk state stays
            compatible, and how long a release is supported.
          </p>
        </div>

        {/* What pre-1.0 means */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">What pre-1.0 means</h2>
          <ul className="text-sm text-text-secondary space-y-1.5">
            <li>• The product is past prototype but before its 1.0 stability freeze: usable and tested, but interfaces may still change.</li>
            <li>• Releases are <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">vMAJOR.MINOR.PATCH</code>; agent, CLI, control plane, and desktop client ship together in one repository release and report one version.</li>
          </ul>
        </div>

        {/* Semver */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Semver policy before 1.0</h2>
          <div className="rounded-lg border border-border bg-bg-elevated overflow-hidden">
            <table className="w-full text-sm">
              <tbody className="divide-y divide-border-subtle">
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent w-40">PATCH</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">Compatible fixes only. Safe to upgrade.</td>
                </tr>
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent">MINOR</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">May contain breaking changes to the HTTP API contract, the CLI surface, and configuration schemas. Breaking changes are called out in the release notes; nothing is removed silently.</td>
                </tr>
                <tr>
                  <td className="px-5 py-3 font-mono text-xs text-accent">MAJOR at 1.0</td>
                  <td className="px-5 py-3 text-xs text-text-secondary">The API contract and CLI become a stable surface; breaking changes then require a major bump and follow the deprecation window below.</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p className="text-sm text-text-muted leading-relaxed mt-4">
            A pre-1.0 MINOR bump is the declared breaking boundary. Within a
            MINOR line nothing breaks; across a MINOR line, read the release
            notes before upgrading automation.
          </p>
        </div>

        {/* State format */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">State-format compatibility</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            On-disk checkpoint state carries an explicit format and version
            (the current line is{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shift-state 1.x</code>,
            written as{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">1.0.0</code>).
            Compatibility is range-based within the format&apos;s major line:
          </p>
          <ul className="text-sm text-text-secondary space-y-1.5">
            <li>• Every <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">1.x</code> checkpoint is restorable by every <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">1.x</code> agent at or above the minor version that wrote it — a mixed-version fleet on the 1.x line restores each other&apos;s checkpoints.</li>
            <li>• A checkpoint written by a newer minor than the restoring agent is refused with <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">STATE_FORMAT_UPGRADE_REQUIRED</code> naming the version to upgrade to — never guessed at, never partially restored.</li>
            <li>• State older than the configured floor is refused with <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">STATE_FORMAT_UNSUPPORTED</code>. A different major line is always refused.</li>
            <li>• The same range rule covers the chunk codecs in flight on the line (zstd now, gzip for older chunks, identified from the frame&apos;s magic bytes), so rolling upgrades keep old checkpoints restorable.</li>
          </ul>
          <div className="mt-4 rounded-lg border border-accent/20 bg-accent/5 p-5">
            <p className="text-sm text-accent font-medium mb-1">
              Upgrade destinations before sources
            </p>
            <p className="text-sm text-text-secondary leading-relaxed">
              A newer source can write state an older destination cannot read.
              Bring a fleet to one version before cutting the next, and run{" "}
              <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate doctor</code>{" "}
              on every machine between release trains.
            </p>
          </div>
        </div>

        {/* N-1 */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">The N-1 upgrade window</h2>
          <p className="text-sm text-text-secondary leading-relaxed">
            The release-level promise is N-1: a release restores state written by
            the immediately previous release (and its own).{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">vN</code>{" "}
            agents interoperate with{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">vN-1</code>{" "}
            agents; vN-2 is out of the window and must be upgraded before mixed
            operation. The update system stages, verifies, and swaps binaries at
            safe points, keeps three backups, and supports{" "}
            <code className="font-mono text-xs bg-bg-surface px-1.5 py-0.5 rounded">shiftgate update rollback</code>{" "}
            plus a block list — an upgrade window is a rollback window too.
          </p>
        </div>

        {/* Deprecation */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Deprecation policy</h2>
          <ul className="text-sm text-text-secondary space-y-1.5">
            <li>• Anything documented as deprecated keeps working for at least the N-1 window (one release) before removal, and its replacement ships in the same release that deprecates it.</li>
            <li>• Deprecations and removals are announced in the release notes of the release that starts them. Pre-1.0, a breaking change may land in a MINOR bump, but it is still announced, never silent.</li>
            <li>• The on-disk state format is held to a stricter bar than the API: state compatibility across the 1.x line is guaranteed as described above, so a CLI or API deprecation can never brick existing checkpoints.</li>
          </ul>
        </div>

        {/* Not promised */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">What is not promised yet</h2>
          <ul className="text-sm text-text-secondary space-y-1.5">
            <li>• API/CLI stability before 1.0 (that is what pre-1.0 is for).</li>
            <li>• Cross-major state-format compatibility (2.x restoring 1.x) — undecided until there is a 2.x; the range rule within a major line is the promise.</li>
            <li>• Support response times — there is no published SLA outside a signed agreement.</li>
          </ul>
        </div>

        {/* Changelog */}
        <div className="rounded-lg border border-border bg-bg-elevated p-6">
          <h3 className="text-sm font-semibold mb-2">What&apos;s new</h3>
          <p className="text-sm text-text-secondary mb-4">
            Release-by-release history lives in the changelog: what each version
            added, changed, and fixed.
          </p>
          <Link
            href="/changelog"
            className="inline-flex items-center gap-2 text-sm font-mono text-accent hover:underline"
          >
            changelog →
          </Link>
        </div>
      </Container>
    </Section>
  );
}
