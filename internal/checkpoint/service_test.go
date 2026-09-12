package checkpoint

import (
	"archive/tar"
	"bytes"
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
	trackMemory    []bool
}

func (e *recordingEngine) Check(context.Context) error             { return nil }
func (e *recordingEngine) Version(context.Context) (string, error) { return "CRIU recording", nil }
func (e *recordingEngine) PreDump(_ context.Context, options DumpOptions) error {
	e.preDumpCalls++
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "precopy.img"), []byte("precopy-memory"), 0o600)
}
func (e *recordingEngine) Dump(_ context.Context, options DumpOptions) error {
	e.trackMemory = append(e.trackMemory, options.TrackMemory)
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
	// Both dumps must arm the memory tracker: the full parent because its
	// process keeps running and can be incrementally checkpointed later, the
	// child because it diffs against that parent.
	if len(engine.trackMemory) != 2 || !engine.trackMemory[0] || !engine.trackMemory[1] {
		t.Fatalf("dumps did not arm memory tracking: %v", engine.trackMemory)
	}
	if len(engine.parentContents) == 0 {
		t.Fatal("retained parent image directory was empty")
	}
	if _, err := repository.Load(child.ID); err != nil {
		t.Fatal(err)
	}
}

func TestLiveSessionInvokesCRIUPreDump(t *testing.T) {
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
	live, err := service.BeginLive(context.Background(), workload.Spec.ID, LiveOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest, err := live.Finalize(context.Background())
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

// deltaEngine writes a controlled number of image bytes per pre-dump pass so
// the iterative loop's convergence and pass-cap behavior is observable. The
// last delta repeats when more passes run than deltas list.
type deltaEngine struct {
	deltas      []int
	passDirs    []string
	passParents []string
	dumpParent  string
	dumpFails   bool
	lastPreDump time.Time
	dumpStarted time.Time
}

func (e *deltaEngine) Check(context.Context) error                        { return nil }
func (e *deltaEngine) Version(context.Context) (string, error)            { return "CRIU delta", nil }
func (*deltaEngine) Restore(context.Context, RestoreOptions) (int, error) { return 0, nil }
func (e *deltaEngine) PreDump(_ context.Context, options DumpOptions) error {
	e.lastPreDump = time.Now()
	e.passDirs = append(e.passDirs, options.ImagesDirectory)
	e.passParents = append(e.passParents, options.ParentImages)
	index := len(e.passDirs) - 1
	if index >= len(e.deltas) {
		index = len(e.deltas) - 1
	}
	if size := e.deltas[index]; size > 0 {
		return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages-1.img"), make([]byte, size), 0o600)
	}
	return nil
}
func (e *deltaEngine) Dump(_ context.Context, options DumpOptions) error {
	e.dumpStarted = time.Now()
	e.dumpParent = options.ParentImages
	if e.dumpFails {
		return errors.New("simulated final-dump failure")
	}
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages-0.img"), []byte("final-image"), 0o600)
}

// preCopyHarness is the boilerplate every pre-copy test needs: one running
// workload and a checkpoint service wired to the given engine.
func preCopyHarness(t *testing.T, engine Engine) (*Service, *chunkstore.Store, *shiftruntime.Manager, string) {
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
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, engine, logger)
	return service, chunks, runtimeManager, workload.Spec.ID
}

// runPasses drains a live session's pass loop the way a live migration does:
// pass, transfer, pass, until the loop converges or hits its cap.
func runPasses(t *testing.T, live *LiveSession) {
	t.Helper()
	for {
		current, err := live.Pass(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !current.More {
			return
		}
	}
}

func TestLiveSessionIteratesUntilConverged(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000, 2000}}
	service, _, runtimeManager, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 5, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	runPasses(t, live)
	manifest, err := live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.passDirs) != 2 {
		t.Fatalf("expected the loop to stop at the converged second pass, got %d", len(engine.passDirs))
	}
	if engine.passParents[0] != "" || engine.passParents[1] != engine.passDirs[0] {
		t.Fatalf("pre-dump passes were not chained: parents=%v", engine.passParents)
	}
	if engine.dumpParent != engine.passDirs[1] {
		t.Fatalf("final dump did not parent the last pre-dump pass: %q", engine.dumpParent)
	}
	if manifest.Engine.PreCopyPasses != 2 || !manifest.Engine.PreCopy {
		t.Fatalf("manifest did not record the actual pass count: %+v", manifest.Engine)
	}
	if current, getErr := runtimeManager.Get(workloadID); getErr != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("source was not resumed after the pre-copy loop: %+v %v", current, getErr)
	}
}

