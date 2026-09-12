package checkpoint

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// cloneEngine stands in for CRIU during clone tests: Restore starts a real
// long-lived process per member (adoption, resume, and liveness run against
// genuine pids) and records the options it was given so the per-member bind
// mounts and image directories can be asserted.
type cloneEngine struct {
	mu       sync.Mutex
	restores []RestoreOptions
	restoreN func() error
}

func (*cloneEngine) Check(context.Context) error             { return nil }
func (*cloneEngine) Version(context.Context) (string, error) { return "CRIU clone", nil }
func (*cloneEngine) PreDump(context.Context, DumpOptions) error {
	return nil
}
func (*cloneEngine) Dump(_ context.Context, options DumpOptions) error {
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}
func (e *cloneEngine) Restore(_ context.Context, options RestoreOptions) (int, error) {
	e.mu.Lock()
	e.restores = append(e.restores, options)
	restoreN := e.restoreN
	e.mu.Unlock()
	if restoreN != nil {
		if err := restoreN(); err != nil {
			return 0, err
		}
	}
	command := exec.Command("/bin/sh", "-c", "while true; do sleep 1; done")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
}

func (e *cloneEngine) restoreOptions() []RestoreOptions {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]RestoreOptions(nil), e.restores...)
}

type cloneFixture struct {
	service *Service
	cloner  *Cloner
	runtime *shiftruntime.Manager
	source  model.Workload
	root    string
}

