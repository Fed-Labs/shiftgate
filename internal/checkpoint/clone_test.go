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
	// unixSockets makes Dump record a captured unix-socket image beside the
	// pages, the way a checkpoint of a workload with live unix sockets does.
	unixSockets bool
}

func (*cloneEngine) Check(context.Context) error             { return nil }
func (*cloneEngine) Version(context.Context) (string, error) { return "CRIU clone", nil }
func (*cloneEngine) PreDump(context.Context, DumpOptions) error {
	return nil
}
func (e *cloneEngine) Dump(_ context.Context, options DumpOptions) error {
	if err := os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600); err != nil {
		return err
	}
	if e.unixSockets {
		if err := os.WriteFile(filepath.Join(options.ImagesDirectory, "sk-unix-1.img"), []byte("unix-sockets"), 0o600); err != nil {
			return err
		}
	}
	return nil
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
	command := exec.CommandContext(context.Background(), "/bin/sh", "-c", "while true; do sleep 1; done")
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

func newCloneFixture(t *testing.T, engine Engine, specOptions ...func(*model.WorkloadSpec)) cloneFixture {
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
	spec := model.WorkloadSpec{
		Name: "clone-source", Command: []string{script}, RootPath: workloadRoot,
		WorkingDir: workloadRoot, UID: os.Geteuid(), GID: os.Getegid(),
	}
	for _, apply := range specOptions {
		apply(&spec)
	}
	source, err := runtimeManager.Create(spec)
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
	// Test roots live under the OS temp directory, which production policy
	// denies outright; the explicit operator override is what admits them.
	if err := service.SetRestorePolicy(RestorePolicy{AllowRootWorkload: true, AllowSystemTreeTargets: []string{os.TempDir()}}); err != nil {
		t.Fatal(err)
	}
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

func TestCloneSpecificationRebasesCommandOntoMemberRoot(t *testing.T) {
	source := model.WorkloadSpec{
		ID: "source", Name: "web", Command: []string{"/srv/web/run.sh", "/srv/web/conf.json", "python3"},
		RootPath: "/srv/web", WorkingDir: "/srv/web", UID: 1000, GID: 1000,
	}
	member, err := cloneSpecification(source, "aaaaaaaabbbbbbbb", "ckpt", 1, "fleet")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/srv/web-clone-aaaaaaaa-1/run.sh", "/srv/web-clone-aaaaaaaa-1/conf.json", "python3"}
	if len(member.Command) != len(want) {
		t.Fatalf("command: %v, want %v", member.Command, want)
	}
	for index := range want {
		if member.Command[index] != want[index] {
			t.Fatalf("command element %d is %q, want %q", index, member.Command[index], want[index])
		}
	}
}

// TestCloneCommandRebasedRestartRunsMemberRootFile is the independence
// regression for clone members: a stop→start executes the member root's own
// file, never the source tree's, even after the source's file is edited.
func TestCloneCommandRebasedRestartRunsMemberRootFile(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	marker := "#!/bin/sh\nrm -f \"$PWD/ran.txt\"\necho \"$0\" > \"$PWD/ran.txt\"\nwhile true; do sleep 1; done\n"
	waitScriptStarted(t, fixture.source.Process.PID, filepath.Join(fixture.root, "run.sh"))
	writeScriptAtomic(t, filepath.Join(fixture.root, "run.sh"), marker)
	manifest := sourceCheckpoint(t, fixture)
	writeScriptAtomic(t, filepath.Join(fixture.root, "run.sh"), "#!/bin/sh\necho SOURCE-RAN > \"$PWD/ran.txt\"\nwhile true; do sleep 1; done\n")
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer cleanupClones(t, fixture, record)
	member := record.Members[0]
	t.Cleanup(func() { _, _ = fixture.runtime.Stop(member.WorkloadID, time.Second) })
	running, err := fixture.runtime.Get(member.WorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := filepath.Join(member.RootPath, "run.sh")
	if len(running.Spec.Command) == 0 || running.Spec.Command[0] != wantCommand {
		t.Fatalf("member command was not rebased onto its root: %v, want %q", running.Spec.Command, wantCommand)
	}
	if _, err := fixture.runtime.Stop(member.WorkloadID, time.Second); err != nil {
		t.Fatalf("stop the member: %v", err)
	}
	if _, err := fixture.runtime.Start(member.WorkloadID); err != nil {
		t.Fatalf("restart the member: %v", err)
	}
	ranPath := filepath.Join(member.RootPath, "ran.txt")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if content, readErr := os.ReadFile(ranPath); readErr == nil {
			got := strings.TrimSpace(string(content))
			if got == "" {
				if time.Now().After(deadline) {
					t.Fatal("the member never executed its command")
				}
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if got != wantCommand {
				t.Fatalf("member executed %q, want its own root's file %q", got, wantCommand)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the member never executed its command")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.root, "ran.txt")); statErr == nil {
		t.Fatal("the restart wrote into the source root; the member still runs the source tree's files")
	}
}

// TestCloneSinglePortCloneFailsEarlyOnLivePortConflict pins the same contract
// a fork gets: cloning beside the still-running original that publishes the
// ports fails explicitly before anything is copied.
func TestCloneSinglePortCloneFailsEarlyOnLivePortConflict(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine, func(spec *model.WorkloadSpec) {
		spec.Ports = []model.PortSpec{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}
	})
	// The fork point of a live source: the workload keeps running and keeps
	// its published claim, exactly as the agent holds it while it runs.
	manifest, err := fixture.service.Create(context.Background(), fixture.source.Spec.ID, CreateOptions{LeaveRunning: true, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	sourceMappings, err := network.MappingsFor(manifest.Workload)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.network.Prepare(manifest.Workload.ID, sourceMappings); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err == nil {
		cleanupClones(t, fixture, record)
		t.Fatal("cloning beside the live published source must fail")
	}
	if !strings.Contains(err.Error(), "port conflict") || !strings.Contains(err.Error(), "18080") {
		t.Fatalf("the failure must name the conflicting port explicitly, got: %v", err)
	}
	if record.ID != "" {
		t.Fatalf("the refusal must happen before any clone state is created: %+v", record)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.service.stateDir, "clones")); statErr == nil {
		t.Fatal("nothing may be extracted before the conflict error")
	}
}

// TestCloneSinglePortCloneClaimsSourcePublishedPorts proves the member's
// reservation lands on the published host port as well as its own listener
// port, and never leaks past the set's teardown.
func TestCloneSinglePortCloneClaimsSourcePublishedPorts(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine, func(spec *model.WorkloadSpec) {
		spec.Ports = []model.PortSpec{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}
	})
	manifest := sourceCheckpoint(t, fixture)
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	member := record.Members[0]
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 8080}}); err == nil {
		t.Fatal("the member's own listener port must be claimed")
	}
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}); err == nil {
		t.Fatal("the source's published host port must be claimed beside the member's own")
	}
	cleanupClones(t, fixture, record)
	fixture.service.network.Deactivate(member.WorkloadID, 0)
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}); err != nil {
		t.Fatalf("released ports must be claimable again: %v", err)
	}
	fixture.service.network.Deactivate("intruder", 0)
}

