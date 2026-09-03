package network

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"shift.dev/shift/internal/model"
)

// Coordinator owns the network life of every restored workload on a machine:
// the NAT reservations that keep workloads off each other's ports, the TCP
// forwarders that publish non-direct mappings, and the status documents that
// tell applications what actually happened to their connections. It is the
// single component the restore path needs; the migration orchestrator only
// needs PlanFor.
type Coordinator struct {
	table  *NATTable
	logger *slog.Logger

	mu         sync.Mutex
	forwarders map[string][]*Forwarder
}

// DefaultDrainGrace is how long drained connections get to finish before the
// remaining ones are closed. It is deliberately generous: a workload under
// drain policy asked for its connections to complete, and cutting them off
// early would silently turn a graceful drain into a reset.
const DefaultDrainGrace = 15 * time.Second

// NewCoordinator creates a coordinator. The logger may be nil.
func NewCoordinator(logger *slog.Logger) *Coordinator {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Coordinator{
		table:      NewNATTable(),
		logger:     logger,
		forwarders: make(map[string][]*Forwarder),
	}
}

// Prepare validates and reserves the workload's network resources without
// starting anything. It runs at VALIDATE time, on the destination, so a port
// conflict fails the migration before a checkpoint is taken rather than after
// the transfer. If it returns an error the table is untouched.
func (coordinator *Coordinator) Prepare(workloadID string, mappings []model.PortMapping) error {
	return coordinator.table.Reserve(workloadID, mappings)
}

// Activate starts the forwarders for every non-direct mapping of the workload.
// It is called after the restored process has been resumed and health-checked:
// by then the process's own listeners are up, so a forwarder that cannot reach
// its upstream is a real fault, not a startup race. On failure everything this
// call started is stopped again and the reservations are released, leaving the
// machine as it was.
func (coordinator *Coordinator) Activate(ctx context.Context, workloadID string, mappings []model.PortMapping) error {
	coordinator.mu.Lock()
	if _, exists := coordinator.forwarders[workloadID]; exists {
		coordinator.mu.Unlock()
		return fmt.Errorf("workload %s already has active network forwarding", workloadID)
	}
	pending := make([]*Forwarder, 0, len(mappings))
	coordinator.mu.Unlock()

	for _, mapping := range mappings {
		if mapping.Direct() {
			continue
		}
		forwarder := NewForwarder(mapping, coordinator.logger)
		if err := forwarder.Start(ctx); err != nil {
			for _, started := range pending {
				started.Stop()
			}
			return fmt.Errorf("workload %s: %w", workloadID, err)
		}
		pending = append(pending, forwarder)
	}
	if len(pending) == 0 {
		return nil
	}

	coordinator.mu.Lock()
	if _, exists := coordinator.forwarders[workloadID]; exists {
		// Another actor activated the same workload between the two
		// locks. Undo ours; the first activation stands.
		coordinator.mu.Unlock()
		for _, started := range pending {
			started.Stop()
		}
		return fmt.Errorf("workload %s already has active network forwarding", workloadID)
	}
	coordinator.forwarders[workloadID] = pending
	coordinator.mu.Unlock()
	coordinator.table.SetForwarded(workloadID, true)

	for _, forwarder := range pending {
		coordinator.logger.Info("forwarding workload port",
			slog.String("workload", workloadID),
			slog.String("mapping", forwarder.Mapping().String()))
	}
	return nil
}

// Deactivate withdraws a workload's network presence: its forwarders stop
// accepting, established connections get the grace period to finish, anything
// still open is closed, and the port reservations are released. It returns the
// number of connections force-closed after the grace period. Deactivating an
// unknown workload is a no-op, because deleting a workload that never had
// network resources must not fail.
func (coordinator *Coordinator) Deactivate(workloadID string, grace time.Duration) int {
	coordinator.table.SetForwarded(workloadID, false)
	coordinator.mu.Lock()
	active := coordinator.forwarders[workloadID]
	delete(coordinator.forwarders, workloadID)
	coordinator.mu.Unlock()

	forceClosed := 0
	for _, forwarder := range active {
		forceClosed += forwarder.Drain(grace)
	}
	coordinator.table.Release(workloadID)
	if len(active) > 0 {
		coordinator.logger.Info("workload network withdrawn",
			slog.String("workload", workloadID),
			slog.Int("forced_connections", forceClosed))
	}
	return forceClosed
}

