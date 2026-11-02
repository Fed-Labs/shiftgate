package network

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// freePort asks the kernel for an unused TCP port. Between the close and the
// test's own bind another process could claim it, which is the usual test
// flake source; callers retry on bind failure rather than asserting on the
// port itself.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), ProtocolTCP, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// echoListener is a real TCP server on the given port that answers every
// connection by echoing whatever it receives. The port is the container port
// a forwarder dials, so the echo is reachable through the forwarder.
func echoListener(t *testing.T, port int) (closer func()) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("start echo listener: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				io.Copy(connection, connection)
			}(connection)
		}
	}()
	return func() { listener.Close(); <-done }
}

func TestIdentityIsDeterministicAndInRange(t *testing.T) {
	first, err := IdentityFor("workload-alpha")
	if err != nil {
		t.Fatalf("derive identity: %v", err)
	}
	second, err := IdentityFor("workload-alpha")
	if err != nil {
		t.Fatalf("derive identity again: %v", err)
	}
	if first != second {
		t.Fatalf("identity must be stable: %v then %v", first, second)
	}
	other, err := IdentityFor("workload-beta")
	if err != nil {
		t.Fatalf("derive other identity: %v", err)
	}
	if other.IP == first.IP {
		t.Fatalf("distinct workloads must not share an address: %s", first.IP)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("derived identity is invalid: %v", err)
	}
	if first.PrefixLength != 10 {
		t.Fatalf("prefix length must be 10, got %d", first.PrefixLength)
	}
	if IdentityDigest(first) == "" {
		t.Fatal("digest must not be empty")
	}
}

func TestIdentityRejectsEmptyWorkloadID(t *testing.T) {
	if _, err := IdentityFor("   "); err == nil {
		t.Fatal("an empty workload id must not get an identity")
	}
}

