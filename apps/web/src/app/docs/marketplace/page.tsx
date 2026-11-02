import type { Metadata } from "next";
import { Container, Section } from "@/components/ui";

export const metadata: Metadata = {
  title: "Compute Marketplace",
  description: "Trade compute on the SHIFT marketplace — publish spare machine capacity, reserve capacity from other organizations, and migrate workloads onto it.",
};

export default function MarketplacePage() {
  return (
    <Section>
      <Container>
        <div className="mb-12">
          <p className="text-xs font-mono uppercase tracking-widest text-accent mb-3">
            Platform
          </p>
          <h1 className="text-3xl md:text-4xl font-bold tracking-tight mb-4">
            Compute marketplace
          </h1>
          <p className="text-text-secondary max-w-2xl text-lg leading-relaxed">
            Organizations with spare machines can publish capacity as offers;
            organizations that need capacity — a burst of clones, a bigger GPU
            box for a migration — reserve it, land workloads on it, and release
            it when done. Everything trades inside the control plane you
            already use.
          </p>
        </div>
        {/* Selling */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Publishing capacity</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Publish a machine&apos;s spare capacity with the CPU cores, memory,
            and optional GPU it offers, plus where it is and who may see it.
            Withdraw the offer any time; reservations in flight run to
            completion.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`# what can this organization offer?
shiftgate marketplace inventory

# publish spare capacity on one machine
shiftgate marketplace publish --machine workstation \\
  --cpu 8 --memory 32GiB --region eu-central \\
  --country DE --visibility organization

# stop offering (destructive: prompts on a terminal, --yes in scripts)
shiftgate marketplace withdraw --offer OFFER_ID --yes`}</code>
            </pre>
          </div>
        </div>

        {/* Buying */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Placing workloads</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            Buy side: browse open offers, match a workload&apos;s requirements
            against them, reserve, and migrate the workload onto the reserved
            machine. The migration itself is an ordinary SHIFT migration —
            the reservation just guarantees the destination.
          </p>
          <div className="rounded-lg border border-border bg-bg-elevated p-5">
            <pre className="text-sm">
              <code className="text-accent">{`# open offers and matching placements
shiftgate marketplace offers
shiftgate marketplace place --workload demo \\
  --cpu 4 --memory 8GiB --checkpoint CHECKPOINT_ID

# hold the destination and migrate
shiftgate marketplace reserve --offer OFFER_ID \\
  --workload demo --cpu 4 --memory 8GiB

# then: commit once landed, or release / fail
shiftgate marketplace commit --reservation RESERVATION_ID`}</code>
            </pre>
          </div>
        </div>

        {/* Lifecycle */}
        <div className="mb-12">
          <h2 className="text-xl font-semibold mb-4">Reservation lifecycle</h2>
          <p className="text-sm text-text-secondary leading-relaxed mb-4">
            A reservation holds offered capacity for one workload while the
            migration runs — created server-side inside the store&apos;s locked
            transaction, so a hold can never be granted on terms a placement
            would refuse. It is committed when the workload landed cleanly,
            released to return the capacity, or failed with a reason — the
            seller sees every transition. A hold expires or is released rather
            than pinning capacity forever.
          </p>
          <div className="grid gap-3 sm:grid-cols-3">
            {[
              { label: "reserve", detail: "Hold capacity from an open offer for a named workload with its CPU/memory requirements." },
              { label: "commit", detail: "The migration landed; the reservation becomes a settled placement." },
              { label: "release / fail", detail: "Return the capacity, or mark the placement failed with a reason the seller sees." },
            ].map((step) => (
              <div key={step.label} className="rounded-lg border border-border bg-bg-elevated p-4">
                <p className="text-sm font-semibold font-mono mb-1">{step.label}</p>
                <p className="text-xs text-text-muted leading-relaxed">{step.detail}</p>
              </div>
            ))}
          </div>
        </div>
      </Container>
    </Section>
  );
}
