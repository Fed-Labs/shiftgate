import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Container, Section, Card, Badge, Mono } from "@/components/ui";

export const metadata: Metadata = {
  title: "Changelog",
  description: "Version history and release notes for SHIFTGATE.",
};

interface ChangelogEntry {
  version: string;
  date: string;
  latest?: boolean;
  added?: string[];
  changed?: string[];
  fixed?: string[];
}

const entries: ChangelogEntry[] = [
  {
    version: "Unreleased",
    date: "2026-09-12",
    latest: true,
    added: [
      "Hosted checkpoint storage: per-organization encrypted mirroring with reconciled, quota-enforced usage; agents fetch short-lived scoped credentials from the control plane.",
      "Live migration: CRIU pre-copy runs before the authoritative final checkpoint (--mode live).",
      "Lazy restore: the process starts before its memory is fully loaded and pages stream in on demand through a userfaultfd-backed daemon (shiftgate restore --lazy).",
      "Warm-standby failover: failover policies replicate checkpoints to a standby agent that restores the newest copy when the source is confirmed gone (shiftgate workload failover, shiftgate standby).",
      "Clone sets: derive up to 128 independent running workloads from one checkpoint on the same machine, with reflink-copied filesystems and an all-or-nothing transaction (shiftgate clone).",
      "One-click migration from the dashboard: dispatch a migration from the workloads fleet or a workload's detail page.",
      "shiftgate.dev is the default control plane endpoint; --control-plane is optional.",
    ],
  },
  {
    version: "0.1.4",
    date: "2026-09-04",
    fixed: [
      "The shipped systemd unit refused CRIU's netlink socket and made workload roots read-only; the sandbox now allows AF_NETLINK and keeps only OS directories read-only.",
      "The CLI names the three real Unix-socket dial failures (agent not running, stale socket, group membership) instead of a bare errno.",
    ],
  },
  {
    version: "0.1.2",
    date: "2026-09-04",
    fixed: [
      "doctor shows why the CRIU check failed — criu's own reason, not just the version.",
      "CLI error prefix is 'shiftgate:'; the dist checksum sidecar works next to the download.",
    ],
  },
  {
    version: "0.1.1",
    date: "2026-09-04",
    changed: [
      "The CLI is 'shiftgate' everywhere — 'shift' is a POSIX shell builtin that shadowed the binary in every shell.",
      "The agent hands its Unix socket to the configured group so the CLI works unprivileged; installers create the group and enroll the user.",
    ],
  },
  {
    version: "0.1.0",
    date: "2026-09-03",
    added: [
      "Initial release: CRIU-based checkpoints with encrypted, deduplicated chunk storage; cold migration between machines; workload management; control plane with organizations, plans, and audit; web dashboard; mTLS peer transport.",
    ],
  },
];

function ChangelogGroup({
  label,
  items,
}: {
  label: string;
  items: string[];
}) {
  return (
    <div className="space-y-2">
      <h4 className="text-xs font-medium uppercase tracking-wider text-text-muted">
        {label}
      </h4>
      <ul className="space-y-1.5">
        {items.map((item) => (
          <li
            key={item}
            className="text-sm text-text-secondary leading-relaxed pl-4 relative before:content-[''] before:absolute before:left-0 before:top-[9px] before:w-1.5 before:h-1.5 before:rounded-full before:bg-border"
          >
            {item}
          </li>
        ))}
      </ul>
    </div>
  );
}

export default function ChangelogPage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <div className="text-center space-y-4 mb-12">
              <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                Changelog
              </h1>
              <p className="text-text-secondary text-lg">
                Version history and release notes.
              </p>
            </div>

            <div className="space-y-6">
              {entries.map((entry) => (
                <Card key={entry.version}>
                  <div className="flex items-center gap-3 mb-5 flex-wrap">
                    <Mono className="text-base font-medium">
                      v{entry.version}
                    </Mono>
                    {entry.latest && <Badge variant="accent">Latest</Badge>}
                    <span className="text-sm text-text-muted">
                      {entry.date}
                    </span>
                  </div>

                  <div className="space-y-5">
                    {entry.added && (
                      <ChangelogGroup label="Added" items={entry.added} />
                    )}
                    {entry.changed && (
                      <ChangelogGroup label="Changed" items={entry.changed} />
                    )}
                    {entry.fixed && (
                      <ChangelogGroup label="Fixed" items={entry.fixed} />
                    )}
                  </div>
                </Card>
              ))}
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
