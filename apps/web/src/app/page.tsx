import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Hero } from "@/components/home/hero";
import { Narrative } from "@/components/home/narrative";
import { WhatMoves, Boundaries, FinalCTA } from "@/components/home/sections";
import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "SHIFTGATE — Move computation between machines",
  description:
    "SHIFT moves supported running computation between Linux machines with encrypted checkpoints, validated restore, and explicit compatibility boundaries.",
};

export default function HomePage() {
  return (
    <>
      <SiteHeader />
      <main>
        <Hero />
        <Narrative />
        <WhatMoves />
        <Boundaries />
        <FinalCTA />
      </main>
      <SiteFooter />
    </>
  );
}
