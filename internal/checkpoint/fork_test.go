package checkpoint

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

// activatingEngine stands in for CRIU during fork activation. Restore starts a
// real long-lived process so that adoption, resume, and liveness validation run
// against a genuine pid instead of a stub, and records the options it was given
// so the bind mount the fork depends on can be asserted.
type activatingEngine struct {
	restores []RestoreOptions
}

func (*activatingEngine) Check(context.Context) error             { return nil }
func (*activatingEngine) Version(context.Context) (string, error) { return "CRIU activating", nil }
func (*activatingEngine) PreDump(context.Context, DumpOptions) error {
	return nil
}
func (*activatingEngine) Dump(_ context.Context, options DumpOptions) error {
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}
func (e *activatingEngine) Restore(_ context.Context, options RestoreOptions) (int, error) {
	e.restores = append(e.restores, options)
	command := exec.Command("/bin/sh", "-c", "while true; do sleep 1; done")
	// A CRIU-restored tree leads its own session and process group, exactly
	// like a workload the manager starts; reproducing that here keeps the
	// runtime manager's group-wide signalling scoped to the fake workload.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
}

type forkFixture struct {
	service  *Service
	forker   *Forker
	runtime  *shiftruntime.Manager
	repo     *Repository
	chunks   *chunkstore.Store
	source   model.Workload
	rootPath string
}