func TestLiveSessionRunsToThePassCap(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000, 9000}}
	service, _, _, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 3, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	runPasses(t, live)
	manifest, err := live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.passDirs) != 3 {
		t.Fatalf("a workload that never converged should run to the cap, got %d passes", len(engine.passDirs))
	}
	if manifest.Engine.PreCopyPasses != 3 {
		t.Fatalf("manifest did not record the capped pass count: %+v", manifest.Engine)
	}
}

// TestMetricsFreezeInstantIsThePauseNotTheStart proves the freeze timestamp
// — the anchor migration downtime is measured from — is the pause before the
// final dump: after the pre-copy passes that ran while the workload was
// live, before the dump itself, and never merely the checkpoint's start.
func TestMetricsFreezeInstantIsThePauseNotTheStart(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000, 2000}}
	service, _, runtimeManager, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 2, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	runPasses(t, live)
	manifest, err := live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := manifest.Metrics
	if metrics.FreezeStartedAt.IsZero() {
		t.Fatal("a checkpoint of a running workload must record when it froze the process")
	}
	if metrics.FreezeStartedAt.Before(metrics.StartedAt) {
		t.Fatalf("freeze (%s) cannot precede the checkpoint's start (%s)", metrics.FreezeStartedAt, metrics.StartedAt)
	}
	if metrics.FreezeStartedAt.Before(engine.lastPreDump) {
		t.Fatalf("freeze (%s) cannot precede the last pre-copy pass (%s): pre-copy runs while the workload is live",
			metrics.FreezeStartedAt, engine.lastPreDump)
	}
	if metrics.FreezeStartedAt.After(engine.dumpStarted) {
		t.Fatalf("freeze (%s) cannot follow the final dump (%s)", metrics.FreezeStartedAt, engine.dumpStarted)
	}
	if current, getErr := runtimeManager.Get(workloadID); getErr != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("source was not resumed after the pre-copy checkpoint: %+v %v", current, getErr)
	}
}

// TestMetricsFreezeInstantIsZeroForAlreadyFrozenWorkload proves the manifest
// never claims a freeze the checkpoint did not cause: a workload that was
// already paused reports no freeze instant, so a migration of it cannot
// pretend its downtime window started at this checkpoint.
func TestMetricsFreezeInstantIsZeroForAlreadyFrozenWorkload(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000}}
	service, _, runtimeManager, workloadID := preCopyHarness(t, engine)
	if _, err := runtimeManager.Pause(workloadID); err != nil {
		t.Fatal(err)
	}
	manifest, err := service.Create(context.Background(), workloadID, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Metrics.FreezeStartedAt.IsZero() {
		t.Fatalf("a workload paused before the checkpoint must not report a freeze instant: %s",
			manifest.Metrics.FreezeStartedAt)
	}
	// The live path must agree: a paused workload has nothing to pre-copy, so
	// its session goes straight to Finalize and reports no freeze of its own.
	live, liveErr := service.BeginLive(context.Background(), workloadID, LiveOptions{})
	if liveErr != nil {
		t.Fatal(liveErr)
	}
	if live.CanPass() {
		t.Fatal("a paused workload must not run pre-copy passes")
	}
	liveManifest, finalizeErr := live.Finalize(context.Background())
	if finalizeErr != nil {
		t.Fatal(finalizeErr)
	}
	if !liveManifest.Metrics.FreezeStartedAt.IsZero() {
		t.Fatalf("a live checkpoint of a paused workload must not report a freeze instant: %s",
			liveManifest.Metrics.FreezeStartedAt)
	}
}

func TestLiveSessionPassBounds(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000, 9000}}
	service, _, _, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 100, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	if live.PassLimit() != 16 {
		t.Fatalf("pass cap should clamp to 16, got %d", live.PassLimit())
	}
	runPasses(t, live)
	manifest, err := live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.passDirs) != 16 {
		t.Fatalf("pass cap should clamp to 16, got %d", len(engine.passDirs))
	}
	engine = &deltaEngine{deltas: []int{10000}}
	service, _, _, workloadID = preCopyHarness(t, engine)
	live, err = service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: -4, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	runPasses(t, live)
	manifest, err = live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.passDirs) != 1 || manifest.Engine.PreCopyPasses != 1 {
		t.Fatalf("a negative cap should mean the single historical pass: %d passes, %+v", len(engine.passDirs), manifest.Engine)
	}
}

