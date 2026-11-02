// Package network implements the SHIFT-layer network abstraction: the virtual
// network identity a workload keeps across machines, the port mapping and NAT
// table that exposes a workload's listeners on host ports, the TCP forwarder
// that carries that traffic, and the draining and reconnection contract that
// applies when a socket cannot follow the process.
//
// SHIFT is explicit about what it can and cannot move. A process checkpointed
// with TCP state capture keeps its kernel socket state on machines whose
// kernel accepts it; everything else — the ports it listens on, the address it
// is reachable at, the connections it had — is re-established by this package
// at the SHIFT layer, and the workload is told so through a status document
// written into its root. Nothing here claims transparent socket migration.
package network

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"

	"shift.dev/shift/internal/model"
)

// virtualNetworkPrefix is the address space virtual identities are drawn from:
// 100.64.0.0/10, the RFC 6598 carrier-grade NAT range. These addresses are
// identifiers, not routes — no interface is configured with them — so they can
// never collide with a machine's real networks the way RFC 1918 addresses
// routinely do. The literal cannot fail to parse; a failure leaves the prefix
// nil and every derivation reports the error instead of panicking.
var virtualNetworkPrefix = func() *net.IPNet {
	_, network, _ := net.ParseCIDR("100.64.0.0/10")
	return network
}()

// IdentityFor derives the virtual identity of a workload. The derivation is a
// hash, not a registry: it is stable across machines and restarts, so every
// machine that restores the same workload computes the same address without
// consulting a coordinator, and a forked workload gets its own distinct one.
func IdentityFor(workloadID string) (model.VirtualIdentity, error) {
	trimmed := strings.TrimSpace(workloadID)
	if trimmed == "" {
		return model.VirtualIdentity{}, fmt.Errorf("network identity requires a workload id")
	}
	if virtualNetworkPrefix == nil {
		return model.VirtualIdentity{}, errors.New("virtual network prefix is unavailable")
	}
	sum := sha256.Sum256([]byte("shift.dev/network-identity/v1\n" + trimmed))
	base := virtualNetworkPrefix.IP.Mask(virtualNetworkPrefix.Mask)
	ones, bits := virtualNetworkPrefix.Mask.Size()
	// The host part of the address is the first bits of the digest mapped
	// onto the available host bits — 22 of them, the RFC 6598 prefix being
	// what it is. Two workloads collide when their ids hash to the same
	// host bits: unlikely at handfuls of workloads, but the birthday bound
	// makes a pairwise collision likely around a few thousand ids, not
	// "vanishingly rare". The address is an identifier, not a route — no
	// interface is configured with it and nothing keys on it yet — but
	// widen the derivation (or record detected collisions) before anything
	// starts routing on these addresses.
	hostBits := uint32(bits - ones)
	host := binary.BigEndian.Uint32(sum[:4]) & (1<<hostBits - 1)
	address := make(net.IP, len(base))
	copy(address, base)
	binary.BigEndian.PutUint32(address[len(address)-4:], binary.BigEndian.Uint32(address[len(address)-4:])+host)
	identity := model.VirtualIdentity{IP: address.String(), PrefixLength: ones}
	if err := identity.Validate(); err != nil {
		// The derivation is arithmetic on a fixed range; failing here
		// means the code above is wrong, not the input.
		return model.VirtualIdentity{}, fmt.Errorf("derived identity is invalid: %w", err)
	}
	return identity, nil
}

// IdentityDigest is a short stable name for an identity, for logs and events.
// It never includes workload ids or other identifying material beyond the
// address itself.
func IdentityDigest(identity model.VirtualIdentity) string {
	sum := sha256.Sum256([]byte(identity.IP))
	return hex.EncodeToString(sum[:8])
}