func newCloneFixture(t *testing.T, engine Engine) cloneFixture {
	t.Helper()
	stubCRIUOnPath(t)
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
	source, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "clone-source", Command: []string{script}, RootPath: workloadRoot,
		WorkingDir: workloadRoot, UID: os.Geteuid(), GID: os.Getegid(),
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
	service.SetNetwork(network.NewCoordinator(logger))
	cloner, err := OpenCloner(service)
	if err != nil {
		t.Fatal(err)
	}
	current, err := runtimeManager.Get(source.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	return cloneFixture{service: service, cloner: cloner, runtime: runtimeManager, source: current, root: workloadRoot}
}

// sourceCheckpoint freezes the fixture's workload into a stopped checkpoint —
// the state a clone set derives from.
func sourceCheckpoint(t *testing.T, fixture cloneFixture) model.CheckpointManifest {
	t.Helper()
	manifest, err := fixture.service.Create(context.Background(), fixture.source.Spec.ID, CreateOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func cleanupClones(t *testing.T, fixture cloneFixture, record CloneRecord) {
	t.Helper()
	for _, member := range record.Members {
		_, _ = fixture.runtime.Stop(member.WorkloadID, time.Second)
		_ = fixture.runtime.Delete(member.WorkloadID)
	}
}

func TestCloneDerivesIndependentRunningWorkloads(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)

	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 3, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer cleanupClones(t, fixture, record)
	if record.State != CloneCommitted {
		t.Fatalf("state %s (error %s), want %s", record.State, record.Error, CloneCommitted)
	}
	if len(record.Members) != 3 {
		t.Fatalf("members: %d, want 3", len(record.Members))
	}
	names := map[string]bool{}
	for _, member := range record.Members {
		if member.PID == 0 {
			t.Fatalf("member %s has no pid", member.Name)
		}
		if member.State != "RUNNING" {
			t.Fatalf("member %s state %s", member.Name, member.State)
		}
		if member.RootPath == fixture.root {
			t.Fatalf("member %s shares the source root", member.Name)
		}
		if info, statErr := os.Stat(member.RootPath); statErr != nil || !info.IsDir() {
			t.Fatalf("member root %s: %v", member.RootPath, statErr)
		}
		workload, getErr := fixture.runtime.Get(member.WorkloadID)
		if getErr != nil {
			t.Fatalf("member workload %s: %v", member.WorkloadID, getErr)
		}
		if workload.Process == nil || workload.Status != model.WorkloadRunning {
			t.Fatalf("member %s workload is not running: %s", member.Name, workload.Status)
		}
		names[member.Name] = true
	}
	if len(names) != 3 {
		t.Fatalf("member names are not unique: %v", names)
	}
	// Every restore got the member's own image directory and the bind mount
	// carrying its root to the source path the images recorded.
	restores := engine.restoreOptions()
	if len(restores) != 3 {
		t.Fatalf("restores: %d, want 3", len(restores))
	}
	for _, options := range restores {
		if len(options.BindMounts) != 1 || options.BindMounts[0].Target != fixture.root {
			t.Fatalf("restore bind mounts %v, want one onto %s", options.BindMounts, fixture.root)
		}
	}
	// The committed set leaves no staging behind.
	if _, statErr := os.Stat(record.SetDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("set directory survived commit: %v", statErr)
	}
}

func TestCloneRollbackRemovesEveryMemberOnFailure(t *testing.T) {
	engine := &cloneEngine{}
	// The third member's restore fails; the first two are already running by
	// then, and the set is still all-or-nothing.
	engine.restoreN = func() error {
		engine.mu.Lock()
		defer engine.mu.Unlock()
		if len(engine.restores) == 3 {
			return fmt.Errorf("injected restore failure")
		}
		return nil
	}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)

	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 3, Parallel: 1, Timeout: time.Minute})
	if err == nil {
		cleanupClones(t, fixture, record)
		t.Fatal("clone with an injected member failure succeeded")
	}
	if record.State != CloneRolledBack {
		t.Fatalf("state %s, want %s", record.State, CloneRolledBack)
	}
	if !strings.Contains(record.Error, "clone member") {
		t.Fatalf("error does not name the member: %s", record.Error)
	}
	// No member workload, root, or process survived.
	for _, member := range record.Members {
		if _, getErr := fixture.runtime.Get(member.WorkloadID); getErr == nil {
			t.Fatalf("member %s workload survived rollback", member.Name)
		}
		if _, statErr := os.Lstat(member.RootPath); !os.IsNotExist(statErr) {
			t.Fatalf("member root %s survived rollback", member.RootPath)
		}
	}
	if _, statErr := os.Stat(record.SetDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("set directory survived rollback: %v", statErr)
	}
	// The source checkpoint is untouched and still restorable — a failed
	// clone never takes the state it was derived from with it.
	loaded, err := fixture.service.repository.Load(manifest.ID)
	if err != nil {
		t.Fatalf("source checkpoint after rollback: %v", err)
	}
	if loaded.ID != manifest.ID {
		t.Fatalf("source checkpoint id changed: %s", loaded.ID)
	}
}