// The interleaving property the whole live session exists for: a completed
// pass's asset is fully packaged and validatable in the chunk store before
// Finalize ever runs, so its chunks can be on the destination while the
// workload is still running.
func TestLiveSessionPassAssetIsValidBeforeFinalize(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000}}
	service, chunks, runtimeManager, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 2, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	first, passErr := live.Pass(context.Background())
	if passErr != nil {
		t.Fatal(passErr)
	}
	if first.Index != 1 || !first.More {
		t.Fatalf("the first of two passes must report index 1 and more to do: %+v", first)
	}
	if first.KeyVersion != live.KeyVersion() {
		t.Fatalf("a pass must pin the session's key version: %d != %d", first.KeyVersion, live.KeyVersion())
	}
	if err := chunks.ValidateAsset(context.Background(), workloadID, first.Asset); err != nil {
		t.Fatalf("a completed pass's asset must be validatable before Finalize: %v", err)
	}
	live.Abort()
	if current, getErr := runtimeManager.Get(workloadID); getErr != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("a workload no pass ever froze must still be running after Abort: %+v %v", current, getErr)
	}
}

// Abort abandons a session mid-pass: the attempt counts as a failed
// checkpoint, the guard is released for the next one, and a second Abort
// after the session closed is a no-op.
func TestLiveSessionAbortReleasesTheGuard(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000}}
	service, _, _, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	live.Abort()
	live.Abort()
	if _, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true}); err != nil {
		t.Fatalf("Abort must release the single-checkpoint guard: %v", err)
	}
}

// The live session takes the same single-checkpoint guard Create takes, so
// no checkpoint can begin behind its back while passes are running.
func TestBeginLiveRefusesConcurrentCheckpoint(t *testing.T) {
	engine := &blockingEngine{entered: make(chan struct{}), release: make(chan struct{})}
	service, _, _, workloadID := preCopyHarness(t, engine)
	first := make(chan error, 1)
	go func() {
		_, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true})
		first <- err
	}()
	<-engine.entered
	if _, err := service.BeginLive(context.Background(), workloadID, LiveOptions{}); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("a live session must not open behind an in-flight checkpoint, got %v", err)
	}
	close(engine.release)
	if err := <-first; err != nil {
		t.Fatalf("the in-flight checkpoint failed: %v", err)
	}
}

// A Finalize that fails after freezing the workload must thaw it: a
// checkpoint that never existed leaves nothing stopped behind, and the freeze
// instant stays readable so a failed migration's downtime is still measured
// from the real freeze, never from the passes that ran before it.
func TestLiveSessionFinalizeFailureThawsTheWorkload(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000}, dumpFails: true}
	service, _, runtimeManager, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 2})
	if err != nil {
		t.Fatal(err)
	}
	runPasses(t, live)
	if _, err := live.Finalize(context.Background()); err == nil {
		t.Fatal("a failing final dump must fail Finalize")
	}
	if live.FreezeStartedAt().IsZero() {
		t.Fatal("the freeze that preceded the failed dump must stay readable for honest downtime accounting")
	}
	if current, getErr := runtimeManager.Get(workloadID); getErr != nil || current.Status != model.WorkloadRunning {
		t.Fatalf("a failed Finalize must thaw the workload it froze: %+v %v", current, getErr)
	}
	engine.dumpFails = false
	if _, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true}); err != nil {
		t.Fatalf("a failed Finalize must release the single-checkpoint guard: %v", err)
	}
}

// The live reservation must bound what the whole session uploads: measured
// from pass 1 once it is packaged, it has to cover every later pass, the
// final dump, and the filesystem root, or an honest transfer would be
// rejected against its own quota.
func TestEstimateLiveTransferBoundsTheSession(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000, 9000}}
	service, chunks, runtimeManager, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 3})
	if err != nil {
		t.Fatal(err)
	}
	first, passErr := live.Pass(context.Background())
	if passErr != nil {
		t.Fatal(passErr)
	}
	workload, getErr := runtimeManager.Get(workloadID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	estimated, estimateErr := EstimateLiveTransferBytes(workload.Spec, first.Asset.StoredSize, live.PassLimit())
	if estimateErr != nil {
		t.Fatalf("the harness root must walk cleanly: %v", estimateErr)
	}
	runPasses(t, live)
	manifest, err := live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.passDirs) != 3 {
		t.Fatalf("expected the pass cap to bind, got %d passes", len(engine.passDirs))
	}
	var uploaded int64
	for _, asset := range manifest.Assets {
		uploaded += asset.StoredSize
		if err := chunks.ValidateAsset(context.Background(), workloadID, asset); err != nil {
			t.Fatalf("asset %s must be validatable: %v", asset.Name, err)
		}
	}
	if estimated < uploaded {
		t.Fatalf("the reservation must bound the whole session: estimated %d < uploaded %d", estimated, uploaded)
	}
}

