import { SiteHeader, SiteFooter } from "@/components/site-header";
import { Hero } from "@/components/home/hero";
import { Narrative } from "@/components/home/narrative";
import { WhatMoves, MoreThanMoving, Boundaries, FinalCTA } from "@/components/home/sections";
import type { Metadata } from "next";

export const metadata: Metadata = {
  title: "SHIFTGATE — Move computation between machines",
  description:
    "SHIFT captures running Linux computation as encrypted checkpoints — then moves it between machines, clones it into fleets, parks and resumes it, and fails it over to a warm standby.",
};

export default function HomePage() {
  return (
    <>
      <SiteHeader />
      <main>
        <Hero />
        <Narrative />
        <WhatMoves />
        <MoreThanMoving />
        <Boundaries />
        <FinalCTA />
      </main>
      <SiteFooter />
    </>
  );
}
