package checkpoint

import (
	"context"
	"errors"
	"fmt"
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
	command := exec.CommandContext(context.Background(), "/bin/sh", "-c", "while true; do sleep 1; done")
	// A CRIU-restored tree leads its own session and process group, exactly
	// like a workload the manager starts; reproducing that here keeps the
	// runtime manager's group-wide signaling scoped to the fake workload.
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
	// Test roots live under the OS temp directory, which production policy
	// denies outright; the explicit operator override is what admits them.
	if err := service.SetRestorePolicy(RestorePolicy{AllowRootWorkload: true, AllowSystemTreeTargets: []string{os.TempDir()}}); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("explicit fork point was not honored: %+v", second)
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
	if len(spec.Command) != 1 || spec.Command[0] != "/srv/app-fork/run.sh" {
		t.Fatalf("command was not rebased onto the fork root: %v", spec.Command)
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

func TestForkSpecificationKeepsHostLevelCommands(t *testing.T) {
	source := model.WorkloadSpec{
		ID: "source-id", Name: "source",
		Command:  []string{"python3", "/srv/app/writer.py", "/usr/bin/env", "--config=/srv/app/cfg", "relative.txt"},
		RootPath: "/srv/app", WorkingDir: "/srv/app", UID: os.Geteuid(), GID: os.Getegid(),
	}
	spec, err := forkSpecification(source, "fork-id", "fork", "/srv/app-fork", "0123456789abcdef", "checkpoint-7")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"python3", "/srv/app-fork/writer.py", "/usr/bin/env", "--config=/srv/app/cfg", "relative.txt"}
	if len(spec.Command) != len(want) {
		t.Fatalf("command: %v, want %v", spec.Command, want)
	}
	for index := range want {
		if spec.Command[index] != want[index] {
			t.Fatalf("command element %d is %q, want %q", index, spec.Command[index], want[index])
		}
	}
}

// writeScriptAtomic replaces a script file the way an editor would: a rename,
// never a truncate-in-place. Overwriting a script a running shell is still
// reading truncates the file under the shell and either kills its loop or
// makes it re-read the new content mid-run.
func writeScriptAtomic(t *testing.T, path, content string) {
	t.Helper()
	temp := path + ".new"
	if err := os.WriteFile(temp, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
}

// waitScriptStarted blocks until the workload's shell has its script open.
// The shell opens the script path shortly after the process is spawned, so a
// rename that replaces the script earlier can be picked up by the workload
// itself instead of the restart under test.
func waitScriptStarted(t *testing.T, pid int, script string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid)); err == nil {
			for _, entry := range entries {
				if target, linkErr := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name())); linkErr == nil && target == script {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload %d never opened its script %s", pid, script)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestForkCommandRebasedRestartRunsForkRootFile is the independence
// regression: after a stop→start the fork must execute its own root's file,
// never the source tree's — even when the source's file is edited afterwards.
func TestForkCommandRebasedRestartRunsForkRootFile(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	marker := "#!/bin/sh\nrm -f \"$PWD/ran.txt\"\necho \"$0\" > \"$PWD/ran.txt\"\nwhile true; do sleep 1; done\n"
	waitScriptStarted(t, fixture.source.Process.PID, filepath.Join(fixture.rootPath, "run.sh"))
	writeScriptAtomic(t, filepath.Join(fixture.rootPath, "run.sh"), marker)
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-restart"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.runtime.Stop(record.ForkWorkloadID, time.Second)
		_ = fixture.runtime.Delete(record.ForkWorkloadID)
	})
	// Poison the source's script after the fork: the fork must not notice.
	writeScriptAtomic(t, filepath.Join(fixture.rootPath, "run.sh"), "#!/bin/sh\necho SOURCE-RAN > \"$PWD/ran.txt\"\nwhile true; do sleep 1; done\n")
	forked, err := fixture.runtime.Get(record.ForkWorkloadID)
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := filepath.Join(record.RootPath, "run.sh")
	if len(forked.Spec.Command) == 0 || forked.Spec.Command[0] != wantCommand {
		t.Fatalf("fork command was not rebased onto the fork root: %v, want %q", forked.Spec.Command, wantCommand)
	}
	if _, err := fixture.runtime.Start(record.ForkWorkloadID); err != nil {
		t.Fatalf("restart the fork: %v", err)
	}
	t.Cleanup(func() { _, _ = fixture.runtime.Stop(record.ForkWorkloadID, time.Second) })
	ranPath := filepath.Join(record.RootPath, "data", "ran.txt")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if content, readErr := os.ReadFile(ranPath); readErr == nil {
			got := strings.TrimSpace(string(content))
			if got == "" {
				// The writer truncates then writes; an empty read is a
				// not-yet, not an answer.
				if time.Now().After(deadline) {
					t.Fatal("the fork never executed its command")
				}
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if got != wantCommand {
				t.Fatalf("fork executed %q, want its own root's file %q", got, wantCommand)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fork never executed its command")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if content, statErr := os.ReadFile(filepath.Join(fixture.rootPath, "data", "ran.txt")); statErr == nil {
		t.Fatalf("the restart wrote into the source root: %q", string(content))
	}
}

// TestForkActivateFailsEarlyOnLivePortConflict pins the documented contract:
// forking a port-publishing workload beside the still-running original fails
// with an explicit port-conflict error before anything is copied — never
// after the full extraction and capture, and never inside the CRIU restore.
func TestForkActivateFailsEarlyOnLivePortConflict(t *testing.T) {
	fixture := newForkFixture(t, &activatingEngine{})
	sourceMappings, err := network.MappingsFor(fixture.source.Spec)
	if err != nil {
		t.Fatal(err)
	}
	// The running source's published claim, exactly as the agent holds it.
	if err := fixture.service.network.Prepare(fixture.source.Spec.ID, sourceMappings); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-conflict", Activate: true})
	if err == nil {
		t.Fatal("forking beside the live published source must fail")
	}
	if !strings.Contains(err.Error(), "port conflict") || !strings.Contains(err.Error(), "18080") {
		t.Fatalf("the failure must name the conflicting port explicitly, got: %v", err)
	}
	if record.ID != "" {
		t.Fatalf("the refusal must happen before any fork state is created: %+v", record)
	}
	matches, globErr := filepath.Glob(fixture.rootPath + "-fork-*")
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("nothing may be copied before the conflict error, found: %v", matches)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.service.stateDir, "forks")); statErr == nil {
		t.Fatal("no fork images may be extracted before the conflict error")
	}
}