func TestMappingsForValidatesAndDefaults(t *testing.T) {
	spec := model.WorkloadSpec{ID: "w1", Ports: []model.PortSpec{
		{Protocol: "TCP", ContainerPort: 8080},
		{Protocol: "", ContainerPort: 9090, HostPort: 19090},
	}}
	mappings, err := MappingsFor(spec)
	if err != nil {
		t.Fatalf("resolve mappings: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("expected 2 mappings, got %d", len(mappings))
	}
	if mappings[0].HostPort != mappings[0].ContainerPort || !mappings[0].Direct() {
		t.Fatalf("a missing host port must default to the container port: %+v", mappings[0])
	}
	if mappings[1].HostPort != 19090 || mappings[1].Direct() {
		t.Fatalf("explicit host port must be honored: %+v", mappings[1])
	}
	if mappings[1].Protocol != ProtocolTCP {
		t.Fatalf("protocol must normalize to tcp: %+v", mappings[1])
	}

	cases := []struct {
		name  string
		ports []model.PortSpec
	}{
		{"udp is refused", []model.PortSpec{{Protocol: "udp", ContainerPort: 53}}},
		{"zero port is refused", []model.PortSpec{{Protocol: "tcp", ContainerPort: 0}}},
		{"oversized port is refused", []model.PortSpec{{Protocol: "tcp", ContainerPort: 70000}}},
		{"oversized host port is refused", []model.PortSpec{{Protocol: "tcp", ContainerPort: 80, HostPort: 70000}}},
		{"duplicate host port is refused", []model.PortSpec{
			{Protocol: "tcp", ContainerPort: 80, HostPort: 8080},
			{Protocol: "tcp", ContainerPort: 81, HostPort: 8080},
		}},
	}
	for _, testCase := range cases {
		if _, err := MappingsFor(model.WorkloadSpec{ID: "w", Ports: testCase.ports}); err == nil {
			t.Fatalf("%s: expected an error", testCase.name)
		}
	}
}

func TestNATTableReservations(t *testing.T) {
	table := NewNATTable()
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 8080, HostPort: 18080}
	if err := table.Reserve("w1", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Re-reserving identical mappings for the same workload is a no-op.
	if err := table.Reserve("w1", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("idempotent reserve: %v", err)
	}
	// A different workload cannot take the port.
	if err := table.Reserve("w2", []model.PortMapping{mapping}); err == nil {
		t.Fatal("two workloads must not share a host port")
	}
	// Nor can the same workload change its mapping while holding the port.
	changed := mapping
	changed.ContainerPort = 8081
	if err := table.Reserve("w1", []model.PortMapping{changed}); err == nil {
		t.Fatal("a workload must not silently change its own mapping")
	}
	entry, ok := table.Lookup(18080)
	if !ok || entry.WorkloadID != "w1" || entry.Forwarded {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	table.Release("w2")
	if _, ok := table.Lookup(18080); !ok {
		t.Fatal("releasing another workload must not drop this one's ports")
	}
	table.Release("w1")
	if _, ok := table.Lookup(18080); ok {
		t.Fatal("release must free the port")
	}
	if err := table.Reserve("", []model.PortMapping{mapping}); err == nil {
		t.Fatal("a reservation requires a workload id")
	}
}

// startForwarder brings up a forwarder whose upstream is the container port,
// with the host port chosen by the kernel.
func startForwarder(t *testing.T, containerPort int) (*Forwarder, model.PortMapping) {
	t.Helper()
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: containerPort, HostPort: freePort(t)}
	forwarder := NewForwarder(mapping, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := forwarder.Start(context.Background()); err != nil {
		t.Fatalf("start forwarder: %v", err)
	}
	return forwarder, mapping
}

func TestForwarderCarriesTraffic(t *testing.T) {
	containerPort := freePort(t)
	closeUpstream := echoListener(t, containerPort)
	defer closeUpstream()
	forwarder, mapping := startForwarder(t, containerPort)
	defer forwarder.Stop()

	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)))
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer connection.Close()
	payload := []byte("shift-me-across-machines")
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write through forwarder: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("read echo through forwarder: %v", err)
	}
	if string(buffer) != string(payload) {
		t.Fatalf("traffic was altered: %q", buffer)
	}
	if forwarder.ActiveConnections() != 1 {
		t.Fatalf("the connection must be tracked, got %d", forwarder.ActiveConnections())
	}
}

func TestForwarderDrainClosesRemainingConnections(t *testing.T) {
	containerPort := freePort(t)
	closeUpstream := echoListener(t, containerPort)
	defer closeUpstream()
	forwarder, mapping := startForwarder(t, containerPort)

	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)))
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("held-open")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Consume the echo so the connection is genuinely idle when draining
	// starts; only then does force-closing it prove the drain worked.
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(connection, make([]byte, len("held-open"))); err != nil {
		t.Fatalf("read echo: %v", err)
	}

	forced := forwarder.Drain(50 * time.Millisecond)
	if forced != 1 {
		t.Fatalf("the held-open connection must be force-closed, got %d", forced)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("the connection must be closed after the grace period")
	}
	// After draining, the host port is free for the next user.
	probe, err := (&net.ListenConfig{}).Listen(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)))
	if err != nil {
		t.Fatalf("the drained port must be releasable: %v", err)
	}
	probe.Close()
}

