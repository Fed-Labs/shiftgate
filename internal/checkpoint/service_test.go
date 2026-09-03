package checkpoint

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

type fakeEngine struct{}

func (fakeEngine) Check(context.Context) error                          { return nil }
func (fakeEngine) Version(context.Context) (string, error)              { return "CRIU fake", nil }
func (fakeEngine) PreDump(context.Context, DumpOptions) error           { return nil }
func (fakeEngine) Restore(context.Context, RestoreOptions) (int, error) { return 0, nil }
func (fakeEngine) Dump(_ context.Context, options DumpOptions) error {
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}

type recordingEngine struct {
	preDumpCalls   int
	parentDirs     []string
	parentContents []string
}

func (e *recordingEngine) Check(context.Context) error             { return nil }
func (e *recordingEngine) Version(context.Context) (string, error) { return "CRIU recording", nil }
func (e *recordingEngine) PreDump(_ context.Context, options DumpOptions) error {
	e.preDumpCalls++
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "precopy.img"), []byte("precopy-memory"), 0o600)
}
func (e *recordingEngine) Dump(_ context.Context, options DumpOptions) error {
	if options.ParentImages != "" {
		e.parentDirs = append(e.parentDirs, options.ParentImages)
		entries, err := os.ReadDir(options.ParentImages)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			e.parentContents = append(e.parentContents, entry.Name())
		}
	}
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}
func (*recordingEngine) Restore(context.Context, RestoreOptions) (int, error) { return 0, nil }

func TestCreateCheckpointPipeline(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(workloadRoot, "data.txt"), []byte("persistent state"), 0o600); err != nil {
		t.Fatal(err)
	}
	workload, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "pipeline", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
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
	keys, _ := securestore.Open(stateRoot + "/keys")
	chunks, _ := chunkstore.Open(stateRoot+"/objects", 64<<10, keys)
	repository, _ := OpenRepository(stateRoot+"/checkpoints", keys)
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, fakeEngine{}, logger)
	manifest, err := service.Create(context.Background(), workload.Spec.ID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 2 || manifest.Metrics.PlainBytes == 0 {
		t.Fatalf("incomplete checkpoint manifest: %+v", manifest)
	}
	current, err := runtimeManager.Get(workload.Spec.ID)
	if err != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("source was not resumed: %+v %v", current, err)
	}
	if _, err := repository.Load(manifest.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCreateIncrementalCheckpointUsesRetainedParentImages(t *testing.T) {
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
	workload, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "incremental", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
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
	repository, err := OpenRepository(stateRoot+"/checkpoints", keys)
	if err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, engine, logger)
	parent, err := service.Create(context.Background(), workload.Spec.ID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.Create(context.Background(), workload.Spec.ID, CreateOptions{
		Kind: model.CheckpointIncremental, ParentID: parent.ID, LeaveRunning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if child.Kind != model.CheckpointIncremental || child.ParentID != parent.ID {
		t.Fatalf("unexpected incremental manifest: %+v", child)
	}
	if !child.Engine.ParentImages || len(engine.parentDirs) != 1 {
		t.Fatalf("parent images were not passed to CRIU: manifest=%+v dirs=%v", child.Engine, engine.parentDirs)
	}
	if len(engine.parentContents) == 0 {
		t.Fatal("retained parent image directory was empty")
	}
	if _, err := repository.Load(child.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePreCopyInvokesCRIUPreDump(t *testing.T) {
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
	workload, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "precopy", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
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
	repository, err := OpenRepository(stateRoot+"/checkpoints", keys)
	if err != nil {
		t.Fatal(err)
	}
	engine := &recordingEngine{}
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, engine, logger)
	manifest, err := service.Create(context.Background(), workload.Spec.ID, CreateOptions{PreCopy: true, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if engine.preDumpCalls != 1 || !manifest.Engine.ParentImages {
		t.Fatalf("pre-copy was not recorded: calls=%d engine=%+v", engine.preDumpCalls, manifest.Engine)
	}
	if current, getErr := runtimeManager.Get(workload.Spec.ID); getErr != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("source was not resumed after pre-copy checkpoint: %+v %v", current, getErr)
	}
}
