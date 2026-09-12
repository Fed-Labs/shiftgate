package checkpoint

import (
	"context"
	"errors"
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
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

// lazyTestEngine stands in for a CRIU build with lazy-pages support. Its
// "daemon" is a real process whose /proc command line is shaped exactly like
// criu lazy-pages over an image set, so the restorer's liveness proof — the
// thing that decides when a retained image set is cleaned up — runs against a
// genuine pid, not a stub.
type lazyTestEngine struct {
	t            *testing.T
	featureErr   error
	restoreErr   error
	restoreDelay time.Duration
	restores     []RestoreOptions
	daemons      []*fakeLazyDaemon
}

func (*lazyTestEngine) Check(context.Context) error                { return nil }
func (*lazyTestEngine) Version(context.Context) (string, error)    { return "CRIU lazy-test", nil }
func (*lazyTestEngine) PreDump(context.Context, DumpOptions) error { return nil }
func (*lazyTestEngine) Dump(_ context.Context, options DumpOptions) error {
	return os.WriteFile(filepath.Join(options.ImagesDirectory, "pages.img"), []byte("process-memory"), 0o600)
}

func (e *lazyTestEngine) CheckFeature(_ context.Context, feature string) error {
	if e.featureErr != nil {
		return e.featureErr
	}
	if feature != "uffd" {
		return errors.New("unknown feature " + feature)
	}
	return nil
}

func (e *lazyTestEngine) StartLazyPages(_ context.Context, imagesDirectory, workDirectory string) (LazyPagesProcess, error) {
	daemon, err := newFakeLazyDaemon(e.t, imagesDirectory)
	if err != nil {
		return nil, err
	}
	e.daemons = append(e.daemons, daemon)
	return daemon, nil
}

func (e *lazyTestEngine) Restore(_ context.Context, options RestoreOptions) (int, error) {
	e.restores = append(e.restores, options)
	if e.restoreErr != nil {
		return 0, e.restoreErr
	}
	if e.restoreDelay > 0 {
		time.Sleep(e.restoreDelay)
	}
	command := exec.Command("/bin/sh", "-c", "while true; do sleep 1; done")
	// A restored tree leads its own session, the way the runtime manager
	// scopes its group-wide signalling; reproduce that so adoption and resume
	// run against a genuinely session-leading pid.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
}

// fakeLazyDaemon is a shell child whose argv is spelled the way criu spells
// lazy-pages, so /proc/<pid>/cmdline — what liveness is proven from — reads
// like the real daemon over the given image set.
type fakeLazyDaemon struct {
	command *exec.Cmd
	stopped bool
}

func newFakeLazyDaemon(t *testing.T, imagesDirectory string) (*fakeLazyDaemon, error) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "lazy-pages.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		return nil, err
	}
	command := exec.Command(script)
	// The kernel's shebang handling drops argv[0] and prepends the interpreter
	// and script path, so the daemon's own spelling starts at the second
	// element — the command line ends up carrying "lazy-pages" and the image
	// set as standalone arguments, the same membership the real
	// `criu lazy-pages -D dir -W work` offers.
	command.Args = []string{"daemon", "lazy-pages", "-D", imagesDirectory}
	if err := command.Start(); err != nil {
		return nil, err
	}
	go func() { _ = command.Wait() }()
	return &fakeLazyDaemon{command: command}, nil
}

func (d *fakeLazyDaemon) PID() int { return d.command.Process.Pid }

func (d *fakeLazyDaemon) Stop() {
	d.stopped = true
	_ = d.command.Process.Signal(syscall.SIGTERM)
}

type lazyFixture struct {
	service  *Service
	restorer *Restorer
	runtime  *shiftruntime.Manager
	engine   Engine
	workload model.Workload
}

