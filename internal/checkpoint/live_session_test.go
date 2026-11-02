package checkpoint

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

// poisonPassEngine fails its Nth PreDump so a pass can be made to fail after
// earlier passes already armed memory tracking — the exact window where a
// retry would silently drop pages.
type poisonPassEngine struct {
	preDumps int
	failAt   int
}

func (*poisonPassEngine) Check(context.Context) error             { return nil }
func (*poisonPassEngine) Version(context.Context) (string, error) { return "CRIU poison", nil }
func (*poisonPassEngine) Restore(context.Context, RestoreOptions) (int, error) {
	return 0, nil
}
func (e *poisonPassEngine) PreDump(_ context.Context, options DumpOptions) error {
	e.preDumps++
	if e.preDumps == e.failAt {
		return errors.New("injected pre-dump failure")
	}
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "precopy.img"), []byte("precopy-memory"), 0o600)
}
func (*poisonPassEngine) Dump(_ context.Context, options DumpOptions) error {
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}

type liveSessionFixture struct {
	service    *Service
	repository *Repository
	chunks     *chunkstore.Store
	runtime    *shiftruntime.Manager
	workloadID string
}

func newLiveSessionFixture(t *testing.T, engine Engine) liveSessionFixture {
	t.Helper()
	stubCRIUOnPath(t)
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
	workload, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "live-source", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
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
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, engine, logger)
	return liveSessionFixture{service: service, repository: repository, chunks: chunks, runtime: runtimeManager, workloadID: workload.Spec.ID}
}

// TestLiveFailedPassPoisonsTheSession pins the retry-safety rule: a pass that
// fails after its PreDump ran has taken pages no retained set holds, so the
// session is finished — every further Pass and the Finalize refuse, and only
// Abort is left.
func TestLiveFailedPassPoisonsTheSession(t *testing.T) {
	engine := &poisonPassEngine{failAt: 2}
	fixture := newLiveSessionFixture(t, engine)
	live, err := fixture.service.BeginLive(context.Background(), fixture.workloadID, LiveOptions{Passes: 5, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := live.Pass(context.Background())
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if !first.More {
		t.Fatal("the first pass should leave room for more")
	}
	if _, err := live.Pass(context.Background()); err == nil {
		t.Fatal("the injected pre-dump failure must fail the pass")
	}
	if live.CanPass() {
		t.Fatal("a failed pass must end the pass loop")
	}
	retry, err := live.Pass(context.Background())
	if err == nil {
		t.Fatalf("a retried pass must be refused, got %+v", retry)
	}
	if !strings.Contains(err.Error(), "abort") {
		t.Fatalf("the refusal must point at aborting the session, got: %v", err)
	}
	if _, err := live.Finalize(context.Background()); err == nil {
		t.Fatal("finalizing over an incomplete image chain must be refused")
	} else if !strings.Contains(err.Error(), "abort") {
		t.Fatalf("the refusal must point at aborting the session, got: %v", err)
	}
	if _, err := fixture.repository.Load(live.checkpointID); err == nil {
		t.Fatal("no checkpoint may be produced from an incomplete chain")
	}
	live.Abort()
	if current, getErr := fixture.runtime.Get(fixture.workloadID); getErr != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("abort must leave the workload running: %+v %v", current, getErr)
	}
	// Abort releases the single-checkpoint guard, so the workload is not
	// stranded from future checkpoints by the poisoned session.
	if _, err := fixture.service.BeginLive(context.Background(), fixture.workloadID, LiveOptions{LeaveRunning: true}); err != nil {
		t.Fatalf("the poisoned session leaked the checkpoint guard: %v", err)
	}
}

// TestLivePassImageBytesSurfacesWalkErrors: the pre-copy delta that drives the
// convergence decision must never be a silent partial sum — an unreadable
// image set is an error, so the loop can refuse to converge on it.
func TestLivePassImageBytesSurfacesWalkErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pages-1.img"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pages-2.img"), make([]byte, 50), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "work", "stats"), make([]byte, 999), 0o600); err != nil {
		t.Fatal(err)
	}
	total, err := passImageBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	if total != 150 {
		t.Fatalf("delta is %d, want 150 (work scratch excluded)", total)
	}
	blocked := filepath.Join(root, "blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "pages-3.img"), make([]byte, 25), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(blocked, 0o700) }()
	if partial, err := passImageBytes(root); err == nil {
		t.Fatalf("a walk failure must surface as an error, not the partial sum %d", partial)
	}
}