func TestPlanForEachPolicy(t *testing.T) {
	base := model.WorkloadSpec{
		ID: "workload-plan", RootPath: "/tmp/workload-plan",
		Ports: []model.PortSpec{
			{Protocol: "tcp", ContainerPort: 8080},
			{Protocol: "tcp", ContainerPort: 9090, HostPort: 19090},
		},
	}
	cases := []struct {
		policy             model.NetworkPolicy
		socketsCarried     bool
		connectionsDropped bool
		drainBefore        bool
		disposition        model.SocketDisposition
	}{
		{model.NetworkPreserve, true, false, false, model.SocketPreserved},
		{model.NetworkReconnect, false, true, false, model.SocketRecreated},
		{model.NetworkDrain, false, true, true, model.SocketRecreated},
	}
	for _, testCase := range cases {
		spec := base
		spec.NetworkPolicy = testCase.policy
		plan, err := PlanFor(spec)
		if err != nil {
			t.Fatalf("%s: plan: %v", testCase.policy, err)
		}
		if plan.SocketsCarried != testCase.socketsCarried ||
			plan.ConnectionsDropped != testCase.connectionsDropped ||
			plan.DrainBeforeCheckpoint != testCase.drainBefore {
			t.Fatalf("%s: unexpected plan: %+v", testCase.policy, plan)
		}
		if len(plan.Ports) != 2 {
			t.Fatalf("%s: expected both ports planned, got %+v", testCase.policy, plan.Ports)
		}
		if plan.Ports[0].Disposition != testCase.disposition || plan.Ports[0].Mechanism != "direct" {
			t.Fatalf("%s: unexpected direct port: %+v", testCase.policy, plan.Ports[0])
		}
		if plan.Ports[1].Mechanism != "forwarded" {
			t.Fatalf("%s: unexpected forwarded port: %+v", testCase.policy, plan.Ports[1])
		}
		if err := plan.Identity.Validate(); err != nil {
			t.Fatalf("%s: plan identity invalid: %v", testCase.policy, err)
		}
		if plan.Summary == "" {
			t.Fatalf("%s: plan must summarize itself", testCase.policy)
		}
	}
	spec := base
	spec.NetworkPolicy = "CarrierPigeon"
	if _, err := PlanFor(spec); err == nil {
		t.Fatal("an unknown policy must fail the plan")
	}
	spec.NetworkPolicy = model.NetworkReconnect
	spec.Ports = append(spec.Ports, model.PortSpec{Protocol: "udp", ContainerPort: 53})
	if _, err := PlanFor(spec); err == nil {
		t.Fatal("an unforwardable protocol must fail the plan")
	}
}