// newLazyFixture boots the full restore stack over the given engine — the
// lazy fake for lazy tests, the plain fake for refusals — the same
// construction the agent performs: a running workload, an identity, a chunk
// store, a repository, a service, and a restorer.
func newLazyFixture(t *testing.T, engine Engine) lazyFixture {
	t.Helper()
	stubCRIUOnPath(t)
	if lazy, ok := engine.(*lazyTestEngine); ok {
		lazy.t = t
	}
	stateRoot := t.TempDir()
	workloadRoot := t.TempDir()
	script := filepath.Join(workloadRoot, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(filepath.Join(stateRoot, "runtime"), false, logger)
	if err != nil {
		t.Fatal(err)
	}
	workload, err := runtimeManager.Create(model.WorkloadSpec{
		Name: "lazy-source", Command: []string{script}, RootPath: workloadRoot, WorkingDir: workloadRoot,
		UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeManager.Start(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = runtimeManager.Stop(workload.Spec.ID, time.Second)
		if lazy, ok := engine.(*lazyTestEngine); ok {
			for _, daemon := range lazy.daemons {
				if !daemon.stopped {
					_ = daemon.command.Process.Kill()
				}
			}
		}
	})
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
	restorer, err := OpenRestorer(service)
	if err != nil {
		t.Fatal(err)
	}
	return lazyFixture{service: service, restorer: restorer, runtime: runtimeManager, engine: engine, workload: workload}
}

// stubCRIUOnPath installs a stub criu executable on PATH for this test. The
// destination inventory probes the real binary — `criu check` needs
// CAP_SYS_ADMIN or CAP_CHECKPOINT_RESTORE, which an unprivileged shell cannot
// grant even on a machine with CRIU installed — so the stub answers that probe
// and the compatibility gate in Prepare reflects the engine under test, the
// fake, rather than the host's privileges. No checkpoint work goes through it:
// the service's engine is the fixture's fake.
func stubCRIUOnPath(t *testing.T) {
	t.Helper()
	directory := t.TempDir()
	stub := filepath.Join(directory, "criu")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'Version: 4.2 (stub)'; fi\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stoppedCheckpoint freezes the fixture's workload into a full stopped
// checkpoint — the state a restore takes over from.
func stoppedCheckpoint(t *testing.T, fixture lazyFixture) model.CheckpointManifest {
	t.Helper()
	manifest, err := fixture.service.Create(context.Background(), fixture.workload.Spec.ID, CreateOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestLazyRestoreRefusedWithoutCapability: an engine without lazy-pages
// support must refuse the request up front — never silently fall back to the
// eager restore the caller did not ask for.
func TestLazyRestoreRefusedWithoutCapability(t *testing.T) {
	fixture := newLazyFixture(t, fakeEngine{})
	manifest := stoppedCheckpoint(t, fixture)
	_, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err == nil || !strings.Contains(err.Error(), "cannot serve lazy restores") {
		t.Fatalf("a lazy request on an engine without the capability must be refused clearly, got %v", err)
	}
}

// TestLazyRestoreRefusedWithoutUFFD: a kernel without userfaultfd fails the
// request in milliseconds with the reason, before anything is staged.
func TestLazyRestoreRefusedWithoutUFFD(t *testing.T) {
	engine := &lazyTestEngine{featureErr: errors.New("uffd is not supported")}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)
	_, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err == nil || !strings.Contains(err.Error(), "userfaultfd") {
		t.Fatalf("an unsupported kernel must fail with the userfaultfd reason, got %v", err)
	}
	if len(engine.daemons) != 0 {
		t.Fatal("no daemon may start when the feature check fails")
	}
}

// TestLazyRestoreStartsDaemonAndRecordsTiming: the happy path. The daemon
// starts before the restore call, the engine is asked for a lazy restore, the
// record carries the daemon's pid and an honest time-to-first-execution, and
// commit keeps the image set on disk for the daemon still reading it.
func TestLazyRestoreStartsDaemonAndRecordsTiming(t *testing.T) {
	engine := &lazyTestEngine{restoreDelay: 50 * time.Millisecond}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)

	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(engine.restores) != 1 || !engine.restores[0].LazyPages {
		t.Fatalf("the engine must be asked for a lazy-pages restore exactly once, saw %+v", engine.restores)
	}
	if len(engine.daemons) != 1 {
		t.Fatalf("exactly one lazy-pages daemon must serve the restore, saw %d", len(engine.daemons))
	}
	if !record.Lazy {
		t.Fatal("the restore record must mark itself lazy")
	}
	if record.LazyPagesPID != engine.daemons[0].PID() {
		t.Fatalf("the record must carry the daemon's pid: record %d, daemon %d", record.LazyPagesPID, engine.daemons[0].PID())
	}
	if record.TimeToFirstExecutionMS < 50 {
		t.Fatalf("time to first execution must span the restore call (>= 50ms here), got %d ms", record.TimeToFirstExecutionMS)
	}
	images := filepath.Join(record.RestoreDirectory, "images")
	if !lazyPagesDaemonAlive(record.LazyPagesPID, images) {
		t.Fatal("the daemon must be provably alive over the retained image set")
	}
	if record.State != RestoreValidated {
		t.Fatalf("a successful prepare ends validated, got %s", record.State)
	}

	committed, err := fixture.restorer.Commit(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != RestoreCommitted {
		t.Fatalf("commit must commit, got %s", committed.State)
	}
	if _, statErr := os.Stat(record.RestoreDirectory); statErr != nil {
		t.Fatalf("a committed lazy restore must retain its image set for the daemon: %v", statErr)
	}
	// The daemon's death is the workload's death: once it is gone the retained
	// image set is derived state and the commit-armed watch must remove it.
	engine.daemons[0].command.Process.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(record.RestoreDirectory); errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the retained image set must be removed once the daemon exits, %s still exists", record.RestoreDirectory)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestLazyRestoreStopsDaemonWhenRestoreFails: a restore that fails after the
// daemon started must not leave the daemon — or the staged image set — behind.
// The rollback path owns both.
func TestLazyRestoreStopsDaemonWhenRestoreFails(t *testing.T) {
	engine := &lazyTestEngine{restoreErr: errors.New("criu restore failed")}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)

	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err == nil {
		t.Fatal("a failing engine restore must fail the prepare")
	}
	if len(engine.daemons) != 1 {
		t.Fatalf("the daemon was started before the restore call, saw %d", len(engine.daemons))
	}
	if !engine.daemons[0].stopped {
		t.Fatal("the rollback must stop the lazy-pages daemon it started")
	}
	if _, statErr := os.Stat(record.RestoreDirectory); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the rollback must remove the staged image set, %s still exists", record.RestoreDirectory)
	}
	stored, getErr := fixture.restorer.Get(record.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.State != RestoreRolledBack {
		t.Fatalf("the failed restore must be recorded as rolled back, got %s", stored.State)
	}
}

// TestLazyPagesDaemonAlive: the liveness proof is an identity proof. A pid is
// this restore's daemon only while /proc says the process behind it is a
// lazy-pages serving exactly this image set — anything else (a dead pid, a
// recycled pid, a different image set, a nonsense pid) must read as dead, or
// a retained image set could be deleted under a live daemon, or linger
// forever behind a dead one.
func TestLazyPagesDaemonAlive(t *testing.T) {
	images := filepath.Join(t.TempDir(), "images")
	if err := os.MkdirAll(images, 0o700); err != nil {
		t.Fatal(err)
	}
	daemon, err := newFakeLazyDaemon(t, images)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(daemon.Stop)
	// The liveness check needs the child to have execed its script; give the
	// freshly started daemon a moment (the real one has already created its
	// socket by the time StartLazyPages returns, so this gap is test-only).
	deadline := time.Now().Add(5 * time.Second)
	for !lazyPagesDaemonAlive(daemon.PID(), images) {
		if time.Now().After(deadline) {
			t.Fatal("a live daemon over this image set must read as alive")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lazyPagesDaemonAlive(daemon.PID(), filepath.Join(t.TempDir(), "other-images")) {
		t.Fatal("a daemon over a different image set must read as dead for this one")
	}
	if lazyPagesDaemonAlive(os.Getpid(), images) {
		t.Fatal("a live pid that is not this daemon must not pass for it")
	}
	if lazyPagesDaemonAlive(0, images) || lazyPagesDaemonAlive(-1, images) {
		t.Fatal("nonsense pids must read as dead")
	}
	if err := daemon.command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(5 * time.Second)
	for lazyPagesDaemonAlive(daemon.PID(), images) {
		if time.Now().After(end) {
			t.Fatal("a killed daemon must read as dead")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLazyRestoreRecoverRearmsWatchForLiveDaemon: an agent restart must not
// orphan a live lazy restore's image set. Reopening the restorer re-arms the
// watch, and the daemon's later death still cleans up. The record is left
// validated rather than committed so no watch from Commit exists — what removes
// the directory can only be the one Recover armed.
func TestLazyRestoreRecoverRearmsWatchForLiveDaemon(t *testing.T) {
	engine := &lazyTestEngine{}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)

	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRestorer(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Get(record.ID); err != nil {
		t.Fatalf("the reopened restorer must hold the same records: %v", err)
	}
	if _, statErr := os.Stat(record.RestoreDirectory); statErr != nil {
		t.Fatalf("a live daemon's image set must survive the agent restart: %v", statErr)
	}
	engine.daemons[0].command.Process.Kill()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(record.RestoreDirectory); errors.Is(statErr, os.ErrNotExist) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watch Recover armed must remove the image set once the daemon dies, %s still exists", record.RestoreDirectory)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestLazyRestoreRecoverRemovesImagesOfDeadDaemon: a workload that ended while
// the agent was down leaves an exited daemon and a retained image set behind.
// The next open must clean it up immediately, not hand the mess to a watch.
func TestLazyRestoreRecoverRemovesImagesOfDeadDaemon(t *testing.T) {
	engine := &lazyTestEngine{}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)

	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err != nil {
		t.Fatal(err)
	}
	engine.daemons[0].command.Process.Kill()
	// The daemon must be observed dead before the restart happens, the way a
	// real gap between workload death and agent restart would observe it.
	deadline := time.Now().Add(5 * time.Second)
	images := filepath.Join(record.RestoreDirectory, "images")
	for lazyPagesDaemonAlive(record.LazyPagesPID, images) {
		if time.Now().After(deadline) {
			t.Fatal("the killed daemon must die")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := OpenRestorer(fixture.service); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(record.RestoreDirectory); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a dead daemon's image set must be removed by the restart, %s still exists", record.RestoreDirectory)
	}
}

// TestEagerRestoreRecordsTimeToFirstExecution: the honest comparison the lazy
// number invites only works if the eager path measures the same moment — the
// restore call through the tree executing. An eager restore also consumes its
// image set at commit; nothing is retained.
func TestEagerRestoreRecordsTimeToFirstExecution(t *testing.T) {
	engine := &lazyTestEngine{restoreDelay: 50 * time.Millisecond}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)

	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if record.Lazy || record.LazyPagesPID != 0 {
		t.Fatalf("an eager restore must not be marked lazy: %+v", record)
	}
	if len(engine.daemons) != 0 {
		t.Fatal("an eager restore starts no daemon")
	}
	if record.TimeToFirstExecutionMS < 50 {
		t.Fatalf("an eager restore records the same measurement, got %d ms", record.TimeToFirstExecutionMS)
	}
	if _, err := fixture.restorer.Commit(record.ID); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(record.RestoreDirectory); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a committed eager restore must consume its image set, %s still exists", record.RestoreDirectory)
	}
}
