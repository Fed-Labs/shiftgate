package migration

import (
	"context"
	"fmt"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
)

// SourceNetwork is the SHIFT-layer network of the machine a workload is
// migrating away from. The orchestrator drains forwarded connections before
// checkpointing a drain-policy workload, and brings the forwarders back if the
// migration fails and the source keeps running. It is optional: without it the
// migration proceeds and the drain policy's honest fallback applies — a
// checkpoint of a process still holding established TCP connections fails
// rather than pretending the connections were carried.
type SourceNetwork interface {
	// DrainForwarders stops the workload's published listeners and waits up
	// to grace for established connections to finish, returning how many
	// were force-closed after the grace period.
	DrainForwarders(workloadID string, grace time.Duration) int
	// Activate republishes the workload's listeners, restarting forwarders
	// for every non-direct mapping.
	Activate(ctx context.Context, workloadID string, mappings []model.PortMapping) error
}

// SetSourceNetwork attaches the source machine's SHIFT-layer network. It must
// be called before any migration starts.
func (o *Orchestrator) SetSourceNetwork(sourceNetwork SourceNetwork) {
	o.sourceNetwork = sourceNetwork
}

// drainSourceForwarders runs the drain half of a drain-policy migration: the
// workload's published listeners stop accepting, and connections in flight get
// the grace period to complete before the process is frozen. Draining counts
// toward migration downtime because the workload is unreachable from the
// moment its forwarders stop.
func (o *Orchestrator) drainSourceForwarders(id, workloadID string, plan model.NetworkPlan) {
	if o.sourceNetwork == nil || !plan.DrainBeforeCheckpoint || len(plan.Ports) == 0 {
		return
	}
	forced := o.sourceNetwork.DrainForwarders(workloadID, network.DefaultDrainGrace)
	_ = o.update(id, func(value *model.Migration) error {
		appendEvent(value, fmt.Sprintf("drained source forwarding; %d connections closed after the grace period", forced), 0.12, 0, 0)
		return nil
	})
}

// restoreSourceNetwork brings the source machine's SHIFT-layer forwarding back
// after a migration attempt ended with the source workload preserved. Its
// failure is logged, never fatal: the workload itself is running again, and
// the migration record already says what could not be restored.
func (o *Orchestrator) restoreSourceNetwork(ctx context.Context, migration model.Migration) {
	if o.sourceNetwork == nil || !migration.Network.DrainBeforeCheckpoint || len(migration.Network.Ports) == 0 {
		return
	}
	mappings := make([]model.PortMapping, 0, len(migration.Network.Ports))
	for _, port := range migration.Network.Ports {
		mappings = append(mappings, port.Mapping)
	}
	if err := o.sourceNetwork.Activate(ctx, migration.WorkloadID, mappings); err != nil {
		o.logger.Warn("could not restore source port forwarding",
			"migration_id", migration.ID, "error", err)
	}
}
