package agent

// replicate_test.go drives the whole replication path against a real standby
// agent: the source's policy loop pushes its newest root checkpoint over the
// mutually authenticated peer channel, the standby records the duty and can
// load the checkpoint from its own store, withdrawals follow policy removal,
// and a standby change withdraws the old duty before anything is pushed to
// the new one.

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
)

// openReplicationSource opens the source agent: a development agent that
// advertises where its own peer listener lives, so the standby it pushes to
// learns where to probe it before believing it dead.
func openReplicationSource(t *testing.T, advertisedURL string) *Service {
	t.Helper()
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + configuration.StateDir + "/agent.sock"
	configuration.RemoteListen = "tcp://127.0.0.1:0"
	configuration.InsecureDevelopment = true
	configuration.ControlPlane.AgentURL = advertisedURL
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// servePeerListener serves one agent's peer handler over live TLS the way the
// agent itself serves a development remote listener: the server's own
// certificate, client certificates requested but not verified — the handler
// digests what arrives. The returned URL is what a failover policy carries.
func servePeerListener(t *testing.T, service *Service) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsConfiguration, err := service.tlsConfig(true)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: service.peerServer.Handler(), TLSConfig: tlsConfiguration}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	go func() { _ = server.Serve(tls.NewListener(listener, tlsConfiguration)) }()
	return "https://" + listener.Addr().String()
}

// replicationPair is a source agent holding a policy-carrying workload and a
// standby agent whose peer listener is live — two full agents, so what the
// test exercises is the wire between them, not a shortcut around it.
type replicationPair struct {
	source      *Service
	destination *Service
	standbyURL  string
	workload    model.WorkloadSpec
}