// TestForkActivateClaimsSourcePublishedPorts proves the reservation is taken
// at the published host port with the container port recorded — not at the
// container port alone, which conflicts with nothing when host != container.
func TestForkActivateClaimsSourcePublishedPorts(t *testing.T) {
	fixture := newForkFixture(t, &activatingEngine{})
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-claims", Activate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.runtime.Stop(record.ForkWorkloadID, time.Second)
		_ = fixture.runtime.Delete(record.ForkWorkloadID)
	})
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 8080}}); err == nil {
		t.Fatal("the fork's own listener port must be claimed")
	}
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}); err == nil {
		t.Fatal("the source's published host port must be claimed beside the fork's own")
	}
	// Releasing the fork gives the ports back — the claim never leaks.
	fixture.service.network.Deactivate(record.ForkWorkloadID, 0)
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}); err != nil {
		t.Fatalf("released ports must be claimable again: %v", err)
	}
	fixture.service.network.Deactivate("intruder", 0)
}

// rejectingEngine fails every restore, so the fork is rolled back after the
// port claim was taken — the claim must roll back with it.
type rejectingEngine struct{ fakeEngine }

func (rejectingEngine) Restore(context.Context, RestoreOptions) (int, error) {
	return 0, errors.New("injected restore failure")
}

func TestForkRollbackReleasesPortClaim(t *testing.T) {
	fixture := newForkFixture(t, rejectingEngine{})
	record, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-doomed-ports", Activate: true})
	if err == nil {
		t.Fatal("the injected restore failure must fail the fork")
	}
	if record.State != ForkRolledBack {
		t.Fatalf("fork state %s, want %s", record.State, ForkRolledBack)
	}
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 18080}}); err != nil {
		t.Fatalf("a rolled-back fork must not keep the source's published port: %v", err)
	}
	fixture.service.network.Deactivate("intruder", 0)
	if err := fixture.service.network.Prepare("intruder", []model.PortMapping{{Protocol: "tcp", ContainerPort: 8080, HostPort: 8080}}); err != nil {
		t.Fatalf("a rolled-back fork must not keep its own port: %v", err)
	}
	fixture.service.network.Deactivate("intruder", 0)
	if _, loadErr := fixture.repo.Load(record.ForkCheckpointID); loadErr == nil {
		t.Fatal("the rolled-back fork's checkpoint must be gone")
	}
}

