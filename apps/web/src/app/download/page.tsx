import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Download } from "lucide-react";
import { Container, Section, Card, Badge } from "@/components/ui";

export const metadata: Metadata = {
  title: "Download",
  description: "Build SHIFTGATE from source for supported Linux x86_64 hosts.",
};

export default function DownloadPage() {
  return (
    <>
      <SiteHeader />
      <main className="pt-14 flex-1">
        <Section>
          <Container narrow>
            <div className="relative text-center space-y-4 mb-12">
              <div className="absolute left-1/2 top-0 -z-10 h-[280px] w-[620px] max-w-full -translate-x-1/2 rounded-full bg-accent/10 blur-[120px]" aria-hidden />
              <h1 className="text-3xl md:text-4xl font-semibold tracking-tight">
                Build from source
              </h1>
              <p className="text-text-secondary text-lg">
                Build the CLI and agent on a supported Linux host.
              </p>
            </div>

            {/* Platform status */}
            <Card className="mb-8">
              <div className="flex items-start gap-3">
                <Download className="w-5 h-5 text-accent mt-0.5 shrink-0" />
                <div className="space-y-1 flex-1">
                  <div className="flex items-center gap-2 flex-wrap">
                    <span className="text-sm font-medium">
                      Supported platform: Linux x86_64
                    </span>
                    <Badge variant="success">Required</Badge>
                  </div>
                  <p className="text-sm text-text-secondary">
                    The agent rejects other operating systems and architectures instead of claiming checkpoint compatibility.
                  </p>
                </div>
              </div>
            </Card>

            {/* Build command */}
            <div className="space-y-4 mb-10">
              <h2 className="text-lg font-medium">Build</h2>
              <Card className="overflow-hidden border-accent/20 shadow-[0_30px_100px_-70px_rgba(0,212,170,0.5)]">
                <pre className="overflow-x-auto rounded-lg border border-border-subtle bg-bg-surface px-4 py-3.5 font-mono text-sm leading-7">
                  <code>{`git clone <repository-url> shift
cd shift
sudo ./install.sh
shift doctor`}</code>
                </pre>
              </Card>
            </div>

            {/* Prerequisites */}
            <div className="space-y-4 mb-10">
              <h2 className="text-lg font-medium">Prerequisites</h2>
              <Card>
                <div className="space-y-4">
                  <div className="flex items-center justify-between flex-wrap gap-3">
                    <div>
                      <div className="text-sm font-medium">
                        Go 1.24+, CRIU 4+, GNU tar
                      </div>
                      <div className="text-xs text-text-muted mt-0.5">
                        Linux x86_64 with checkpoint/restore kernel support
                      </div>
                    </div>
                  </div>
                </div>
              </Card>
            </div>

            {/* Compatibility */}
            <div className="space-y-4">
              <h2 className="text-lg font-medium">Compatibility</h2>
              <Card>
                <table className="w-full text-sm">
                  <thead>
                    <tr className="border-b border-border-subtle">
                      <th className="text-left py-2 text-text-muted font-normal text-xs uppercase tracking-wider">
                        Platform
                      </th>
                      <th className="text-left py-2 text-text-muted font-normal text-xs uppercase tracking-wider">
                        Status
                      </th>
                      <th className="text-left py-2 text-text-muted font-normal text-xs uppercase tracking-wider hidden sm:table-cell">
                        Notes
                      </th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-border-subtle">
                    <tr>
                      <td className="py-2.5">
                        <span className="font-medium">Linux x86_64</span>
                      </td>
                      <td className="py-2.5">
                        <Badge variant="success">Available</Badge>
                      </td>
                      <td className="py-2.5 text-text-secondary hidden sm:table-cell">
                        Modern kernel with CRIU-required features; `shift doctor` is authoritative
                      </td>
                    </tr>
                    <tr>
                      <td className="py-2.5">
                        <span className="font-medium">Linux aarch64</span>
                      </td>
                      <td className="py-2.5">
                        <Badge variant="info">Planned</Badge>
                      </td>
                      <td className="py-2.5 text-text-secondary hidden sm:table-cell">
                        Not implemented
                      </td>
                    </tr>
                    <tr>
                      <td className="py-2.5">
                        <span className="font-medium">macOS</span>
                      </td>
                      <td className="py-2.5">
                        <Badge variant="info">Planned</Badge>
                      </td>
                      <td className="py-2.5 text-text-secondary hidden sm:table-cell">
                        Not implemented
                      </td>
                    </tr>
                    <tr>
                      <td className="py-2.5">
                        <span className="font-medium">Windows</span>
                      </td>
                      <td className="py-2.5">
                        <Badge variant="info">Planned</Badge>
                      </td>
                      <td className="py-2.5 text-text-secondary hidden sm:table-cell">
                        Not implemented
                      </td>
                    </tr>
                  </tbody>
                </table>
              </Card>
            </div>
          </Container>
        </Section>
      </main>
      <SiteFooter />
    </>
  );
}
