package network

import (
	"log/slog"
	"sync"

	"shift.dev/shift/internal/model"
)

// The NAT table is in-memory on purpose — it describes what this agent
// process has bound — but the workloads behind it survive the process: an
// agent restart leaves every detached workload running with its ports held,
// while the fresh table remembers none of them. Without a rebuild, the next
// restore on the machine is promised those ports at VALIDATE and fails only
// at bind time (or inside CRIU, for direct mappings): the exact mid-restore
// failure the reservation layer exists to prevent. RebuildReservations is the
// rebuild: the runtime layer calls it at manager open — right after it
// reconciles process state, and before the agent wires its coordinator — with
// the workloads whose port claims still hold.

var startupClaims struct {
	mu        sync.Mutex
	version   uint64
	workloads []model.Workload
}

// RebuildReservations records the port claims of the workloads that still
// hold host ports after an agent restart — processes Reconcile found alive,
// and checkpointed records whose ports belong to their next restore — so
// every coordinator constructed afterwards starts from the machine's real
// occupancy. Pass nil to record that nothing holds ports. Conflicts between
// claims surface when a coordinator adopts them, not here: the caller knows
// which record predates which.
func RebuildReservations(workloads []model.Workload) {
	startupClaims.mu.Lock()
	defer startupClaims.mu.Unlock()
	startupClaims.version++
	startupClaims.workloads = append([]model.Workload(nil), workloads...)
}

// syncStartupClaims adopts any startup claims this coordinator has not yet
// reserved. It runs at construction and before each Prepare, so the claims
// survive either wiring order in agent startup. Re-adopting a claim the
// table already holds is a no-op, and a claim that lost its port to an
// earlier reservation is reported and skipped — the earlier reservation is
// the one actually protecting the port.
func (coordinator *Coordinator) syncStartupClaims() {
	coordinator.claimsMu.Lock()
	defer coordinator.claimsMu.Unlock()
	startupClaims.mu.Lock()
	version := startupClaims.version
	workloads := startupClaims.workloads
	startupClaims.mu.Unlock()
	if version == coordinator.claimsVersion {
		return
	}
	coordinator.claimsVersion = version
	for _, workload := range workloads {
		if workload.Spec.ID == "" {
			continue
		}
		mappings, err := MappingsFor(workload.Spec)
		if err != nil || len(mappings) == 0 {
			continue
		}
		if err := coordinator.table.Reserve(workload.Spec.ID, mappings); err != nil {
			coordinator.logger.Warn("startup port claim not restored",
				slog.String("workload", workload.Spec.ID),
				slog.String("error", err.Error()))
		}
	}
}
