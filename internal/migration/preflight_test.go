package migration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/transfer"
)

// preflightEngine satisfies the checkpoint engine interface without CRIU —
// a preflight never dumps, the engine only exists to construct the service.
type preflightEngine struct{}

func (preflightEngine) Check(context.Context) error                           { return nil }
func (preflightEngine) Version(context.Context) (string, error)               { return "CRIU preflight test", nil }
func (preflightEngine) PreDump(context.Context, checkpoint.DumpOptions) error { return nil }
func (preflightEngine) Dump(context.Context, checkpoint.DumpOptions) error    { return nil }
func (preflightEngine) Restore(context.Context, checkpoint.RestoreOptions) (int, error) {
	return 0, nil
}

// fakePeer answers the machine endpoint with a controlled profile and fails
// every other peer operation — a preflight only ever calls Machine.
type fakePeer struct {
	profile    model.MachineCapabilities
	machineErr error
}

func (p *fakePeer) Machine(context.Context) (model.MachineCapabilities, error) {
	return p.profile, p.machineErr
}

func (p *fakePeer) Reserve(context.Context, transfer.ReserveRequest) (transfer.Session, error) {
	return transfer.Session{}, errors.New("preflight never reserves")
}

func (p *fakePeer) ImportKey(context.Context, string, string, uint32, []byte) error {
	return errors.New("preflight never imports keys")
}

func (p *fakePeer) ImportManifest(context.Context, string, model.CheckpointManifest) (transfer.MissingResponse, error) {
	return transfer.MissingResponse{}, errors.New("preflight never imports manifests")
}

func (p *fakePeer) UploadChunk(context.Context, string, model.ChunkRef, *chunkstore.Store) error {
	return errors.New("preflight never uploads chunks")
}

func (p *fakePeer) Verify(context.Context, string) (transfer.Session, error) {
	return transfer.Session{}, errors.New("preflight never verifies")
}

func (p *fakePeer) Restore(context.Context, string, time.Duration) (checkpoint.RestoreRecord, error) {
	return checkpoint.RestoreRecord{}, errors.New("preflight never restores")
}

func (p *fakePeer) Commit(context.Context, string) (transfer.Session, error) {
	return transfer.Session{}, errors.New("preflight never commits")
}

func (p *fakePeer) Rollback(context.Context, string) (transfer.Session, error) {
	return transfer.Session{}, errors.New("preflight never rolls back")
}

func (p *fakePeer) Get(context.Context, string) (transfer.Session, error) {
	return transfer.Session{}, errors.New("preflight never reads sessions")
}