// treeBound must charge tar's real per-entry framing — headers, block
// padding, pax records — beyond the raw file bytes, honor plain-path
// exclusions, and stay loose (never undercount) for glob patterns.
func TestTreeBoundCoversTarFraming(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "exact.bin"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "work", "scratch.log"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	longName := strings.Repeat("n", 200) + ".txt"
	if err := os.WriteFile(filepath.Join(root, longName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withAll, err := treeBound(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutWork, err := treeBound(root, []string{"work"})
	if err != nil {
		t.Fatal(err)
	}
	if withoutWork >= withAll {
		t.Fatalf("excluding the work directory must shrink the bound: %d >= %d", withoutWork, withAll)
	}
	withGlob, err := treeBound(root, []string{"work/*"})
	if err != nil {
		t.Fatal(err)
	}
	if withGlob != withAll {
		t.Fatalf("a glob exclusion must not shrink the bound: %d != %d", withGlob, withAll)
	}
	var plain int64
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if info, infoErr := entry.Info(); infoErr == nil && info.Mode().IsRegular() {
			plain += info.Size()
		}
		return nil
	})
	if withAll < plain+1024 {
		t.Fatalf("the bound must cover tar framing and the archive trailer beyond the raw bytes: %d < %d", withAll, plain+1024)
	}
}

// assetMembers streams a stored asset back and returns its regular-file
// member names, so tests can assert what a checkpoint actually packages.
func assetMembers(t *testing.T, store *chunkstore.Store, workloadID string, asset model.AssetManifest) []string {
	t.Helper()
	var archive bytes.Buffer
	if err := store.RestoreAsset(context.Background(), workloadID, asset, &archive); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(&archive)
	members := []string{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			members = append(members, header.Name)
		}
	}
	return members
}

func containsMember(members []string, name string) bool {
	for _, member := range members {
		if member == name {
			return true
		}
	}
	return false
}

// A live-migration checkpoint ships each pass as its own asset beside the
// final image set: the final set references unchanged pages in the last
// pre-dump pass through a parent symlink, and that pass references the one
// before it. The sets stay sibling directories — never overlaid into one
// archive, where their same-named page files would shadow each other — and
// each pass's asset stands alone, its archive starting at its own first
// byte, so its chunks address identically whenever the destination receives
// them.
func TestLiveSessionPackagesEachPassAsItsOwnAsset(t *testing.T) {
	engine := &deltaEngine{deltas: []int{10000, 2000}}
	service, chunks, _, workloadID := preCopyHarness(t, engine)
	live, err := service.BeginLive(context.Background(), workloadID, LiveOptions{Passes: 5, LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	runPasses(t, live)
	manifest, err := live.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.passDirs) != 2 {
		t.Fatalf("expected the loop to stop at the converged second pass, got %d", len(engine.passDirs))
	}
	final, ok := assetByName(manifest.Assets, "process-state")
	if !ok {
		t.Fatal("checkpoint has no process-state asset")
	}
	members := assetMembers(t, chunks, workloadID, final)
	if !containsMember(members, "images/pages-0.img") {
		t.Fatalf("the final asset must carry the final image set, members: %v", members)
	}
	for _, member := range members {
		if strings.Contains(member, "precopy-") || strings.Contains(member, "work") {
			t.Fatalf("the final asset must carry only the final image set: %s", member)
		}
	}
	for _, expected := range []struct {
		asset  string
		member string
	}{
		{"process-state-precopy-1", "precopy-1/pages-1.img"},
		{"process-state-precopy-2", "precopy-2/pages-1.img"},
	} {
		asset, found := assetByName(manifest.Assets, expected.asset)
		if !found {
			t.Fatalf("checkpoint has no %s asset", expected.asset)
		}
		passMembers := assetMembers(t, chunks, workloadID, asset)
		if !containsMember(passMembers, expected.member) {
			t.Fatalf("%s must package %s, members: %v", expected.asset, expected.member, passMembers)
		}
		for _, member := range passMembers {
			if strings.Contains(member, "work") {
				t.Fatalf("CRIU work directories are scratch logs, not image data: %s", member)
			}
		}
	}
}

// An incremental checkpoint's own asset carries only its delta; restore
// reconstructs the ancestor chain as nested sibling directories beside it,
// which is what the parent symlinks inside the image sets point at.
func TestIncrementalAssetStaysDeltaOnlyAndMaterializesAncestors(t *testing.T) {
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
		Name: "incremental-chain", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
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
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, fakeEngine{}, logger)
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
	asset, ok := assetByName(child.Assets, "process-state")
	if !ok {
		t.Fatal("incremental checkpoint has no process-state asset")
	}
	members := assetMembers(t, chunks, workload.Spec.ID, asset)
	if !containsMember(members, "images/pages.img") {
		t.Fatalf("incremental asset must carry the checkpoint's own images, members: %v", members)
	}
	for _, member := range members {
		if strings.HasPrefix(member, "parent-images") {
			t.Fatalf("incremental asset must stay delta-only, the ancestor chain ships as its own assets: %s", member)
		}
	}
	destination := t.TempDir()
	if err := materializeProcessChain(context.Background(), repository, chunks, child, destination); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"images/pages.img", "parent-images/images/pages.img"} {
		if _, statErr := os.Stat(filepath.Join(destination, filepath.FromSlash(expected))); statErr != nil {
			t.Fatalf("restored chain must materialize %s: %v", expected, statErr)
		}
	}
}

