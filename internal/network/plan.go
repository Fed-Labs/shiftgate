package network

import (
	"fmt"
	"strings"

	"shift.dev/shift/internal/model"
)

// PlanFor computes the network plan for a migrating workload. It fails on any
// unportable port declaration — an unsupported protocol or a duplicate host
// port is a failed plan, not a degraded one, because a silently half-connected
// workload is a false success state.
func PlanFor(spec model.WorkloadSpec) (model.NetworkPlan, error) {
	identity, err := IdentityFor(spec.ID)
	if err != nil {
		return model.NetworkPlan{}, err
	}
	mappings, err := MappingsFor(spec)
	if err != nil {
		return model.NetworkPlan{}, err
	}
	policy := spec.NetworkPolicy
	if policy == "" {
		policy = model.NetworkReconnect
	}
	switch policy {
	case model.NetworkPreserve, model.NetworkReconnect, model.NetworkDrain:
	default:
		return model.NetworkPlan{}, fmt.Errorf("unknown network policy %q", string(policy))
	}

	plan := model.NetworkPlan{
		Policy: policy, Identity: identity,
		SocketsCarried: policy == model.NetworkPreserve,
		Ports:          make([]model.NetworkPortStatus, 0, len(mappings)),
	}
	for _, mapping := range mappings {
		disposition := model.SocketRecreated
		if policy == model.NetworkPreserve {
			disposition = model.SocketPreserved
		}
		mechanism := "direct"
		if !mapping.Direct() {
			mechanism = "forwarded"
		}
		plan.Ports = append(plan.Ports, model.NetworkPortStatus{Mapping: mapping, Disposition: disposition, Mechanism: mechanism})
	}
	plan.ConnectionsDropped = policy != model.NetworkPreserve
	plan.DrainBeforeCheckpoint = policy == model.NetworkDrain
	plan.Summary = SummarizePlan(plan)
	return plan, nil
}

// Summarize renders the plan in one sentence an operator can act on. It is a
// free function so a plan read back from a migration record can be summarized
// without recomputation.
func SummarizePlan(plan model.NetworkPlan) string {
	var parts []string
	switch plan.Policy {
	case model.NetworkPreserve:
		parts = append(parts, "TCP state travels with the checkpoint")
	case model.NetworkReconnect:
		parts = append(parts, "listeners are re-established and connections must reconnect")
	case model.NetworkDrain:
		parts = append(parts, "connections are drained before checkpoint and listeners re-established")
	default:
		parts = append(parts, fmt.Sprintf("unknown network policy %q", string(plan.Policy)))
	}
	if len(plan.Ports) == 0 {
		parts = append(parts, "no ports are declared")
	} else {
		labels := make([]string, 0, len(plan.Ports))
		for _, port := range plan.Ports {
			labels = append(labels, port.Mapping.String())
		}
		parts = append(parts, "ports "+strings.Join(labels, ", "))
	}
	parts = append(parts, fmt.Sprintf("virtual address %s follows the workload", plan.Identity.IP))
	return strings.Join(parts, "; ") + "."
}
