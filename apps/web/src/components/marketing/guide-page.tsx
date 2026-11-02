import type { Metadata } from "next";
import Link from "next/link";
import { Container, Section } from "@/components/ui";

export type ContentBlock =
  | { kind: "p"; text: string }
  | { kind: "code"; body: string }
  | { kind: "bullets"; items: string[] }
  | { kind: "table"; head: string[]; rows: string[][] };

export type ContentSection = { heading: string; blocks: ContentBlock[] };

export type ContentPageProps = {
  eyebrow: string;
  title: string;
  lede: string;
  sections: ContentSection[];
  honestEdges: string[];
  faqs: { q: string; a: string }[];
  cta?: { heading: string; body: string };
  related?: { href: string; label: string }[];
};

export function contentMetadata(props: {
  title: string;
  description: string;
}): Metadata {
  return { title: props.title, description: props.description };
}

function Blocks({ blocks }: { blocks: ContentBlock[] }) {
  return (
    <div className="space-y-4">
      {blocks.map((block, i) => {
        if (block.kind === "p")
          return (
            <p key={i} className="text-sm text-text-secondary leading-relaxed">
              {block.text}
            </p>
          );
        if (block.kind === "code")
          return (
            <pre
              key={i}
              className="rounded-lg border border-border bg-bg-elevated p-4 text-sm text-accent overflow-x-auto"
            >
              <code>{block.body}</code>
            </pre>
          );
        if (block.kind === "bullets")
          return (
            <ul key={i} className="space-y-2 text-sm text-text-secondary list-disc pl-5">
              {block.items.map((item) => (
                <li key={item}>{item}</li>
              ))}
            </ul>
          );
        return (
          <div key={i} className="overflow-x-auto rounded-lg border border-border">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border bg-bg-elevated text-left">
                  {block.head.map((h) => (
                    <th key={h} className="px-4 py-3 font-medium text-text">
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {block.rows.map((row) => (
                  <tr key={row.join("|")} className="border-b border-border-subtle last:border-0">
                    {row.map((cell, ci) => (
                      <td key={ci} className="px-4 py-3 text-text-secondary align-top">
                        {cell}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        );
      })}
    </div>
  );
}

export function GuidePage(props: ContentPageProps) {
  const faqJsonLd = {
    "@context": "https://schema.org",
    "@type": "FAQPage",
    mainEntity: props.faqs.map(({ q, a }) => ({
      "@type": "Question",
      name: q,
      acceptedAnswer: { "@type": "Answer", text: a },
    })),
  };

  return (
    <Section>
      <Container>
        <div className="mb-12 max-w-3xl">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            {props.eyebrow}
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            {props.title}
          </h1>
          <p className="text-text-secondary text-lg leading-relaxed">{props.lede}</p>
        </div>

        <div className="space-y-12 max-w-3xl">
          {props.sections.map((section) => (
            <section key={section.heading}>
              <h2 className="text-xl font-semibold mb-4">{section.heading}</h2>
              <Blocks blocks={section.blocks} />
            </section>
          ))}

          <section className="rounded-lg border border-status-warning/40 bg-bg-elevated p-6">
            <h2 className="text-sm font-semibold mb-3">Honest edges</h2>
            <ul className="space-y-2 text-sm text-text-secondary list-disc pl-5">
              {props.honestEdges.map((edge) => (
                <li key={edge}>{edge}</li>
              ))}
            </ul>
          </section>

          {props.cta && (
            <section className="rounded-lg border border-border bg-bg-elevated p-6">
              <h2 className="text-base font-semibold mb-2">{props.cta.heading}</h2>
              <p className="text-sm text-text-secondary mb-4">{props.cta.body}</p>
              <div className="flex flex-wrap gap-3">
                <Link href="/download" className="inline-flex items-center rounded-md bg-accent px-4 py-2 text-sm font-medium text-bg hover:bg-accent-dim transition-colors">
                  Install SHIFT
                </Link>
                <Link href="/docs/ci-burst" className="inline-flex items-center rounded-md border border-border px-4 py-2 text-sm font-medium text-text hover:bg-bg-hover transition-colors">
                  Run the CI-burst proof
                </Link>
              </div>
            </section>
          )}

          <section>
            <h2 className="text-xl font-semibold mb-4">Frequently asked questions</h2>
            <div className="space-y-6">
              {props.faqs.map(({ q, a }) => (
                <div key={q}>
                  <h3 className="text-sm font-medium text-text mb-2">{q}</h3>
                  <p className="text-sm text-text-secondary leading-relaxed">{a}</p>
                </div>
              ))}
            </div>
          </section>

          {props.related && props.related.length > 0 && (
            <section>
              <h2 className="text-sm font-semibold mb-3">Related</h2>
              <ul className="space-y-2 text-sm">
                {props.related.map((link) => (
                  <li key={link.href}>
                    <Link href={link.href} className="text-text-secondary hover:text-accent transition-colors">
                      {link.label}
                    </Link>
                  </li>
                ))}
                <li>
                  <Link href="/guides/linux-workload-mobility" className="text-text-secondary hover:text-accent transition-colors">
                    The pillar guide: Linux workload mobility
                  </Link>
                </li>
                <li>
                  <Link href="/docs/quickstart" className="text-text-secondary hover:text-accent transition-colors">
                    The 5-minute quickstart
                  </Link>
                </li>
              </ul>
            </section>
          )}
        </div>

        <script
          type="application/ld+json"
          dangerouslySetInnerHTML={{
            // Escape `<` so FAQ text can never break out of the JSON-LD
            // script element (U+2028/2029 follow JSON.stringify's escaping).
            __html: JSON.stringify(faqJsonLd).replace(/</g, "\\u003c"),
          }}
        />
      </Container>
    </Section>
  );
}