func newReplicationPair(t *testing.T) *replicationPair {
	t.Helper()
	destination := openStandbyTestService(t, "")
	standbyURL := servePeerListener(t, destination)
	source := openReplicationSource(t, "https://source.example:9443")
	root := t.TempDir()
	workload, err := source.runtime.Create(model.WorkloadSpec{
		Name: "replicated", Command: []string{"/bin/true"}, RootPath: root, WorkingDir: root,
		UID: 4242, GID: 4242,
		CheckpointPolicy: &model.CheckpointPolicySpec{IntervalSeconds: 10},
		FailoverPolicy: &model.FailoverPolicySpec{
			AgentURL: standbyURL, MachineID: destination.identity.Machine.ID, KeepLast: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &replicationPair{source: source, destination: destination, standbyURL: standbyURL, workload: workload.Spec}
}

// pushRootCheckpoint fabricates a root checkpoint plus a newer incremental on
// the source, runs one replication pass, and returns the root — the state a
// standby can restore on its own.
func pushRootCheckpoint(t *testing.T, pair *replicationPair) model.CheckpointManifest {
	t.Helper()
	root := fabricateCheckpoint(t, pair.source, pair.workload, "checkpoint-root-1", "")
	delta := fabricateCheckpoint(t, pair.source, pair.workload, "checkpoint-delta-1", root.ID)
	// Precondition: the delta is the newest checkpoint. The pass must still
	// push the root — a delta without its ancestors restores nothing.
	if summaries := pair.source.checkpoints.List(pair.workload.ID); summaries[0].ID != delta.ID {
		t.Fatalf("test precondition: newest checkpoint is %s, want the delta %s", summaries[0].ID, delta.ID)
	}
	pair.source.replicator.apply(context.Background())
	return root
}

// TestReplicatorPushesTheNewestRootCheckpoint is the whole sending half in
// one pass: the ledger records the push to the pinned standby, the standby
// records the duty with the source's facts, its own repository can load the
// checkpoint, and a pass with nothing new pushes nothing.
func TestReplicatorPushesTheNewestRootCheckpoint(t *testing.T) {
	pair := newReplicationPair(t)
	root := pushRootCheckpoint(t, pair)
	entry, err := pair.source.replicator.ledger.Get(pair.workload.ID)
	if err != nil {
		t.Fatalf("ledger entry after push: %v", err)
	}
	if entry.StandbyURL != pair.standbyURL || entry.StandbyMachineID != pair.destination.identity.Machine.ID {
		t.Fatalf("ledger entry: %+v", entry)
	}
	if entry.LastCheckpointID != root.ID || entry.LastError != "" || entry.LastPushAt.IsZero() {
		t.Fatalf("ledger entry after a clean push: %+v", entry)
	}
	duty, err := pair.destination.standby.Duty(pair.workload.ID)
	if err != nil {
		t.Fatalf("standby duty after push: %v", err)
	}
	if duty.State != model.StandbyArmed || duty.LastCheckpointID != root.ID {
		t.Fatalf("duty after push: %+v", duty)
	}
	if duty.SourceMachineID != pair.source.identity.Machine.ID || duty.SourceAgentURL != "https://source.example:9443" {
		t.Fatalf("duty source facts: %+v", duty)
	}
	if duty.WorkloadName != "replicated" || duty.WorkloadUID != 4242 || duty.KeepLast != 2 {
		t.Fatalf("duty workload facts: %+v", duty)
	}
	if _, err := pair.destination.repository.Load(root.ID); err != nil {
		t.Fatalf("replicated checkpoint not loadable on the standby: %v", err)
	}
	pair.source.replicator.apply(context.Background())
	again, err := pair.source.replicator.ledger.Get(pair.workload.ID)
	if err != nil || again.LastCheckpointID != root.ID || !again.LastPushAt.Equal(entry.LastPushAt) {
		t.Fatalf("an unchanged pass must not re-push: %+v (%v)", again, err)
	}
}

// TestReplicatorWithdrawsWhenThePolicyIsRemoved: clearing the failover
// policy releases the standby from its duty on the next pass — the duty is
// gone, the ledger entry is gone, and the replicated checkpoints stay (they
// are the standby operator's to prune, not the source's to delete).
func TestReplicatorWithdrawsWhenThePolicyIsRemoved(t *testing.T) {
	pair := newReplicationPair(t)
	root := pushRootCheckpoint(t, pair)
	if _, err := pair.source.runtime.SetFailoverPolicy(pair.workload.ID, nil); err != nil {
		t.Fatal(err)
	}
	pair.source.replicator.apply(context.Background())
	if _, err := pair.destination.standby.Duty(pair.workload.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the withdrawn duty must be gone, got %v", err)
	}
	if _, err := pair.source.replicator.ledger.Get(pair.workload.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the ledger entry must be gone, got %v", err)
	}
	if _, err := pair.destination.repository.Load(root.ID); err != nil {
		t.Fatalf("withdrawal must leave the replicated checkpoint behind: %v", err)
	}
}

// TestReplicatorRefusesTheWrongStandby: a policy pinning a machine the
// standby does not answer as fails before anything leaves the source — no
// duty appears, and the failure lands on the ledger where `failover status`
// shows it, with the backoff holding until the operator fixes the pin.
func TestReplicatorRefusesTheWrongStandby(t *testing.T) {
	pair := newReplicationPair(t)
	fabricateCheckpoint(t, pair.source, pair.workload, "checkpoint-root-1", "")
	wrongPin := &model.FailoverPolicySpec{AgentURL: pair.standbyURL, MachineID: "machine-imposter"}
	if _, err := pair.source.runtime.SetFailoverPolicy(pair.workload.ID, wrongPin); err != nil {
		t.Fatal(err)
	}
	pair.source.replicator.apply(context.Background())
	entry, err := pair.source.replicator.ledger.Get(pair.workload.ID)
	if err != nil {
		t.Fatalf("a refused push must still be on the ledger: %v", err)
	}
	if !strings.Contains(entry.LastError, "standby identity mismatch") {
		t.Fatalf("the refusal must name the identity mismatch, got %q", entry.LastError)
	}
	if _, err := pair.destination.standby.Duty(pair.workload.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("no duty may exist for a refused push, got %v", err)
	}
	// The retry backoff holds: an immediate second pass does not re-attempt.
	pair.source.replicator.apply(context.Background())
	again, err := pair.source.replicator.ledger.Get(pair.workload.ID)
	if err != nil || again.LastErrorAt != entry.LastErrorAt {
		t.Fatalf("an immediate pass must respect the retry backoff: %+v (%v)", again, err)
	}
}

// TestReplicatorChangesStandbysSafely: re-pointing a workload at a new
// standby withdraws the old standby's duty first. Until that withdrawal
// lands, nothing is pushed to the new standby — and a new standby that
// cannot be reached leaves the change recorded as a failure, never as a
// silent success with the old duty stranded.
func TestReplicatorChangesStandbysSafely(t *testing.T) {
	pair := newReplicationPair(t)
	root := pushRootCheckpoint(t, pair)
	if _, err := pair.source.runtime.SetFailoverPolicy(pair.workload.ID,
		&model.FailoverPolicySpec{AgentURL: unreachablePeer}); err != nil {
		t.Fatal(err)
	}
	pair.source.replicator.apply(context.Background())
	// The old standby was released before anything else happened.
	if _, err := pair.destination.standby.Duty(pair.workload.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("the old standby's duty must be withdrawn, got %v", err)
	}
	// The new standby never answered, so the ledger says so instead of
	// claiming a push.
	entry, err := pair.source.replicator.ledger.Get(pair.workload.ID)
	if err != nil {
		t.Fatalf("the failed change must be on the ledger: %v", err)
	}
	if entry.StandbyURL != unreachablePeer || entry.LastError == "" {
		t.Fatalf("ledger after a failed standby change: %+v", entry)
	}
	// The old standby keeps the replicated checkpoints it already holds.
	if _, err := pair.destination.repository.Load(root.ID); err != nil {
		t.Fatalf("the old standby must keep what it holds: %v", err)
	}
}

// TestReplicatorHoldsBackWhileAMigrationIsInFlight: a workload being
// migrated is not replicated in the same pass — the migration owns the
// workload's state transfer, and a concurrent push would race it.
func TestReplicatorHoldsBackWhileAMigrationIsInFlight(t *testing.T) {
	pair := newReplicationPair(t)
	fabricateCheckpoint(t, pair.source, pair.workload, "checkpoint-root-1", "")
	pair.source.replicator.migrationInFlight = func(string) bool { return true }
	pair.source.replicator.apply(context.Background())
	if _, err := pair.source.replicator.ledger.Get(pair.workload.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("a migration in flight must hold replication back, got %v", err)
	}
	if _, err := pair.destination.standby.Duty(pair.workload.ID); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("no duty may appear while a migration is in flight, got %v", err)
	}
}
