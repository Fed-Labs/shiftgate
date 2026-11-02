import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Developers",
  description:
    "SHIFT is a CLI-first tool with a REST API underneath. Script it, automate it, integrate it — the shiftgate CLI, shell completion, and the documented control-plane API.",
};

export default function DevelopersLayout({ children }: { children: React.ReactNode }) {
  return children;
}
