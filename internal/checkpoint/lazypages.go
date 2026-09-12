package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// lazyPagesStartTimeout bounds how long StartLazyPages waits for the daemon
// to create its socket: past this the daemon is broken, not slow, and the
// restore must not proceed against a memory server that is not there.
const lazyPagesStartTimeout = 15 * time.Second

// lazyPagesStopGrace is how long Stop waits after SIGTERM before SIGKILL.
const lazyPagesStopGrace = 5 * time.Second

// lazyPagesWatchInterval is how often a committed lazy restore's watcher
// re-checks its daemon. The daemon exits with the workload, so the check is
// cheap and the cleanup is at most this late.
const lazyPagesWatchInterval = 2 * time.Second

// LazyPagesEngine is the optional capability an Engine can offer: serving a
// restored process's memory on demand through userfaultfd instead of loading
// all of it before the process starts. The production CRIU engine implements
// it; a restore that asks for lazy pages on an engine without the capability
// is refused, never silently downgraded to an eager restore.
type LazyPagesEngine interface {
	Engine
	// CheckFeature must return an error when the named feature does not work
	// on this machine — "uffd" is the one a lazy restore needs.
	CheckFeature(ctx context.Context, feature string) error
	// StartLazyPages runs the memory server a lazy restore faults its pages
	// in from, over one image set. The daemon outlives the call, the restore,
	// and the request that caused it, so it is never bound to ctx.
	StartLazyPages(ctx context.Context, imagesDirectory, workDirectory string) (LazyPagesProcess, error)
}

// LazyPagesProcess is a running lazy-pages daemon.
type LazyPagesProcess interface {
	// PID identifies the daemon across agent restarts: the restorer records
	// it and later proves liveness from /proc, where a recycled pid fails the
	// command-line check instead of passing for the daemon.
	PID() int
	// Stop kills the daemon — only for a restore that failed before its
	// process could live with unserved pages. A daemon whose workload lives
	// and pages remain unserved is never stopped: the restored process would
	// lose its memory server and die on its next page fault.
	Stop()
}

// lazyPagesDaemon is the CRIU implementation of LazyPagesProcess: a
// foreground `criu lazy-pages` child of this agent. The daemon's lifetime
// rule is CRIU's own — it exits once it has finished serving: every image
// page transferred, or the process gone — so orphaning it on agent shutdown
// is correct, not a leak: killing it while pages remain unserved would take
// the workload down with it on its next page fault.
type lazyPagesDaemon struct {
	command  *exec.Cmd
	logPath  string
	done     chan struct{}
	stopOnce sync.Once
}

// StartLazyPages runs `criu lazy-pages` over an image set and waits for it to
// create its socket — the signal that a restore can connect. The daemon is
// deliberately not started with CRIU's own --daemon flag: as this agent's
// child, its exit is reaped here, while the process it serves still ends it.
func (c *CRIU) StartLazyPages(ctx context.Context, imagesDirectory, workDirectory string) (LazyPagesProcess, error) {
	if imagesDirectory == "" || workDirectory == "" {
		return nil, errors.New("lazy-pages needs the image set and a work directory")
	}
	if err := os.MkdirAll(workDirectory, 0o700); err != nil {
		return nil, err
	}
	logPath := filepath.Join(workDirectory, "lazy-pages.log")
	daemon := &lazyPagesDaemon{
		command: exec.CommandContext(context.Background(), c.binary, "lazy-pages",
			"-D", imagesDirectory, "-W", workDirectory, "-v4", "-o", filepath.Base(logPath)),
		logPath: logPath,
		done:    make(chan struct{}),
	}
	// The daemon must outlive this call, the restore, and the API request
	// that caused it: it runs under a context that is never canceled, so
	// nothing kills it when the request ends. Its lifetime is managed by
	// the done channel and the process it serves.
	if err := daemon.command.Start(); err != nil {
		return nil, fmt.Errorf("start criu lazy-pages: %w", err)
	}
	exit := make(chan error, 1)
	go func() {
		exit <- daemon.command.Wait()
		close(daemon.done)
	}()
	socket := filepath.Join(workDirectory, "lazy-pages.socket")
	ready := func() bool {
		info, statErr := os.Stat(socket)
		return statErr == nil && info.Mode()&os.ModeSocket != 0
	}
	// The socket appears within milliseconds of the daemon's start, and every
	// millisecond here is a millisecond added to the restored process's time
	// to first execution — so the first check is immediate and the retry
	// granularity is fine. The daemon takes ~100ms to boot; a coarser tick
	// would round that up by its full interval.
	if ready() {
		return daemon, nil
	}
	deadline := time.After(lazyPagesStartTimeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exit:
			return nil, fmt.Errorf("criu lazy-pages exited during startup: %w: %s", err, tailFile(logPath, 20))
		case <-deadline:
			daemon.Stop()
			return nil, fmt.Errorf("criu lazy-pages never created its socket: %s", tailFile(logPath, 20))
		case <-ticker.C:
			if ready() {
				return daemon, nil
			}
		}
	}
}

// PID returns the daemon's process id.
func (d *lazyPagesDaemon) PID() int { return d.command.Process.Pid }

// Stop terminates the daemon — SIGTERM first, SIGKILL after the grace
// period. The exit itself is reaped by the waiter StartLazyPages spawned.
func (d *lazyPagesDaemon) Stop() {
	d.stopOnce.Do(func() {
		if d.command.Process == nil {
			return
		}
		_ = d.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-d.done:
		case <-time.After(lazyPagesStopGrace):
			_ = d.command.Process.Kill()
		}
	})
}

// lazyPagesDaemonAlive proves from /proc that a recorded daemon pid is still
// the lazy-pages daemon serving this restore's images — the command line must
// say so. A pid recycled by an unrelated process fails the check and reads as
// dead, so a retained image set is neither kept forever by a stranger nor
// deleted under a live daemon.
func lazyPagesDaemonAlive(pid int, imagesDirectory string) bool {
	if pid <= 0 {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	servesLazyPages, servesTheseImages := false, false
	for _, argument := range strings.Split(string(raw), "\x00") {
		switch argument {
		case "lazy-pages":
			servesLazyPages = true
		case imagesDirectory:
			servesTheseImages = true
		}
	}
	return servesLazyPages && servesTheseImages
}
