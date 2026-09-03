package network

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"shift.dev/shift/internal/model"
)

// ProtocolTCP is the only protocol SHIFT forwards today. UDP and Unix sockets
// are rejected with an explicit error rather than silently ignored, so a
// workload can never discover mid-migration that half of its listeners never
// came back.
const ProtocolTCP = "tcp"

// MappingsFor resolves a workload's port specifications into NAT mappings. A
// missing host port defaults to the container port: the restored process binds
// that port itself in the host network namespace, so forwarding it to itself
// would only collide with itself. Every mapping is validated — unknown
// protocols and out-of-range ports are errors, never silent drops.
func MappingsFor(spec model.WorkloadSpec) ([]model.PortMapping, error) {
	mappings := make([]model.PortMapping, 0, len(spec.Ports))
	for _, port := range spec.Ports {
		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = ProtocolTCP
		}
		if protocol != ProtocolTCP {
			return nil, fmt.Errorf("port %d: protocol %q is not supported for forwarding (only %s)", port.ContainerPort, port.Protocol, ProtocolTCP)
		}
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return nil, fmt.Errorf("container port %d is outside 1-65535", port.ContainerPort)
		}
		hostPort := port.HostPort
		if hostPort == 0 {
			hostPort = port.ContainerPort
		}
		if hostPort < 1 || hostPort > 65535 {
			return nil, fmt.Errorf("host port %d is outside 1-65535", hostPort)
		}
		mappings = append(mappings, model.PortMapping{Protocol: protocol, ContainerPort: port.ContainerPort, HostPort: hostPort})
	}
	seen := make(map[int]string, len(mappings))
	for _, mapping := range mappings {
		if previous, clash := seen[mapping.HostPort]; clash {
			return nil, fmt.Errorf("host port %d is mapped twice (%s and %s)", mapping.HostPort, previous, mapping.String())
		}
		seen[mapping.HostPort] = mapping.String()
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].HostPort < mappings[j].HostPort })
	return mappings, nil
}

// NATEntry is one row of the table: which workload owns a host port and how
// its traffic reaches the workload. Forwarded reports live state, not the
// mapping's shape — it stays false while the port is merely reserved and is
// true only while a forwarder is actually carrying it, so the table never
// claims traffic is flowing before it is.
type NATEntry struct {
	WorkloadID string            `json:"workload_id"`
	Mapping    model.PortMapping `json:"mapping"`
	Forwarded  bool              `json:"forwarded"`
}

// NATTable is the machine-local registry of host ports and the workloads they
// belong to. It exists so that two workloads restored on the same machine
// cannot silently steal each other's ports: every mapping is reserved before a
// restore begins, and the reservation is released only when the workload goes
// away. Ports a forwarder will bind are checked here first, before any socket
// is opened, so a plan fails at VALIDATE rather than mid-restore.
type NATTable struct {
	mu      sync.Mutex
	entries map[int]NATEntry
}

// NewNATTable returns an empty table.
func NewNATTable() *NATTable {
	return &NATTable{entries: make(map[int]NATEntry)}
}

// Reserve claims the host ports of the workload's mappings. It fails without
// modifying the table if any port is already taken by another workload.
// Re-reserving the exact same mappings for the same workload is a no-op — a
// restore that re-prepares an already-published workload must not fight
// itself — but the same workload asking for different ports is an error until
// the old reservation is released.
func (table *NATTable) Reserve(workloadID string, mappings []model.PortMapping) error {
	if strings.TrimSpace(workloadID) == "" {
		return fmt.Errorf("NAT reservation requires a workload id")
	}
	if len(mappings) == 0 {
		return nil
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	for _, mapping := range mappings {
		existing, taken := table.entries[mapping.HostPort]
		if !taken {
			continue
		}
		if existing.WorkloadID != workloadID {
			return fmt.Errorf("host port %d is already reserved by workload %s", mapping.HostPort, existing.WorkloadID)
		}
		if existing.Mapping != mapping {
			return fmt.Errorf("host port %d is already reserved by workload %s with a different mapping", mapping.HostPort, existing.WorkloadID)
		}
	}
	for _, mapping := range mappings {
		// A reservation is not a forwarder: Forwarded flips when the
		// coordinator activates, so a reserved port is never reported as
		// carrying traffic before a forwarder exists.
		table.entries[mapping.HostPort] = NATEntry{WorkloadID: workloadID, Mapping: mapping}
	}
	return nil
}

// Release drops every reservation held by the workload. Ports it did not hold
// are left alone.
func (table *NATTable) Release(workloadID string) {
	table.mu.Lock()
	defer table.mu.Unlock()
	for hostPort, entry := range table.entries {
		if entry.WorkloadID == workloadID {
			delete(table.entries, hostPort)
		}
	}
}

// SetForwarded flips the live-forwarding flag on the workload's non-direct
// reservations. Direct mappings never carry a forwarder — the process binds
// them itself — so they stay false.
func (table *NATTable) SetForwarded(workloadID string, forwarded bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	for hostPort, entry := range table.entries {
		if entry.WorkloadID == workloadID && !entry.Mapping.Direct() {
			entry.Forwarded = forwarded
			table.entries[hostPort] = entry
		}
	}
}

// Lookup returns the workload that owns a host port.
func (table *NATTable) Lookup(hostPort int) (NATEntry, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	entry, ok := table.entries[hostPort]
	return entry, ok
}

// List returns every reservation sorted by host port, for status output and
// diagnostics.
func (table *NATTable) List() []NATEntry {
	table.mu.Lock()
	defer table.mu.Unlock()
	entries := make([]NATEntry, 0, len(table.entries))
	for _, entry := range table.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Mapping.HostPort < entries[j].Mapping.HostPort })
	return entries
}
