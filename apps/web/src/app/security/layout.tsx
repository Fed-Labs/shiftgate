import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "Security",
  description:
    "Encrypted by default, verified at every step. Machine identity via Ed25519 key pairs, encrypted checkpoint state, and explicit trust boundaries — every layer of SHIFT is designed around moving sensitive process data.",
};

export default function SecurityLayout({ children }: { children: React.ReactNode }) {
  return children;
}