func TestStatusDocumentRoundTrip(t *testing.T) {
	root := t.TempDir()
	document := StatusDocument{
		OperationID: "restore-1", Operation: OperationRestore, Outcome: StatusCompleted,
		Policy: string(model.NetworkReconnect), VirtualIP: "100.72.10.9",
		SocketsPreserved: false, ConnectionsDropped: true,
		Ports:          []model.NetworkPortStatus{{Mapping: model.PortMapping{Protocol: "tcp", ContainerPort: 80, HostPort: 8080}, Disposition: model.SocketRecreated, Mechanism: "forwarded"}},
		ForwardedPorts: []int{8080},
	}
	path, err := WriteStatus(root, document)
	if err != nil {
		t.Fatalf("write status: %v", err)
	}
	if filepath.Base(path) != StatusDocumentName {
		t.Fatalf("unexpected document path: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the status document is application-readable state, expected 0600, got %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(root, ".shift"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("no staged files may remain: %v", entries)
	}
	read, err := ReadStatus(root)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if read.OperationID != document.OperationID || read.Outcome != document.Outcome ||
		read.SocketsPreserved != document.SocketsPreserved || len(read.Ports) != 1 || read.ForwardedPorts[0] != 8080 {
		t.Fatalf("round trip lost information: %+v", read)
	}
	if read.RecordedAt.IsZero() || read.SchemaVersion != 1 {
		t.Fatalf("write must stamp the document: %+v", read)
	}

	invalid := document
	invalid.Operation = "teleport"
	if _, err := WriteStatus(root, invalid); err == nil {
		t.Fatal("an unknown operation must be refused")
	}
	invalid = document
	invalid.Outcome = "maybe"
	if _, err := WriteStatus(root, invalid); err == nil {
		t.Fatal("an unknown outcome must be refused")
	}
	invalid = document
	invalid.OperationID = "  "
	if _, err := WriteStatus(root, invalid); err == nil {
		t.Fatal("a missing operation id must be refused")
	}
}

func TestCoordinatorLifecyclePublishesDrainsAndRecords(t *testing.T) {
	upstreamPort := freePort(t)
	closeUpstream := echoListener(t, upstreamPort)
	defer closeUpstream()
	coordinator := NewCoordinator(slog.New(slog.NewTextHandler(io.Discard, nil)))

	direct := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 8080, HostPort: 8080}
	forwarded := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: upstreamPort, HostPort: freePort(t)}
	mappings := []model.PortMapping{direct, forwarded}
	if err := coordinator.Prepare("workload-live", mappings); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Another workload cannot prepare the same host port.
	if err := coordinator.Prepare("workload-rival", []model.PortMapping{forwarded}); err == nil {
		t.Fatal("a host port must not be double-booked")
	}
	if err := coordinator.Activate(context.Background(), "workload-live", mappings); err != nil {
		t.Fatalf("activate: %v", err)
	}
	// The forwarded mapping is reachable and carries traffic; the direct
	// mapping needed no forwarder at all.
	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(forwarded.HostPort)))
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(connection, make([]byte, 4)); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if ports := coordinator.ForwardedPorts("workload-live"); len(ports) != 1 || ports[0] != forwarded.HostPort {
		t.Fatalf("unexpected forwarded ports: %v", ports)
	}

	spec := model.WorkloadSpec{ID: "workload-live", RootPath: t.TempDir(), NetworkPolicy: model.NetworkReconnect,
		Ports: []model.PortSpec{{Protocol: "tcp", ContainerPort: upstreamPort, HostPort: forwarded.HostPort}}}
	plan, err := PlanFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	path, err := coordinator.RecordStatus(spec, plan, OperationRestore, "restore-42", StatusCompleted, "", false)
	if err != nil {
		t.Fatalf("record status: %v", err)
	}
	if filepath.Base(path) != StatusDocumentName {
		t.Fatalf("the status must land in the workload root: %s", path)
	}
	document, err := ReadStatus(spec.RootPath)
	if err != nil {
		t.Fatalf("read status back: %v", err)
	}
	if document.OperationID != "restore-42" || document.SocketsPreserved || !document.ConnectionsDropped {
		t.Fatalf("the application must be told the truth: %+v", document)
	}
	if len(document.ForwardedPorts) != 1 || document.ForwardedPorts[0] != forwarded.HostPort {
		t.Fatalf("the status must list the forwarded port: %+v", document)
	}

	// Deactivating withdraws everything: the port frees up and the
	// reservations are gone.
	coordinator.Deactivate("workload-live", 0)
	if ports := coordinator.ForwardedPorts("workload-live"); len(ports) != 0 {
		t.Fatalf("forwarders must stop: %v", ports)
	}
	probe, err := (&net.ListenConfig{}).Listen(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(forwarded.HostPort)))
	if err != nil {
		t.Fatalf("the deactivated port must be free: %v", err)
	}
	probe.Close()
	if err := coordinator.Prepare("workload-rival", []model.PortMapping{forwarded}); err != nil {
		t.Fatalf("a deactivated port must be reservable again: %v", err)
	}
}