func TestCloneRecoversInterruptedSet(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)

	// A committed set must survive recovery untouched.
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer cleanupClones(t, fixture, record)

	// Forge a set that looks like a crash mid-cloning: a member workload
	// exists in the runtime manager, a root directory is on disk, staging is
	// alive, and the state never reached COMMITTED. Recovery must reverse
	// all of it.
	orphanRoot := filepath.Join(t.TempDir(), "orphan-root")
	if err := os.MkdirAll(orphanRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan, err := fixture.runtime.Create(model.WorkloadSpec{
		Name: "orphan-clone", Command: []string{"/bin/sh", "-c", "while true; do sleep 1; done"},
		RootPath: orphanRoot, UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	setDirectory := filepath.Join(fixture.service.stateDir, "clones", "forged-set")
	if err := os.MkdirAll(setDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	forged := CloneRecord{
		ID: "forged-set", CheckpointID: manifest.ID, SourceWorkloadID: fixture.source.Spec.ID,
		SourceRootPath: fixture.root, Count: 1, Parallel: 1, State: CloneCloning,
		SetDirectory: setDirectory, StagingRoot: filepath.Join(setDirectory, "staging"),
		ImagesRoot: filepath.Join(setDirectory, "images"),
		Members:    []CloneMember{{Index: 1, WorkloadID: orphan.Spec.ID, Name: "orphan-clone", RootPath: orphanRoot, State: "MATERIALIZING", CreatedWorkload: true}},
		CreatedAt:  time.Now().UTC().Add(-time.Minute), UpdatedAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := fixture.cloner.put(forged); err != nil {
		t.Fatal(err)
	}
	if err := fixture.cloner.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	recovered, err := fixture.cloner.Get(forged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != CloneRolledBack {
		t.Fatalf("recovered set state %s, want %s", recovered.State, CloneRolledBack)
	}
	if _, getErr := fixture.runtime.Get(orphan.Spec.ID); getErr == nil {
		t.Fatal("orphan member workload survived recovery")
	}
	if _, statErr := os.Lstat(orphanRoot); !os.IsNotExist(statErr) {
		t.Fatalf("orphan root survived recovery: %v", statErr)
	}
	if _, statErr := os.Stat(setDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("forged set directory survived recovery: %v", statErr)
	}
	// The real committed set is unaffected.
	if committed, getErr := fixture.cloner.Get(record.ID); getErr != nil || committed.State != CloneCommitted {
		t.Fatalf("committed set was disturbed by recovery: %v %s", getErr, committed.State)
	}
}

func TestCloneValidation(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)

	if _, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 0}); err == nil {
		t.Fatal("count 0 was accepted")
	}
	if _, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: MaxCloneCount + 1}); err == nil {
		t.Fatal("over-cap count was accepted")
	}
	if _, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Parallel: 99}); err == nil {
		t.Fatal("over-cap parallelism was accepted")
	}
	// A name colliding with an existing workload is refused before any work
	// starts — the same mistake surfacing mid-set would roll the whole set
	// back instead of erroring in milliseconds. The generated name is
	// prefix-1, so a workload already named taken-1 collides.
	takenRoot := filepath.Join(t.TempDir(), "taken-root")
	if err := os.MkdirAll(takenRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.runtime.Create(model.WorkloadSpec{
		Name: "taken-1", Command: []string{"/bin/sh", "-c", "while true; do sleep 1; done"},
		RootPath: takenRoot, UID: os.Geteuid(), GID: os.Getegid(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, NamePrefix: "taken"}); err == nil {
		t.Fatal("colliding name prefix was accepted")
	}
}

func TestCloneSpecificationDerivesDistinctIdentities(t *testing.T) {
	source := model.WorkloadSpec{
		ID: "source", Name: "web", Command: []string{"/bin/sh"}, RootPath: "/srv/web",
		WorkingDir: "/srv/web/app", UID: 1000, GID: 1000,
		Paths: []model.PathSpec{{Path: "/srv/web/data", Mode: model.PathReadWrite}},
	}
	first, err := cloneSpecification(source, "aaaaaaaabbbbbbbb", "ckpt", 1, "fleet")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cloneSpecification(source, "aaaaaaaabbbbbbbb", "ckpt", 2, "fleet")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.Name == second.Name || first.RootPath == second.RootPath {
		t.Fatalf("members are not distinct: %s/%s vs %s/%s", first.ID, first.RootPath, second.ID, second.RootPath)
	}
	if first.Name != "fleet-1" || second.Name != "fleet-2" {
		t.Fatalf("names: %s %s", first.Name, second.Name)
	}
	if first.RootPath != "/srv/web-clone-aaaaaaaa-1" {
		t.Fatalf("root: %s", first.RootPath)
	}
	if first.WorkingDir != "/srv/web-clone-aaaaaaaa-1/app" {
		t.Fatalf("working dir not rebased: %s", first.WorkingDir)
	}
	if len(first.Paths) != 1 || first.Paths[0].Path != "/srv/web-clone-aaaaaaaa-1/data" {
		t.Fatalf("paths not rebased: %+v", first.Paths)
	}
	if first.Lineage == nil || first.Lineage.SourceCheckpointID != "ckpt" || first.Lineage.SourceWorkloadID != "source" {
		t.Fatalf("lineage: %+v", first.Lineage)
	}
}
