import type { Metadata } from "next";
import { SiteHeader, SiteFooter } from "@/components/site-header";
import { LandingHero, ProofStrip } from "@/components/home/landing";
import {
  BoundaryBand,
  CommitGate,
  FinalCTA,
  Primitives,
  StateInventory,
  UseCases,
} from "@/components/home/sections";
import { MercuryProgress } from "@/components/motion/liquid";

export const metadata: Metadata = {
  title: "CI without cold starts, on hardware you own",
  description:
    "One warm checkpoint becomes N workers in seconds, and any running Linux workload moves between machines you own. State moves — and a failed move never destroys the source.",
};

export default function HomePage() {
  return (
    <>
      <MercuryProgress />
      <SiteHeader />
      <main>
        <LandingHero />
        <ProofStrip />
        <StateInventory />
        <CommitGate />
        <UseCases />
        <BoundaryBand />
        <Primitives />
        <FinalCTA />
      </main>
      <SiteFooter />
    </>
  );
}