// TestForkRecoverReversesCrashWindowRecord is the crash-window regression: a
// fork that died mid-materialize left the workload record and the fork
// checkpoint on disk, and recovery must remove exactly those — the record
// names them because materialize persists each mutation the moment it
// happens, not at the next state change after the heavy capture and mirror.
func TestForkRecoverReversesCrashWindowRecord(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	point, err := fixture.service.Create(context.Background(), fixture.source.Spec.ID, CreateOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	orphanRoot := filepath.Join(t.TempDir(), "crash-root")
	if err := os.MkdirAll(orphanRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan, err := fixture.runtime.Create(model.WorkloadSpec{
		Name: "crash-fork", Command: []string{"/bin/sh", "-c", "while true; do sleep 1; done"},
		RootPath: orphanRoot, UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The fork's own checkpoint, saved under the fork's key namespace the way
	// captureFork saves it before the crash.
	forkManifest := point
	forkCheckpointID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	forkManifest.ID = forkCheckpointID
	forkManifest.Workload = orphan.Spec
	if err := SignManifest(&forkManifest, fixture.service.identity); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repo.Save(forkManifest); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Exactly what materialize persists between PrepareRestore and the
	// MATERIALIZED transition when the agent dies mid-capture.
	forged := ForkRecord{
		ID: "crash-fork-record", SourceWorkloadID: fixture.source.Spec.ID, SourceCheckpointID: point.ID,
		SourceRootPath: fixture.rootPath, ForkWorkloadID: orphan.Spec.ID, ForkName: "crash-fork",
		State: ForkPreparing, RootPath: orphanRoot, StagingRoot: filepath.Join(t.TempDir(), "crash-staging"),
		ImagesDirectory:  filepath.Join(t.TempDir(), "crash-images"),
		ForkCheckpointID: forkManifest.ID, CreatedWorkload: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.forker.records.Put(forged.ID, forged); err != nil {
		t.Fatal(err)
	}
	if err := fixture.forker.Recover(context.Background()); err != nil {
		t.Fatalf("recover: %v", err)
	}
	rolled, err := fixture.forker.Get(forged.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.State != ForkRolledBack {
		t.Fatalf("crash-window fork state %s, want %s", rolled.State, ForkRolledBack)
	}
	if _, getErr := fixture.runtime.Get(orphan.Spec.ID); getErr == nil {
		t.Fatal("the crash-window fork workload survived recovery")
	}
	if _, statErr := os.Stat(orphanRoot); !os.IsNotExist(statErr) {
		t.Fatalf("the crash-window fork root survived recovery: %v", statErr)
	}
	if _, loadErr := fixture.repo.Load(forkManifest.ID); loadErr == nil {
		t.Fatal("the crash-window fork checkpoint survived recovery")
	}
	if _, loadErr := fixture.repo.Load(point.ID); loadErr != nil {
		t.Fatalf("recovery removed the source checkpoint: %v", loadErr)
	}
}

// TestForkRecoversEveryRecordDespiteRollbackError pins the startup contract:
// one stuck rollback must not strand every later interrupted fork or block
// the agent from booting — all records are attempted and the failures are
// collected.
func TestForkRecoversEveryRecordDespiteRollbackError(t *testing.T) {
	fixture := newForkFixture(t, fakeEngine{})
	committed, err := fixture.forker.Fork(context.Background(), fixture.source.Spec.ID, ForkOptions{Name: "fork-safe"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.runtime.Delete(committed.ForkWorkloadID) })

	stuckRoot := filepath.Join(t.TempDir(), "stuck-root")
	if err := os.MkdirAll(stuckRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stuck := ForkRecord{
		ID: "stuck-fork", SourceWorkloadID: fixture.source.Spec.ID, SourceCheckpointID: committed.SourceCheckpointID,
		SourceRootPath: fixture.rootPath, ForkWorkloadID: "ghost-workload", ForkName: "stuck",
		State: ForkMaterialized, RootPath: stuckRoot, StagingRoot: filepath.Join(t.TempDir(), "stuck-staging"),
		ImagesDirectory: filepath.Join(t.TempDir(), "stuck-images"),
		// The rollback's repository.Delete fails on a checkpoint that never
		// existed — a deterministic rollback failure on any uid.
		ForkCheckpointID: "missing-checkpoint",
		CreatedAt:        now, UpdatedAt: now,
	}
	if err := fixture.forker.records.Put(stuck.ID, stuck); err != nil {
		t.Fatal(err)
	}
	orphanRoot := filepath.Join(t.TempDir(), "orphan-root")
	if err := os.MkdirAll(orphanRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := ForkRecord{
		ID: "orphan-fork", SourceWorkloadID: fixture.source.Spec.ID, SourceCheckpointID: committed.SourceCheckpointID,
		SourceRootPath: fixture.rootPath, ForkWorkloadID: "ghost-workload-2", ForkName: "orphan",
		State: ForkPreparing, RootPath: orphanRoot, StagingRoot: filepath.Join(t.TempDir(), "orphan-staging"),
		ImagesDirectory: filepath.Join(t.TempDir(), "orphan-images"),
		CreatedAt:       now, UpdatedAt: now,
	}
	if err := fixture.forker.records.Put(orphan.ID, orphan); err != nil {
		t.Fatal(err)
	}
	recoverErr := fixture.forker.Recover(context.Background())
	if recoverErr == nil {
		t.Fatal("the stuck rollback must be reported")
	}
	if !strings.Contains(recoverErr.Error(), stuck.ID) {
		t.Fatalf("the aggregate must name the record that failed: %v", recoverErr)
	}
	rolled, err := fixture.forker.Get(orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.State != ForkRolledBack {
		t.Fatalf("the later interrupted fork was stranded by the stuck one: %s", rolled.State)
	}
	if _, statErr := os.Stat(orphanRoot); !os.IsNotExist(statErr) {
		t.Fatalf("the later fork root survived recovery: %v", statErr)
	}
	if safe, getErr := fixture.forker.Get(committed.ID); getErr != nil || safe.State != ForkCommitted {
		t.Fatalf("the committed fork was disturbed by recovery: %+v %v", safe, getErr)
	}
	// Recovery problems never fail OpenForker — the agent boots either way.
	if opened, openErr := OpenForker(fixture.service); openErr != nil || opened == nil {
		t.Fatalf("fork recovery problems must not block startup: %v", openErr)
	}
}