// preflightOrchestrator builds a real orchestrator around one running
// workload and a client factory the test controls.
func preflightOrchestrator(t *testing.T, clients ClientFactory) (*Orchestrator, model.Workload, *identity.Identity) {
	t.Helper()
	stateRoot := t.TempDir()
	workloadRoot := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(stateRoot+"/runtime", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workloadRoot, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := model.WorkloadSpec{
		Name: "preflighted", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(),
	}
	workload, err := runtimeManager.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeManager.Start(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = runtimeManager.Stop(workload.Spec.ID, time.Second) })
	machine, err := identity.Ensure(stateRoot + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(stateRoot + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(stateRoot+"/objects", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := checkpoint.OpenRepository(stateRoot+"/checkpoints", keys)
	if err != nil {
		t.Fatal(err)
	}
	inventory := linuxplatform.NewInventory(machine.Machine.ID)
	checkpoints := checkpoint.NewService(stateRoot, runtimeManager, machine, inventory, chunks, repository, preflightEngine{}, logger)
	orchestrator, err := Open(stateRoot, keys, runtimeManager, checkpoints, repository, chunks, machine, inventory, clients, 1, logger)
	if err != nil {
		t.Fatal(err)
	}
	return orchestrator, workload, machine
}

// destinationLike builds a destination profile the checker accepts against
// this machine's real inventory: same kernel, architecture, OS, and
// features, ample storage on a mount covering any workload root, and a
// healthy CRIU.
func destinationLike(source model.MachineCapabilities, machineID string) model.MachineCapabilities {
	destination := source
	destination.MachineID = machineID
	destination.Hostname = "destination.example"
	destination.Storage = []model.StorageDevice{{Mountpoint: "/", AvailableBytes: 1 << 40}}
	destination.CRIU = model.CRIUCapabilities{Installed: true, Version: "3.17.1", Healthy: true}
	return destination
}

// TestPreflight covers the dry run's contract: a compatible destination
// produces a report with both machines and the network plan and no
// migration record; an incompatible one carries the rejection in the
// report; and the three hard identity failures — unreachable, wrong
// machine, destination-is-source — come back as the same errors a real
// migration would fail with.
func TestPreflight(t *testing.T) {
	inventory := linuxplatform.NewInventory("source-machine")
	source, err := inventory.Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect source: %v", err)
	}
	peer := &fakePeer{profile: destinationLike(source, "destination-machine")}
	var dialed model.Destination
	orchestrator, workload, machine := preflightOrchestrator(t, func(destination model.Destination) (PeerClient, error) {
		dialed = destination
		return peer, nil
	})
	request := CreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{AgentURL: "https://destination.example:8443"},
	}

	result, err := orchestrator.Preflight(context.Background(), request)
	if err != nil {
		t.Fatalf("preflight against a compatible destination: %v", err)
	}
	if !result.Report.Compatible {
		t.Fatalf("identical machines must be compatible, got issues: %+v", result.Report.Issues)
	}
	if result.Destination.MachineID != "destination-machine" || result.SourceMachine.MachineID != machine.Machine.ID {
		t.Fatalf("preflight returned the wrong machines: source=%s destination=%s",
			result.SourceMachine.MachineID, result.Destination.MachineID)
	}
	if result.Workload.ID != workload.Spec.ID {
		t.Fatalf("preflight reported workload %s, want %s", result.Workload.ID, workload.Spec.ID)
	}
	if result.Network.Summary == "" {
		t.Fatal("the network plan the migration would apply must be part of the report")
	}
	if dialed.AgentURL != request.Destination.AgentURL {
		t.Fatalf("the peer client dialed %s, want %s", dialed.AgentURL, request.Destination.AgentURL)
	}

	// A different architecture is a rejection carried in the report, not an
	// error: the dry run ran, and its finding is "this would not work".
	incompatible := peer.profile
	incompatible.Architecture = "arm64"
	peer.profile = incompatible
	result, err = orchestrator.Preflight(context.Background(), request)
	if err != nil {
		t.Fatalf("preflight of an incompatible destination must still run: %v", err)
	}
	if result.Report.Compatible {
		t.Fatal("an arm64 destination for an amd64 workload must be incompatible")
	}
	found := false
	for _, issue := range result.Report.Issues {
		if issue.Code == "ARCH_MISMATCH" && issue.Severity == "error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the report must carry the architecture rejection, got %+v", result.Report.Issues)
	}
	peer.profile = destinationLike(source, "destination-machine")

	// The three hard failures mirror a real migration's codes.
	_, err = orchestrator.Preflight(context.Background(), CreateRequest{
		WorkloadID:  workload.Spec.ID,
		Destination: model.Destination{MachineID: "someone-else", AgentURL: "https://destination.example:8443"},
	})
	if !errors.Is(err, ErrDestinationIdentityMismatch) {
		t.Fatalf("a machine-id mismatch must fail preflight with the identity error, got %v", err)
	}
	peer.profile = destinationLike(source, machine.Machine.ID)
	_, err = orchestrator.Preflight(context.Background(), request)
	if !errors.Is(err, ErrDestinationIsSource) {
		t.Fatalf("migrating to the source machine must fail preflight with the is-source error, got %v", err)
	}
	peer.profile = destinationLike(source, "destination-machine")
	peer.machineErr = errors.New("connection refused")
	_, err = orchestrator.Preflight(context.Background(), request)
	if !errors.Is(err, ErrDestinationUnreachable) {
		t.Fatalf("an unreachable destination must fail preflight with the unreachable error, got %v", err)
	}
	peer.machineErr = nil

	if migrations := orchestrator.List(); len(migrations) != 0 {
		t.Fatalf("a dry run must never create migration records, got %d", len(migrations))
	}
}
