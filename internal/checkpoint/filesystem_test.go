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

// filesystemFixture is a running workload plus everything a checkpoint needs.
type filesystemFixture struct {
	service  *Service
	runtime  *shiftruntime.Manager
	workload model.Workload
	root     string
	state    string
}

func newFilesystemFixture(t *testing.T) filesystemFixture {
	t.Helper()
	stateRoot := t.TempDir()
	workloadRoot := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(filepath.Join(stateRoot, "runtime"), false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workloadRoot, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workloadRoot, "data.txt"), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workloadRoot, "doomed.txt"), []byte("delete me"), 0o600); err != nil {
		t.Fatal(err)
	}
	workload, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "filesystem", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeManager.Start(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = runtimeManager.Stop(workload.Spec.ID, time.Second) })
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
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, fakeEngine{}, logger)
	return filesystemFixture{service: service, runtime: runtimeManager, workload: workload, root: workloadRoot, state: stateRoot}
}

func TestCheckpointRecordsFilesystemCaptureAndDependencies(t *testing.T) {
	fixture := newFilesystemFixture(t)
	manifest, err := fixture.service.Create(context.Background(), fixture.workload.Spec.ID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Filesystem.Filesystem == "" {
		t.Fatal("the manifest must name the filesystem the root lives on")
	}
	if manifest.Filesystem.Snapshot == "" && !manifest.Filesystem.FrozenCapture {
		t.Fatal("capture must be recorded as snapshotted or frozen, never implied")
	}
	if manifest.Filesystem.ChangedFiles != 0 || manifest.Filesystem.AddedFiles != 0 || manifest.Filesystem.DeletedFiles != 0 {
		t.Fatalf("the first checkpoint has no baseline to diff against: %+v", manifest.Filesystem)
	}
	if len(manifest.Dependencies) == 0 {
		t.Fatal("dependency discovery must record the command's dependencies")
	}
	scriptInside := false
	outside := 0
	for _, dependency := range manifest.Dependencies {
		if dependency.Path == filepath.Join(fixture.root, "run.sh") && dependency.InsideRoot {
			scriptInside = true
		}
		if !dependency.InsideRoot {
			outside++
		}
	}
	if !scriptInside {
		t.Fatalf("the script inside the root must be recorded as a dependency: %+v", manifest.Dependencies)
	}
	if outside == 0 {
		t.Fatalf("the interpreter and libraries outside the root must be recorded: %+v", manifest.Dependencies)
	}
}

func TestSecondCheckpointReportsChangedFiles(t *testing.T) {
	fixture := newFilesystemFixture(t)
	first, err := fixture.service.Create(context.Background(), fixture.workload.Spec.ID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "data.txt"), []byte("v2 with more bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "fresh.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.root, "doomed.txt")); err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.Create(context.Background(), fixture.workload.Spec.ID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	capture := second.Filesystem
	if capture.ChangedFiles != 1 || capture.AddedFiles != 1 || capture.DeletedFiles != 1 {
		t.Fatalf("changed-file tracking missed the edit, addition, and deletion: %+v", capture)
	}
	if capture.ChangedFiles+capture.AddedFiles+capture.DeletedFiles == 0 || first.ID == second.ID {
		t.Fatal("unexpected checkpoint sequence")
	}
	// The persisted index must match the captured tree: a third checkpoint
	// with no further edits reports no changes at all.
	third, err := fixture.service.Create(context.Background(), fixture.workload.Spec.ID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if third.Filesystem.ChangedFiles != 0 || third.Filesystem.AddedFiles != 0 || third.Filesystem.DeletedFiles != 0 {
		t.Fatalf("an unchanged tree must report an empty diff: %+v", third.Filesystem)
	}
}

func TestDiscardIndexRemovesTheBaseline(t *testing.T) {
	fixture := newFilesystemFixture(t)
	if _, err := fixture.service.Create(context.Background(), fixture.workload.Spec.ID, CreateOptions{LeaveRunning: true}); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(fixture.state, "indices", fixture.workload.Spec.ID+".json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("the checkpoint must persist its file index: %v", err)
	}
	fixture.service.DiscardIndex(fixture.workload.Spec.ID)
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("the index must be gone after discard: %v", err)
	}
	// Discarding an unknown workload's index is not an error.
	fixture.service.DiscardIndex("never-existed")
}

func TestCrossDeviceMaterializationFallsBackToClone(t *testing.T) {
	staging := t.TempDir()
	target := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(filepath.Join(staging, "payload.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// staging and target are both real directories; the rename path succeeds
	// when the two temp directories share a filesystem, which is the common
	// case in tests. The clone fallback itself is covered by CloneTree tests.
	if err := materializeRoot(staging, target); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(target, "payload.txt"))
	if err != nil || string(content) != "payload" {
		t.Fatalf("the payload did not arrive: %q %v", content, err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("the staged copy must be gone after materialization")
	}
}