func TestCloneNameCollisionIsCaseInsensitive(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)
	takenRoot := filepath.Join(t.TempDir(), "taken-root")
	if err := os.MkdirAll(takenRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.runtime.Create(model.WorkloadSpec{
		Name: "Taken-1", Command: []string{"/bin/sh", "-c", "while true; do sleep 1; done"},
		RootPath: takenRoot, UID: os.Geteuid(), GID: os.Getegid(),
	}); err != nil {
		t.Fatal(err)
	}
	// The runtime rejects names case-insensitively; the pre-flight must too,
	// or the mistake surfaces as a full mid-set rollback.
	if _, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, NamePrefix: "taken"}); err == nil {
		t.Fatal("a case-variant name collision was accepted")
	}
}

func TestCloneRefusesMultiMemberWithCapturedTCPState(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest, err := fixture.service.Create(context.Background(), fixture.source.Spec.ID, CreateOptions{TCPState: true, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Engine.TCPState {
		t.Fatal("the test checkpoint must record captured TCP state")
	}
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 2, Timeout: time.Minute})
	if err == nil {
		cleanupClones(t, fixture, record)
		t.Fatal("a checkpoint with captured TCP state must not fan out to many members")
	}
	if !strings.Contains(err.Error(), "rebind") || !strings.Contains(err.Error(), "one at a time") {
		t.Fatalf("the refusal must explain the per-machine socket rule, got: %v", err)
	}
	if record.ID != "" {
		t.Fatalf("the refusal must happen before any clone state is created: %+v", record)
	}
}

