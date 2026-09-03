package checkpoint

import (
	"context"
	"time"

	"shift.dev/shift/internal/model"
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