func TestCoordinatorRefusesSecondActivation(t *testing.T) {
	coordinator := NewCoordinator(nil)
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 80, HostPort: freePort(t)}
	if err := coordinator.Prepare("workload-a", []model.PortMapping{mapping}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Activate(context.Background(), "workload-a", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := coordinator.Activate(context.Background(), "workload-a", []model.PortMapping{mapping}); err == nil {
		t.Fatal("a workload must not be activated twice")
	}
	coordinator.Deactivate("workload-a", 0)
	// Deactivating an unknown workload is a no-op, not an error.
	coordinator.Deactivate("workload-ghost", 0)
}

func TestDrainForwardersKeepsReservations(t *testing.T) {
	coordinator := NewCoordinator(nil)
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 80, HostPort: freePort(t)}
	if err := coordinator.Prepare("workload-drain", []model.PortMapping{mapping}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Activate(context.Background(), "workload-drain", []model.PortMapping{mapping}); err != nil {
		t.Fatal(err)
	}
	coordinator.DrainForwarders("workload-drain", 0)
	// Draining stops the forwarders but keeps the ports reserved: the
	// workload is mid-migration, not gone.
	if err := coordinator.Prepare("workload-other", []model.PortMapping{mapping}); err == nil {
		t.Fatal("a drained workload must still hold its ports")
	}
	// Reactivation brings the forwarder back.
	if err := coordinator.Activate(context.Background(), "workload-drain", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("reactivate after drain: %v", err)
	}
	coordinator.Deactivate("workload-drain", 0)
}

// TestForwarderSurvivesCanceledActivationContext: every production caller
// activates a forwarder with a context it cancels the moment the activating
// call returns — restore, API start, fork, clone. The forwarder's contract is
// to serve until Stop or Drain, so a canceled activation context must not
// kill the upstream dials: before the fix, the first forwarded connection
// after cancellation was accepted and reset.
func TestForwarderSurvivesCanceledActivationContext(t *testing.T) {
	containerPort := freePort(t)
	closeUpstream := echoListener(t, containerPort)
	defer closeUpstream()
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: containerPort, HostPort: freePort(t)}
	forwarder := NewForwarder(mapping, slog.New(slog.NewTextHandler(io.Discard, nil)))
	activation, cancel := context.WithCancel(context.Background())
	if err := forwarder.Start(activation); err != nil {
		t.Fatalf("start forwarder: %v", err)
	}
	defer forwarder.Stop()
	cancel()

	for attempt := 0; attempt < 3; attempt++ {
		connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)))
		if err != nil {
			t.Fatalf("dial forwarded port after activation returned: %v", err)
		}
		payload := []byte("still-forwarding")
		if _, err := connection.Write(payload); err != nil {
			t.Fatalf("write through forwarder: %v", err)
		}
		if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, len(payload))
		if _, err := io.ReadFull(connection, buffer); err != nil {
			t.Fatalf("connection %d was reset: %v", attempt, err)
		}
		connection.Close()
	}
}

// TestForwarderReachesIPv6OnlyUpstream: a workload whose listener binds only
// ::1 is reachable through the forwarder, which must try both loopback
// families instead of hard-coding 127.0.0.1.
func TestForwarderReachesIPv6OnlyUpstream(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), ProtocolTCP, "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback unavailable")
	}
	upstreamPort := listener.Addr().(*net.TCPAddr).Port
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func(connection net.Conn) {
				defer connection.Close()
				io.Copy(connection, connection)
			}(connection)
		}
	}()
	defer func() { listener.Close(); <-done }()

	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: upstreamPort, HostPort: freePort(t)}
	forwarder := NewForwarder(mapping, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := forwarder.Start(context.Background()); err != nil {
		t.Fatalf("start forwarder: %v", err)
	}
	defer forwarder.Stop()

	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)))
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("v6")); err != nil {
		t.Fatal(err)
	}
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 2)
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("IPv6-only upstream unreachable: %v", err)
	}
}

// TestForwarderDrainAbortsPendingDials: a connection still inside the
// upstream dial must not stretch Drain past its grace period — the dial runs
// under the forwarder's lifecycle and dies with it.
func TestForwarderDrainAbortsPendingDials(t *testing.T) {
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 65000, HostPort: freePort(t)}
	forwarder := NewForwarder(mapping, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A TEST-NET-1 address: the dial hangs until it is canceled or times out.
	forwarder.upstream = []string{"192.0.2.1:65000"}
	if err := forwarder.Start(context.Background()); err != nil {
		t.Fatalf("start forwarder: %v", err)
	}
	defer forwarder.Stop()

	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(mapping.HostPort)))
	if err != nil {
		t.Fatalf("dial forwarded port: %v", err)
	}
	defer connection.Close()
	time.Sleep(100 * time.Millisecond)

	started := time.Now()
	forwarder.Drain(50 * time.Millisecond)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Drain overshot its bound waiting on a dial: %s", elapsed)
	}
}