func TestCloneRefusesMultiMemberWithCapturedUnixSockets(t *testing.T) {
	engine := &cloneEngine{unixSockets: true}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 2, Timeout: time.Minute})
	if err == nil {
		cleanupClones(t, fixture, record)
		t.Fatal("a checkpoint with captured unix sockets must not fan out to many members")
	}
	if !strings.Contains(err.Error(), "unix") || !strings.Contains(err.Error(), "one at a time") {
		t.Fatalf("the refusal must explain the per-machine socket rule, got: %v", err)
	}
	if record.State != CloneRolledBack {
		t.Fatalf("state %s (error %s), want %s", record.State, record.Error, CloneRolledBack)
	}
	// One member is exactly a restore's posture and stays allowed.
	single, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("a single clone must still be allowed: %v", err)
	}
	cleanupClones(t, fixture, single)
}

func TestCloneCountFiftyIndependentWorkers(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 50, Parallel: MaxCloneParallel, Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatalf("clone count 50: %v", err)
	}
	defer cleanupClones(t, fixture, record)
	if record.State != CloneCommitted {
		t.Fatalf("state %s (error %s), want %s", record.State, record.Error, CloneCommitted)
	}
	if len(record.Members) != 50 {
		t.Fatalf("members: %d, want 50", len(record.Members))
	}
	names := map[string]bool{}
	for _, member := range record.Members {
		if member.State != "RUNNING" || member.PID == 0 {
			t.Fatalf("member %d is not running: %+v", member.Index, member)
		}
		if names[member.Name] {
			t.Fatalf("duplicate member name %q", member.Name)
		}
		names[member.Name] = true
		if workload, getErr := fixture.runtime.Get(member.WorkloadID); getErr != nil || workload.Process == nil {
			t.Fatalf("member %d workload: %+v %v", member.Index, workload, getErr)
		}
	}
	if _, statErr := os.Stat(record.SetDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("set directory survived commit: %v", statErr)
	}
}

