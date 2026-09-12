"use client";

import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/lib/store";
import { api } from "@/lib/api";
import { Badge, Card, EmptyState, Mono } from "@/components/ui";
import { formatBytes } from "@/lib/utils";
import type { ComputeOffer, ComputeReservation, ComputeResources } from "@/lib/types";

// resourceSummary names the capacity an offer exposes or a reservation holds,
// one part per resource class that is actually present.
function resourceSummary(resources: ComputeResources): string[] {
  const parts: string[] = [];
  if (resources.cpu_count) parts.push(`${resources.cpu_count} CPU`);
  if (resources.memory_bytes) parts.push(formatBytes(resources.memory_bytes, 0));
  if (resources.storage_bytes) parts.push(`${formatBytes(resources.storage_bytes, 0)} disk`);
  if (resources.network_mbps) parts.push(`${resources.network_mbps} Mbps`);
  if (resources.gpus?.length) parts.push(`${resources.gpus.length} GPU`);
  return parts.length > 0 ? parts : ["—"];
}

// priceLabel renders an offer's pricing honestly: every rate at zero is free
// (the case for a machine exposed to its own organization), otherwise the
// per-CPU-hour rate in whole currency units.
function priceLabel(offer: ComputeOffer): string {
  const pricing = offer.pricing;
  const rates = [
    pricing.cpu_hour_micros,
    pricing.memory_gib_hour_micros,
    pricing.storage_gib_hour_micros,
    pricing.gpu_hour_micros,
    pricing.egress_gib_micros,
    pricing.minimum_charge_micros,
  ];
  if (rates.every((rate) => !rate)) return "free";
  const currency = (pricing.currency || "USD").toUpperCase();
  return `${((pricing.cpu_hour_micros ?? 0) / 1_000_000).toFixed(2)} ${currency}/CPU·hr`;
}

function OfferCard({ offer }: { offer: ComputeOffer }) {
  const withdrawn = !!offer.withdrawn_at || offer.availability.status === "withdrawn";
  return (
    <Card className="p-5">
      <div className="flex items-start justify-between mb-3">
        <div className="min-w-0">
          <p className="font-medium text-text truncate">
            {(offer.machine_name || offer.machine_id).toUpperCase()}
          </p>
          <Mono className="text-[10px] text-text-muted mt-1 block truncate">{offer.machine_id}</Mono>
        </div>
        <Badge variant={withdrawn ? "error" : offer.availability.status === "available" ? "success" : "warning"}>
          {withdrawn ? "withdrawn" : offer.availability.status}
        </Badge>
      </div>
      <div className="flex flex-wrap gap-x-5 gap-y-1 mb-4">
        {resourceSummary(offer.available).map((part) => (
          <Mono key={part} className="text-xs text-text-secondary">{part}</Mono>
        ))}
      </div>
      <div className="flex items-center justify-between gap-4">
        <Mono className="text-[10px] text-text-muted truncate">
          {offer.policy.visibility} · trust {offer.trust}
          {offer.identity_verified ? " (verified)" : ""}
          {offer.geography?.region ? ` · ${offer.geography.region}` : ""}
        </Mono>
        <Mono className="text-xs text-accent shrink-0">{priceLabel(offer)}</Mono>
      </div>
    </Card>
  );
}

function ReservationRow({ reservation }: { reservation: ComputeReservation }) {
  return (
    <div className="flex items-center justify-between gap-4 p-4">
      <div className="min-w-0">
        <p className="text-sm text-text truncate">{reservation.machine_id}</p>
        <Mono className="text-[10px] text-text-muted mt-0.5 block truncate">
          {reservation.id}
          {reservation.workload_id ? ` · workload ${reservation.workload_id}` : ""}
          {reservation.migration_id ? ` · migration ${reservation.migration_id}` : ""}
          {reservation.error ? ` · ${reservation.error}` : ""}
        </Mono>
      </div>
      <div className="flex items-center gap-4 shrink-0">
        <Mono className="text-xs text-text-secondary hidden sm:block">
          {resourceSummary(reservation.requested).join(" · ")}
        </Mono>
        <Badge
          variant={
            reservation.state === "ACTIVE"
              ? "success"
              : reservation.state === "PENDING"
                ? "info"
                : reservation.state === "FAILED"
                  ? "error"
                  : "default"
          }
        >
          {reservation.state}
        </Badge>
      </div>
    </div>
  );
}

