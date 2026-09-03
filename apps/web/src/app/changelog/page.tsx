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

const entries: ChangelogEntry[] = [];

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