// DrainForwarders stops the workload's forwarders — no new connections are
// accepted, established ones get the grace period to finish — while keeping
// the port reservations, so the workload's published ports are not handed to
// another workload while it is being migrated. It returns the number of
// connections that were force-closed after the grace period. Draining an
// unknown workload is a no-op: a workload that never had forwarders has
// nothing to drain.
func (coordinator *Coordinator) DrainForwarders(workloadID string, grace time.Duration) int {
	coordinator.table.SetForwarded(workloadID, false)
	coordinator.mu.Lock()
	active := coordinator.forwarders[workloadID]
	delete(coordinator.forwarders, workloadID)
	coordinator.mu.Unlock()

	forceClosed := 0
	for _, forwarder := range active {
		forceClosed += forwarder.Drain(grace)
	}
	if len(active) > 0 {
		coordinator.logger.Info("workload forwarding drained",
			slog.String("workload", workloadID),
			slog.Int("forced_connections", forceClosed))
	}
	return forceClosed
}

// ForwardedPorts reports the host ports currently being forwarded for a
// workload.
func (coordinator *Coordinator) ForwardedPorts(workloadID string) []int {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	active := coordinator.forwarders[workloadID]
	ports := make([]int, 0, len(active))
	for _, forwarder := range active {
		ports = append(ports, forwarder.Mapping().HostPort)
	}
	return ports
}

// ActiveConnections reports how many connections are being carried for a
// workload, for status output and drain progress.
func (coordinator *Coordinator) ActiveConnections(workloadID string) int {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	total := 0
	for _, forwarder := range coordinator.forwarders[workloadID] {
		total += forwarder.ActiveConnections()
	}
	return total
}

// RecordStatus writes the status document a migrated application reads. The
// dispositions it records are the ones that actually hold: socketsPreserved
// says whether the restore carried TCP state, and every listener is recorded as
// recreated when it was not — regardless of what the plan hoped for. The
// document is written into the workload's own root, where the restored process
// can read it.
func (coordinator *Coordinator) RecordStatus(spec model.WorkloadSpec, plan model.NetworkPlan, operation, operationID, outcome, detail string, socketsPreserved bool) (string, error) {
	ports := make([]model.NetworkPortStatus, 0, len(plan.Ports))
	for _, port := range plan.Ports {
		disposition := port.Disposition
		if !socketsPreserved && disposition == model.SocketPreserved {
			disposition = model.SocketRecreated
		}
		ports = append(ports, model.NetworkPortStatus{Mapping: port.Mapping, Disposition: disposition, Mechanism: port.Mechanism})
	}
	document := StatusDocument{
		OperationID:        operationID,
		Operation:          operation,
		Outcome:            outcome,
		Policy:             string(plan.Policy),
		VirtualIP:          plan.Identity.IP,
		SocketsPreserved:   socketsPreserved,
		ConnectionsDropped: !socketsPreserved,
		Ports:              ports,
		ForwardedPorts:     coordinator.ForwardedPorts(spec.ID),
	}
	if detail != "" {
		document.Detail = detail
	}
	path, err := WriteStatus(spec.RootPath, document)
	if err != nil {
		return "", fmt.Errorf("record migration status for workload %s: %w", spec.ID, err)
	}
	coordinator.logger.Info("migration status recorded",
		slog.String("workload", spec.ID),
		slog.String("outcome", outcome),
		slog.Bool("sockets_preserved", socketsPreserved))
	return path, nil
}
