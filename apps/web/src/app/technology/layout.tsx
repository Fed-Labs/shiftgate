import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Technology",
  description:
    "How SHIFT works: built on CRIU (Checkpoint/Restore In Userspace) with encrypted transport and verified state. The checkpoint/restore pipeline explained — no magic, just careful systems work.",
};

export default function TechnologyLayout({ children }: { children: React.ReactNode }) {
  return children;
}