export default function ComputePage() {
  const orgId = useAuth((s) => s.organization?.id || "");

  const { data: inventory, isLoading } = useQuery({
    queryKey: ["compute-inventory", orgId],
    queryFn: () => api.computeInventory(orgId),
    enabled: !!orgId,
  });

  const { data: offers } = useQuery({
    queryKey: ["compute-offers", orgId],
    queryFn: () => api.listComputeOffers(orgId),
    enabled: !!orgId,
  });

  const { data: reservations } = useQuery({
    queryKey: ["compute-reservations", orgId],
    queryFn: () => api.listComputeReservations(orgId),
    enabled: !!orgId,
  });

  if (isLoading) {
    return (
      <div className="py-32 text-center">
        <Mono className="text-[11px] text-text-muted tracking-[0.2em]">READING INVENTORY…</Mono>
      </div>
    );
  }

  const activeReservations = (reservations ?? []).filter(
    (reservation) => reservation.state === "PENDING" || reservation.state === "ACTIVE"
  );

  return (
    <div className="max-w-6xl mx-auto px-5 md:px-10 py-10">
      <div className="flex items-baseline justify-between mb-10">
        <div>
          <span className="tlabel block mb-2">marketplace</span>
          <h1 className="font-display text-3xl font-medium text-text">Compute</h1>
        </div>
        <span className="font-mono text-[11px] text-text-muted">
          {inventory?.offers.length ?? 0} SCHEDULABLE
        </span>
      </div>

      <Card className="p-4 mb-10">
        <div className="flex items-center gap-3 flex-wrap">
          {inventory?.trading_enabled ? (
            <Badge variant="success">public trading enabled</Badge>
          ) : (
            <Badge variant="warning">public trading disabled</Badge>
          )}
          <p className="text-sm text-text-secondary">
            {inventory?.trading_enabled
              ? "Offers published here are visible to other organizations."
              : "This control plane schedules inside this organization only; no public offer is published or considered until an operator enables trading."}
          </p>
        </div>
      </Card>

      <h2 className="tlabel mb-4">Inventory</h2>
      {!inventory || inventory.offers.length === 0 ? (
        <EmptyState
          title="No offers to schedule against"
          description="Expose a machine's resources to make them schedulable for migrations."
        />
      ) : (
        <div className="grid md:grid-cols-2 gap-5 mb-10">
          {inventory.offers.map((offer) => (
            <OfferCard key={offer.id} offer={offer} />
          ))}
        </div>
      )}

      <h2 className="tlabel mb-4">Published offers</h2>
      {!offers || offers.length === 0 ? (
        <EmptyState
          title="No offers published"
          description="Publishing a machine's resources puts them in this organization's inventory."
        />
      ) : (
        <div className="grid md:grid-cols-2 gap-5 mb-10">
          {offers.map((offer) => (
            <OfferCard key={offer.id} offer={offer} />
          ))}
        </div>
      )}

      <h2 className="tlabel mb-4">Reservations</h2>
      {activeReservations.length === 0 ? (
        <EmptyState
          title="No active reservations"
          description="A migration reserves capacity on its destination machine for the duration of the move."
        />
      ) : (
        <Card className="divide-y divide-border mb-12">
          {activeReservations.map((reservation) => (
            <ReservationRow key={reservation.id} reservation={reservation} />
          ))}
        </Card>
      )}
    </div>
  );
}