func newForkFixture(t *testing.T, engine Engine) forkFixture {
	t.Helper()
	stateRoot := t.TempDir()
	workloadRoot := filepath.Join(t.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(workloadRoot, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(filepath.Join(stateRoot, "runtime"), false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workloadRoot, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workloadRoot, "data", "state.txt"), []byte("forked state"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "fork-source", Command: []string{script}, RootPath: workloadRoot, WorkingDir: filepath.Join(workloadRoot, "data"),
		UID: os.Geteuid(), GID: os.Getegid(),
		Ports: []model.PortSpec{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeManager.Start(source.Spec.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = runtimeManager.Stop(source.Spec.ID, time.Second) })
	machine, err := identity.Ensure(filepath.Join(stateRoot, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(filepath.Join(stateRoot, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(filepath.Join(stateRoot, "objects"), 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(filepath.Join(stateRoot, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, engine, logger)
	// The agent always attaches a network coordinator; a fork of the
	// port-declaring source below needs it to reserve the fork's ports.
	service.SetNetwork(network.NewCoordinator(logger))
	forker, err := OpenForker(service)
	if err != nil {
		t.Fatal(err)
	}
	current, err := runtimeManager.Get(source.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return forkFixture{service: service, forker: forker, runtime: runtimeManager, repo: repository, chunks: chunks, source: current, rootPath: workloadRoot}
}

func TestForkCreatesIndependentWorkloadAndCheckpoint(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-a"})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != ForkCommitted {
		t.Fatalf("fork was not committed: %+v", record)
	}
	if record.ForkWorkloadID == fixture.source.Spec.ID || record.ForkCheckpointID == "" {
		t.Fatalf("fork did not receive its own identity: %+v", record)
	}
	if record.Activated || record.PID != 0 {
		t.Fatalf("fork should not have been activated: %+v", record)
	}
	content, err := os.ReadFile(filepath.Join(record.RootPath, "data", "state.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "forked state" {
		t.Fatalf("fork filesystem was not materialized: %q", content)
	}
	// The source workload must be untouched and still running.
	source, err := fixture.runtime.Get(fixture.source.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != model.WorkloadRunning {
		t.Fatalf("source workload was disturbed by the fork: %+v", source)
	}
	if _, err := os.Stat(filepath.Join(fixture.rootPath, "data", "state.txt")); err != nil {
		t.Fatalf("source filesystem was disturbed: %v", err)
	}
	forked, err := fixture.runtime.Get(record.ForkWorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if forked.Spec.Name != "fork-a" || forked.Spec.RootPath != record.RootPath {
		t.Fatalf("unexpected fork specification: %+v", forked.Spec)
	}
	if forked.Status != model.WorkloadCheckpointed || forked.LatestCheckpointID != record.ForkCheckpointID {
		t.Fatalf("fork checkpoint was not recorded on the workload: %+v", forked)
	}
	if forked.Spec.WorkingDir != filepath.Join(record.RootPath, "data") {
		t.Fatalf("fork working directory was not rebased: %q", forked.Spec.WorkingDir)
	}
	if len(forked.Spec.Ports) != 1 || forked.Spec.Ports[0].HostPort != 0 || forked.Spec.Ports[0].ContainerPort != 8080 {
		t.Fatalf("fork must not inherit published host ports: %+v", forked.Spec.Ports)
	}
	if forked.Spec.Lineage == nil {
		t.Fatal("fork lineage was not recorded")
	}
	lineage := *forked.Spec.Lineage
	if lineage.SourceWorkloadID != fixture.source.Spec.ID || lineage.SourceCheckpointID != record.SourceCheckpointID {
		t.Fatalf("unexpected lineage: %+v", lineage)
	}
	if lineage.SourceRootPath != fixture.rootPath || lineage.Generation != 1 || lineage.ForkedAt.IsZero() {
		t.Fatalf("incomplete lineage: %+v", lineage)
	}
}

// TestForkCheckpointIsSelfContained proves the fork does not depend on the
// source's key namespace or retention: the fork's checkpoint is a full
// checkpoint whose chunks validate under the fork's own workload id.
func TestForkCheckpointIsSelfContained(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := fixture.repo.Load(record.ForkCheckpointID)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Kind != model.CheckpointFull || manifest.ParentID != "" {
		t.Fatalf("fork checkpoint must be a parentless full checkpoint: %+v", manifest)
	}
	if manifest.Workload.ID != record.ForkWorkloadID {
		t.Fatalf("fork checkpoint belongs to %s, want %s", manifest.Workload.ID, record.ForkWorkloadID)
	}
	if len(manifest.Assets) != 2 || manifest.Metrics.PlainBytes == 0 {
		t.Fatalf("incomplete fork checkpoint: %+v", manifest.Assets)
	}
	for _, asset := range manifest.Assets {
		if err := fixture.chunks.ValidateAsset(context.Background(), record.ForkWorkloadID, asset); err != nil {
			t.Fatalf("fork asset %s does not validate in the fork key namespace: %v", asset.Name, err)
		}
	}
	if err := VerifyManifest(manifest); err != nil {
		t.Fatal(err)
	}
	// Deleting the source's checkpoint must not affect the fork.
	if err := fixture.repo.Delete(record.SourceCheckpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repo.Load(record.ForkCheckpointID); err != nil {
		t.Fatalf("fork checkpoint became unreadable after the source checkpoint was deleted: %v", err)
	}
}

func TestForkActivationBindsForkRootOverSourceRoot(t *testing.T) {
	engine := &activatingEngine{}
	fixture := newForkFixture(t, engine)
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-live", Activate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !record.Activated || record.PID <= 0 || record.State != ForkCommitted {
		t.Fatalf("fork was not activated: %+v", record)
	}
	t.Cleanup(func() { _, _ = fixture.runtime.Stop(record.ForkWorkloadID, time.Second) })
	if len(engine.restores) != 1 {
		t.Fatalf("expected exactly one restore, got %d", len(engine.restores))
	}
	options := engine.restores[0]
	if len(options.BindMounts) != 1 {
		t.Fatalf("fork activation must bind the fork root over the source root: %+v", options.BindMounts)
	}
	mount := options.BindMounts[0]
	if mount.Source != record.RootPath || mount.Target != fixture.rootPath {
		t.Fatalf("unexpected bind mount %+v", mount)
	}
	if !strings.HasPrefix(options.ImagesDirectory, filepath.Join(fixture.service.stateDir, "forks")) {
		t.Fatalf("restore did not use the fork's own images: %q", options.ImagesDirectory)
	}
	forked, err := fixture.runtime.Get(record.ForkWorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	if forked.Status != model.WorkloadRunning || forked.Process == nil || forked.Process.PID != record.PID {
		t.Fatalf("forked workload is not running: %+v", forked)
	}
	source, err := fixture.runtime.Get(fixture.source.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != model.WorkloadRunning || source.Process == nil {
		t.Fatalf("source stopped running while its fork was activated: %+v", source)
	}
	if source.Process.PID == record.PID {
		t.Fatal("fork and source share a pid")
	}
}

func TestForkFromExplicitCheckpointRejectsForeignCheckpoint(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	first, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-first"})
	if err != nil {
		t.Fatal(err)
	}
	// The first fork's own checkpoint belongs to the fork, not the source.
	_, err = fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{
		Name: "fork-second", CheckpointID: first.ForkCheckpointID,
	})
	if err == nil {
		t.Fatal("forking a source from another workload's checkpoint must fail")
	}
	if !strings.Contains(err.Error(), "belongs to workload") {
		t.Fatalf("unexpected error: %v", err)
	}
	// Forking from the source's own checkpoint is accepted and reuses it.
	second, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{
		Name: "fork-second", CheckpointID: first.SourceCheckpointID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.SourceCheckpointID != first.SourceCheckpointID {
		t.Fatalf("explicit fork point was not honoured: %+v", second)
	}
	if second.RootPath == first.RootPath || second.ForkWorkloadID == first.ForkWorkloadID {
		t.Fatalf("two forks of one checkpoint must be independent: %+v %+v", first, second)
	}
}

func TestForkRollbackRemovesForkAndKeepsSource(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-doomed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.forker.Rollback(context.Background(), record.ID, "operator request"); err == nil {
		t.Fatal("a committed fork must not be rollback-able")
	}
	// Simulate an interrupted fork: rewind the record to the materialized state
	// and recover, which is what an agent restart does.
	interrupted, err := fixture.forker.records.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	interrupted.State = ForkMaterialized
	if err := fixture.forker.records.Put(interrupted.ID, interrupted); err != nil {
		t.Fatal(err)
	}
	if err := fixture.forker.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	rolled, err := fixture.forker.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.State != ForkRolledBack {
		t.Fatalf("fork was not rolled back: %+v", rolled)
	}
	if _, err := os.Stat(record.RootPath); !os.IsNotExist(err) {
		t.Fatalf("fork root survived rollback: %v", err)
	}
	if _, err := fixture.runtime.Get(record.ForkWorkloadID); err == nil {
		t.Fatal("fork workload survived rollback")
	}
	if _, err := fixture.repo.Load(record.ForkCheckpointID); err == nil {
		t.Fatal("fork checkpoint survived rollback")
	}
	source, err := fixture.runtime.Get(fixture.source.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Status != model.WorkloadRunning {
		t.Fatalf("rollback disturbed the source workload: %+v", source)
	}
	if _, err := fixture.repo.Load(record.SourceCheckpointID); err != nil {
		t.Fatalf("rollback removed the source's checkpoint: %v", err)
	}
}

func TestForkRootPathValidation(t *testing.T) {
	const source = "/srv/workload"
	cases := []struct {
		name      string
		requested string
		wantError string
	}{
		{name: "same as source", requested: source, wantError: "must differ"},
		{name: "inside source", requested: "/srv/workload/child", wantError: "nested"},
		{name: "parent of source", requested: "/srv", wantError: "nested"},
		{name: "filesystem root", requested: "/", wantError: "filesystem root"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := forkRootPath(source, testCase.requested, "0123456789abcdef"); err == nil || !strings.Contains(err.Error(), testCase.wantError) {
				t.Fatalf("got %v, want an error containing %q", err, testCase.wantError)
			}
		})
	}
	derived, err := forkRootPath(source, "", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if derived != "/srv/workload-fork-01234567" {
		t.Fatalf("unexpected derived fork root %q", derived)
	}
	explicit, err := forkRootPath(source, "/srv/other", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if explicit != "/srv/other" {
		t.Fatalf("unexpected explicit fork root %q", explicit)
	}
}

func TestForkSpecificationRebasesPathsAndIncrementsGeneration(t *testing.T) {
	source := model.WorkloadSpec{
		ID: "source-id", Name: "source", Command: []string{"/srv/app/run.sh"},
		RootPath: "/srv/app", WorkingDir: "/srv/app/work",
		Paths: []model.PathSpec{
			{Path: "/srv/app", Mode: model.PathReadWrite, Exclusions: []string{"cache"}},
			{Path: "/srv/app/data", Mode: model.PathReadOnly},
		},
		Environment: map[string]string{"MODE": "production"},
		Lineage: &model.Lineage{
			SourceWorkloadID: "grandparent", SourceCheckpointID: "checkpoint-0",
			Generation: 4, ForkedAt: time.Now().UTC(),
		},
		UID: os.Geteuid(), GID: os.Getegid(),
	}
	spec, err := forkSpecification(source, "fork-id", "", "/srv/app-fork", "0123456789abcdef", "checkpoint-7")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != "fork-id" || spec.Name != "source-fork-01234567" {
		t.Fatalf("unexpected fork identity: %+v", spec)
	}
	if spec.RootPath != "/srv/app-fork" || spec.WorkingDir != "/srv/app-fork/work" {
		t.Fatalf("paths were not rebased: %+v", spec)
	}
	if len(spec.Paths) != 2 || spec.Paths[0].Path != "/srv/app-fork" || spec.Paths[1].Path != "/srv/app-fork/data" {
		t.Fatalf("path specs were not rebased: %+v", spec.Paths)
	}
	if len(spec.Paths[0].Exclusions) != 1 || spec.Paths[0].Exclusions[0] != "cache" {
		t.Fatalf("exclusions were lost: %+v", spec.Paths[0])
	}
	if spec.Paths[1].Mode != model.PathReadOnly {
		t.Fatalf("path mode was lost: %+v", spec.Paths[1])
	}
	if spec.Lineage == nil || spec.Lineage.Generation != 5 {
		t.Fatalf("generation was not incremented: %+v", spec.Lineage)
	}
	if spec.Lineage.SourceWorkloadID != "source-id" || spec.Lineage.SourceCheckpointID != "checkpoint-7" {
		t.Fatalf("unexpected lineage: %+v", spec.Lineage)
	}
	spec.Environment["MODE"] = "fork"
	if source.Environment["MODE"] != "production" {
		t.Fatal("fork specification shares the source environment map")
	}
}

func TestRebasePathRejectsPathsOutsideTheSourceRoot(t *testing.T) {
	if got, err := rebasePath("/srv/app", "/srv/app-fork", "/srv/app"); err != nil || got != "/srv/app-fork" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, err := rebasePath("/srv/app", "/srv/app-fork", "/srv/app/data/x"); err != nil || got != "/srv/app-fork/data/x" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := rebasePath("/srv/app", "/srv/app-fork", "/etc/passwd"); err == nil {
		t.Fatal("a path outside the source root must not be rebased")
	}
}