// blockingEngine parks its first dump until released, so a test can hold one
// Create in flight and observe how a second one is treated.
type blockingEngine struct {
	entered chan struct{}
	release chan struct{}
	dumps   int
}

func (*blockingEngine) Check(context.Context) error                          { return nil }
func (*blockingEngine) Version(context.Context) (string, error)              { return "CRIU blocking", nil }
func (*blockingEngine) PreDump(context.Context, DumpOptions) error           { return nil }
func (*blockingEngine) Restore(context.Context, RestoreOptions) (int, error) { return 0, nil }
func (e *blockingEngine) Dump(_ context.Context, options DumpOptions) error {
	e.dumps++
	if e.dumps == 1 {
		close(e.entered)
		<-e.release
	}
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}

func TestCreateRefusesConcurrentCheckpointOfSameWorkload(t *testing.T) {
	engine := &blockingEngine{entered: make(chan struct{}), release: make(chan struct{})}
	service, _, _, workloadID := preCopyHarness(t, engine)
	first := make(chan error, 1)
	go func() {
		_, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true})
		first <- err
	}()
	<-engine.entered
	if _, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true}); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("a concurrent checkpoint of the same workload must be refused, got %v", err)
	}
	close(engine.release)
	if err := <-first; err != nil {
		t.Fatalf("the in-flight checkpoint failed: %v", err)
	}
	if _, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true}); err != nil {
		t.Fatalf("checkpointing must work again once the first completed: %v", err)
	}
}

func TestDeleteAndPruneProtectLineage(t *testing.T) {
	service, _, _, workloadID := preCopyHarness(t, fakeEngine{})
	base, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	stranger, err := service.Create(context.Background(), workloadID, CreateOptions{LeaveRunning: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	child, err := service.Create(context.Background(), workloadID, CreateOptions{
		Kind: model.CheckpointIncremental, ParentID: base.ID, LeaveRunning: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	grandchild, err := service.Create(context.Background(), workloadID, CreateOptions{
		Kind: model.CheckpointIncremental, ParentID: child.ID, LeaveRunning: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A parent whose descendant still exists is load-bearing and must not be
	// deletable.
	if err := service.Delete(child.ID); err == nil {
		t.Fatal("deleting a checkpoint a retained descendant still needs must be refused")
	}

	// Newest-first the chain reads grandchild, child, stranger, base. Keeping
	// the two newest protects the whole child lineage — base survives because
	// grandchild and child resolve their pages through it — while the
	// independent stranger is pruned.
	deleted, err := service.PruneWorkload(workloadID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != stranger.ID {
		t.Fatalf("only the independent checkpoint should be pruned, deleted: %v", deleted)
	}
	remaining := service.List(workloadID)
	if len(remaining) != 3 {
		t.Fatalf("the chain (grandchild, child, base) must survive pruning, got %d checkpoints", len(remaining))
	}
	survivors := map[string]bool{}
	for _, summary := range remaining {
		survivors[summary.ID] = true
	}
	if !survivors[base.ID] || !survivors[child.ID] || !survivors[grandchild.ID] {
		t.Fatalf("lineage survivors are wrong: %v", survivors)
	}

	// With the descendants gone the base becomes deletable.
	for _, id := range []string{grandchild.ID, child.ID, base.ID} {
		if err := service.Delete(id); err != nil {
			t.Fatalf("delete %s after its descendants: %v", id, err)
		}
	}
	if remaining := service.List(workloadID); len(remaining) != 0 {
		t.Fatalf("workload checkpoints should be empty, got %d", len(remaining))
	}
}
