package checkpoint

import (
	"context"
	"fmt"
	"sort"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
)

// rollbackDrainGrace is how long forwarded connections are allowed to finish
// when a restore rolls back. The restored process is already being stopped, so
// this is short — long enough for in-flight requests to complete, not long
// enough to stall the rollback.
const rollbackDrainGrace = 3 * time.Second

// DestinationNetwork is the SHIFT-layer network of the machine a workload is
// being restored onto. The restorer drives it through one full transaction:
// host ports are reserved before any restore work begins, forwarders are
// activated only after the restored process is healthy, everything is drained
// and released if the restore rolls back, and the application is handed a
// status document that states what actually happened to its sockets.
type DestinationNetwork interface {
	// Prepare reserves the workload's host ports. It fails when another
	// workload on this machine already holds one of them, before any bytes
	// are extracted or processes restored.
	Prepare(workloadID string, mappings []model.PortMapping) error
	// Activate publishes the workload's listeners, starting forwarders for
	// every non-direct mapping. It runs after the restored process has
	// passed its health check.
	Activate(ctx context.Context, workloadID string, mappings []model.PortMapping) error
	// Deactivate withdraws the workload's network presence: forwarders
	// drain for the grace period, remaining connections are closed, and the
	// port reservations are released.
	Deactivate(workloadID string, grace time.Duration) int
	// RecordStatus writes the status document the restored application
	// reads, stating the true disposition of its sockets and listeners.
	RecordStatus(spec model.WorkloadSpec, plan model.NetworkPlan, operation, operationID, outcome, detail string, socketsPreserved bool) (string, error)
}

// SetNetwork attaches the machine's SHIFT-layer network to the checkpoint
// service. Without it, restores of workloads that declare ports are refused
// rather than silently restored without their listeners.
func (s *Service) SetNetwork(destinationNetwork DestinationNetwork) {
	s.network = destinationNetwork
}

// portGuardMappings is the set of host ports one upcoming restore can collide
// with. It is the union of the derived workload's own effective mappings —
// the host ports its restored listeners occupy (a fork or clone drops the
// published host port, so those default to the container ports) — and the
// source workload's published mappings, whose host ports a running source's
// reservations hold and whose container ports its processes bind. Reserving
// only the derived mapping misses the source entirely whenever the source
// publishes host != container ports; the union is what turns "the original
// still holds those ports" into an immediate conflict instead of an
// address-in-use failure inside the CRIU restore.
func portGuardMappings(source, derived model.WorkloadSpec) ([]model.PortMapping, error) {
	own, err := network.MappingsFor(derived)
	if err != nil {
		return nil, err
	}
	guard := make([]model.PortMapping, 0, len(own)+len(source.Ports))
	claimed := make(map[string]bool, len(own)+len(source.Ports))
	for _, mapping := range own {
		claimed[fmt.Sprintf("%s/%d", mapping.Protocol, mapping.HostPort)] = true
		guard = append(guard, mapping)
	}
	published, err := network.MappingsFor(source)
	if err != nil {
		return nil, err
	}
	for _, mapping := range published {
		key := fmt.Sprintf("%s/%d", mapping.Protocol, mapping.HostPort)
		if claimed[key] {
			continue
		}
		claimed[key] = true
		guard = append(guard, mapping)
	}
	sort.Slice(guard, func(i, j int) bool {
		if guard[i].HostPort != guard[j].HostPort {
			return guard[i].HostPort < guard[j].HostPort
		}
		return guard[i].Protocol < guard[j].Protocol
	})
	return guard, nil
}

// reserveRestorePorts claims every host port the restore about to run can
// collide with, before anything is copied. A workload that declares no ports
// needs nothing; one that does cannot be forked or cloned without a network
// layer to hold the claim. The claim is held for the workload's whole
// operation and released with it — rollback deactivates, and so does deleting
// the workload — so the reservation can never outlive the restore it guards.
func reserveRestorePorts(destination DestinationNetwork, workloadID, what string, source, derived model.WorkloadSpec) error {
	if len(derived.Ports) == 0 {
		return nil
	}
	if destination == nil {
		return fmt.Errorf("this agent has no network layer configured; a workload with declared ports cannot be %sed", what)
	}
	mappings, err := portGuardMappings(source, derived)
	if err != nil {
		return fmt.Errorf("resolve %s ports: %w", what, err)
	}
	if err := destination.Prepare(workloadID, mappings); err != nil {
		return fmt.Errorf("%s port conflict before anything was copied: %w", what, err)
	}
	return nil
}
