import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Product",
  description:
    "SHIFT checkpoints running computation — processes, memory, file descriptors, network policy — within the limits reported by CRIU and device adapters, and restores it on a different machine.",
};

export default function ProductLayout({ children }: { children: React.ReactNode }) {
  return children;
}