// TestNATTableReserveRejectsDuplicatesInOneCall: Reserve's contract says it
// fails without modifying the table when a port is taken — including by an
// earlier mapping in the same call, which used to be silently overwritten.
func TestNATTableReserveRejectsDuplicatesInOneCall(t *testing.T) {
	table := NewNATTable()
	first := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 8080, HostPort: 18080}
	second := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 8081, HostPort: 18080}
	if err := table.Reserve("w1", []model.PortMapping{first, second}); err == nil {
		t.Fatal("a duplicate host port within one reservation must be refused")
	}
	if _, taken := table.Lookup(18080); taken {
		t.Fatal("a refused reservation must leave the table untouched")
	}
}

// TestNATTableManyWorkersReserveUniquePorts: cloning many workers is only
// safe when each claims a distinct host port — a collision is refused up
// front, not discovered at bind time.
func TestNATTableManyWorkersReserveUniquePorts(t *testing.T) {
	table := NewNATTable()
	for i := 0; i < 32; i++ {
		mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 20000 + i, HostPort: 21000 + i}
		if err := table.Reserve("worker-"+strconv.Itoa(i), []model.PortMapping{mapping}); err != nil {
			t.Fatalf("worker %d could not claim its own port: %v", i, err)
		}
	}
	clash := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 20000, HostPort: 21000}
	if err := table.Reserve("worker-33", []model.PortMapping{clash}); err == nil {
		t.Fatal("a worker must not steal another worker's host port")
	}
}

// TestCoordinatorActivateFailureReleasesReservations: when activation cannot
// bind, everything it started is undone and the ports are released — a failed
// activation leaves the machine as it was, per the contract, instead of
// pinning ports against forwarding that does not exist.
func TestCoordinatorActivateFailureReleasesReservations(t *testing.T) {
	hostPort := freePort(t)
	held, err := (&net.ListenConfig{}).Listen(context.Background(), ProtocolTCP, net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)))
	if err != nil {
		t.Fatalf("hold host port: %v", err)
	}
	defer held.Close()

	coordinator := NewCoordinator(nil)
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 9000, HostPort: hostPort}
	if err := coordinator.Prepare("w-fail", []model.PortMapping{mapping}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Activate(context.Background(), "w-fail", []model.PortMapping{mapping}); err == nil {
		t.Fatal("activation over an occupied host port must fail")
	}
	if err := coordinator.Prepare("w-next", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("a failed activation must release its reservations: %v", err)
	}
}

// TestRebuildReservationsSurvivesCoordinatorStartup: the runtime layer
// records the port claims of surviving workloads at manager open, and a
// coordinator constructed afterwards starts from them — the NAT table is
// in-memory and would otherwise be emptier than the machine after a restart.
func TestRebuildReservationsSurvivesCoordinatorStartup(t *testing.T) {
	defer RebuildReservations(nil)
	mapping := model.PortMapping{Protocol: ProtocolTCP, ContainerPort: 9000, HostPort: 22000}
	RebuildReservations([]model.Workload{{
		Spec: model.WorkloadSpec{ID: "survivor", Ports: []model.PortSpec{
			{Protocol: "tcp", ContainerPort: mapping.ContainerPort, HostPort: mapping.HostPort},
		}},
		Status: model.WorkloadRunning,
	}})
	coordinator := NewCoordinator(nil)
	if err := coordinator.Prepare("rival", []model.PortMapping{mapping}); err == nil {
		t.Fatal("a rebuilt claim must block the port for anyone but its holder")
	}
	if err := coordinator.Prepare("survivor", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("the claim holder must re-reserve its own mapping: %v", err)
	}

	RebuildReservations(nil)
	fresh := NewCoordinator(nil)
	if err := fresh.Prepare("rival", []model.PortMapping{mapping}); err != nil {
		t.Fatalf("cleared claims must release the machine: %v", err)
	}
}