func TestCloneFromReadOnlySourceRoot(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	if err := os.WriteFile(filepath.Join(fixture.root, "data", "state.txt"), []byte("source state"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := sourceCheckpoint(t, fixture)
	if err := os.Chmod(fixture.root, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(fixture.root, 0o755) }()
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 2, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone from a read-only source root: %v", err)
	}
	defer cleanupClones(t, fixture, record)
	for _, member := range record.Members {
		if content, readErr := os.ReadFile(filepath.Join(member.RootPath, "data", "state.txt")); readErr != nil || string(content) != "source state" {
			t.Fatalf("member %s lost the source state: %q %v", member.Name, content, readErr)
		}
	}
}

func TestForkFromReadOnlySourceRoot(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	// The fork point is captured while the source is writable; the source is
	// then made read-only and the fork runs against it — the fork must only
	// read the source and write everything of its own beside it.
	point, err := fixture.service.Create(context.Background(), fixture.source.Spec.ID, CreateOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.rootPath, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(fixture.rootPath, 0o755) }()
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-readonly", CheckpointID: point.ID})
	if err != nil {
		t.Fatalf("fork from a read-only source root: %v", err)
	}
	t.Cleanup(func() { _ = fixture.runtime.Delete(record.ForkWorkloadID) })
	if content, readErr := os.ReadFile(filepath.Join(record.RootPath, "data", "state.txt")); readErr != nil || string(content) != "forked state" {
		t.Fatalf("fork lost the source state: %q %v", content, readErr)
	}
}

// TestCloneFilesystemClonedAccountingOnCopyOnlyFilesystem pins the
// FilesystemCloned claim to the real copy-on-write outcome: on a filesystem
// without reflink support (where every file is byte-copied) the record must
// report copied files and must not claim shared extents.
func TestCloneFilesystemClonedAccountingOnCopyOnlyFilesystem(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	if err := os.WriteFile(filepath.Join(fixture.root, "data", "state.txt"), []byte("source state"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := sourceCheckpoint(t, fixture)
	record, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer cleanupClones(t, fixture, record)
	member := record.Members[0]
	if record.ClonedFiles+record.CopiedFiles == 0 {
		t.Fatal("no files were accounted")
	}
	fullyCloned := record.CopiedFiles == 0 && record.ClonedFiles > 0
	if member.FilesystemCloned != fullyCloned {
		t.Fatalf("FilesystemCloned=%v but cloned=%d copied=%d", member.FilesystemCloned, record.ClonedFiles, record.CopiedFiles)
	}
	if content, readErr := os.ReadFile(filepath.Join(member.RootPath, "data", "state.txt")); readErr != nil || string(content) != "source state" {
		t.Fatalf("copied member lost content fidelity: %q %v", content, readErr)
	}
}

// TestCloneRecoversEverySetDespiteRollbackError: one stuck rollback must not
// strand the later interrupted sets — all are attempted, failures collected.
func TestCloneRecoversEverySetDespiteRollbackError(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)
	committed, err := fixture.cloner.Clone(context.Background(), manifest.ID, CloneOptions{Count: 1, Timeout: time.Minute})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	defer cleanupClones(t, fixture, committed)
	now := time.Now().UTC()
	stuckSet := filepath.Join(fixture.service.stateDir, "clones", "stuck-set")
	if err := os.MkdirAll(stuckSet, 0o700); err != nil {
		t.Fatal(err)
	}
	stuck := CloneRecord{
		ID: "stuck-set", CheckpointID: manifest.ID, SourceWorkloadID: fixture.source.Spec.ID,
		SourceRootPath: fixture.root, Count: 1, Parallel: 1, State: CloneCloning,
		SetDirectory: stuckSet, StagingRoot: filepath.Join(stuckSet, "staging"), ImagesRoot: filepath.Join(stuckSet, "images"),
		// A target whose parent is a file fails RemoveAll on every uid —
		// a deterministic rollback failure.
		Members:   []CloneMember{{Index: 1, WorkloadID: "ghost", Name: "ghost", RootPath: "/dev/null/nope", State: "MATERIALIZING"}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.cloner.put(stuck); err != nil {
		t.Fatal(err)
	}
	orphanRoot := filepath.Join(t.TempDir(), "orphan-root")
	if err := os.MkdirAll(orphanRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	orphanSet := filepath.Join(fixture.service.stateDir, "clones", "orphan-set")
	if err := os.MkdirAll(orphanSet, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := CloneRecord{
		ID: "orphan-set", CheckpointID: manifest.ID, SourceWorkloadID: fixture.source.Spec.ID,
		SourceRootPath: fixture.root, Count: 1, Parallel: 1, State: CloneCloning,
		SetDirectory: orphanSet, StagingRoot: filepath.Join(orphanSet, "staging"), ImagesRoot: filepath.Join(orphanSet, "images"),
		Members:   []CloneMember{{Index: 1, WorkloadID: "ghost-2", Name: "ghost-2", RootPath: orphanRoot, State: "MATERIALIZING"}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.cloner.put(orphan); err != nil {
		t.Fatal(err)
	}
	recoverErr := fixture.cloner.Recover(context.Background())
	if recoverErr == nil {
		t.Fatal("the stuck rollback must be reported")
	}
	if !strings.Contains(recoverErr.Error(), stuck.ID) {
		t.Fatalf("the aggregate must name the set that failed: %v", recoverErr)
	}
	rolled, err := fixture.cloner.Get(orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.State != CloneRolledBack {
		t.Fatalf("the later interrupted set was stranded by the stuck one: %s", rolled.State)
	}
	if _, statErr := os.Stat(orphanRoot); !os.IsNotExist(statErr) {
		t.Fatalf("the later set's root survived recovery: %v", statErr)
	}
	if live, getErr := fixture.cloner.Get(committed.ID); getErr != nil || live.State != CloneCommitted {
		t.Fatalf("the committed set was disturbed by recovery: %+v %v", live, getErr)
	}
	if opened, openErr := OpenCloner(fixture.service); openErr != nil || opened == nil {
		t.Fatalf("clone recovery problems must not block startup: %v", openErr)
	}
}

// TestCloneRecoverCommitsFullyRunningSet: a set whose members all reached
// RUNNING is built, not half-built — recovery must finish its commit instead
// of destroying healthy workloads over a record-store hiccup.
func TestCloneRecoverCommitsFullyRunningSet(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)
	orphanRoot := filepath.Join(t.TempDir(), "healthy-root")
	if err := os.MkdirAll(orphanRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan, err := fixture.runtime.Create(model.WorkloadSpec{
		Name: "healthy-member", Command: []string{"/bin/sh", "-c", "while true; do sleep 1; done"},
		RootPath: orphanRoot, UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	setDirectory := filepath.Join(fixture.service.stateDir, "clones", "healthy-set")
	if err := os.MkdirAll(setDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	forged := CloneRecord{
		ID: "healthy-set", CheckpointID: manifest.ID, SourceWorkloadID: fixture.source.Spec.ID,
		SourceRootPath: fixture.root, Count: 1, Parallel: 1, State: CloneCloning,
		SetDirectory: setDirectory, StagingRoot: filepath.Join(setDirectory, "staging"),
		ImagesRoot: filepath.Join(setDirectory, "images"),
		Members: []CloneMember{{
			Index: 1, WorkloadID: orphan.Spec.ID, Name: "healthy-member", RootPath: orphanRoot,
			State: "RUNNING", CreatedWorkload: true, RestoreAttempted: true,
		}},
		CreatedAt: now, UpdatedAt: now,
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
	if recovered.State != CloneCommitted {
		t.Fatalf("a fully running set must be committed by recovery, got %s (%s)", recovered.State, recovered.Error)
	}
	if _, getErr := fixture.runtime.Get(orphan.Spec.ID); getErr != nil {
		t.Fatalf("recovery destroyed a healthy running member: %v", getErr)
	}
	if _, statErr := os.Stat(orphanRoot); statErr != nil {
		t.Fatalf("recovery destroyed a healthy member root: %v", statErr)
	}
	if _, statErr := os.Stat(setDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("commit did not clean the set staging: %v", statErr)
	}
}

// TestCloneCommitSurvivesStagingCleanupFailure: once every member is running,
// a cleanup hiccup is a janitor problem — the set commits anyway and keeps
// its members.
func TestCloneCommitSurvivesStagingCleanupFailure(t *testing.T) {
	engine := &cloneEngine{}
	fixture := newCloneFixture(t, engine)
	manifest := sourceCheckpoint(t, fixture)
	// A set directory whose contents cannot be removed (read-only holder).
	setDirectory := filepath.Join(t.TempDir(), "set")
	locked := filepath.Join(setDirectory, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "leftover.img"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(locked, 0o700) }()
	now := time.Now().UTC()
	record := CloneRecord{
		ID: "cleanup-set", CheckpointID: manifest.ID, SourceWorkloadID: fixture.source.Spec.ID,
		SourceRootPath: fixture.root, Count: 1, Parallel: 1, State: CloneCloning,
		SetDirectory: setDirectory, StagingRoot: filepath.Join(setDirectory, "staging"),
		ImagesRoot: filepath.Join(setDirectory, "images"),
		Members: []CloneMember{{
			Index: 1, WorkloadID: "running-member", Name: "running-member", RootPath: filepath.Join(t.TempDir(), "root"),
			State: "RUNNING", CreatedWorkload: true, RestoreAttempted: true,
		}},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.cloner.put(record); err != nil {
		t.Fatal(err)
	}
	if err := fixture.cloner.commit(&record); err != nil {
		t.Fatalf("staging cleanup failure must not fail the commit: %v", err)
	}
	if record.State != CloneCommitted {
		t.Fatalf("state %s, want %s", record.State, CloneCommitted)
	}
}
