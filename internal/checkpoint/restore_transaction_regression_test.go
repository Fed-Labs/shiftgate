package checkpoint

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe log sink for assertions against messages
// emitted from watcher goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(value)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRegressionConcurrentRestoreOfSameTargetRefused is the HIGH regression:
// the restore switch protocol is single-writer. A second restore racing the
// first — a failover against an operator restore, a double submission — must
// be refused outright, not allowed to adopt the first transaction's live tree
// as its backup and delete it on commit.
func TestRegressionConcurrentRestoreOfSameTargetRefused(t *testing.T) {
	engine := &lazyTestEngine{restoreDelay: 2 * time.Second}
	fixture := newLazyFixture(t, engine)
	manifest := stoppedCheckpoint(t, fixture)
	target := filepath.Clean(manifest.Workload.RootPath)

	first := make(chan error, 1)
	go func() {
		_, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{})
		first <- err
	}()

	// Wait until the first transaction provably holds the guard.
	deadline := time.Now().Add(10 * time.Second)
	for {
		fixture.restorer.inFlightMu.Lock()
		_, busy := fixture.restorer.inFlight[target]
		fixture.restorer.inFlightMu.Unlock()
		if busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first restore never claimed the transaction guard")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("a second restore over the same target must be refused, got %v", err)
	}

	if err := <-first; err != nil {
		t.Fatalf("the first restore failed: %v", err)
	}
}

// TestRegressionRestoreGuardReleasesAfterCommit: the guard must free the
// target once the transaction reaches a terminal state.
func TestRegressionRestoreGuardReleasesAfterCommit(t *testing.T) {
	fixture := newLazyFixture(t, &lazyTestEngine{})
	manifest := stoppedCheckpoint(t, fixture)
	target := filepath.Clean(manifest.Workload.RootPath)
	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fixture.restorer.inFlightMu.Lock()
	_, busy := fixture.restorer.inFlight[target]
	fixture.restorer.inFlightMu.Unlock()
	if !busy {
		t.Fatal("a validated restore must hold the guard until commit")
	}
	if _, err := fixture.restorer.Commit(record.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.restorer.beginRestore(target); err != nil {
		t.Fatalf("the guard must be free after commit: %v", err)
	}
	fixture.restorer.endRestore(target)
}

// TestRegressionRecoverCompletesStandingValidatedRestore is the MEDIUM
// regression: an agent crash in the Prepare→Commit window must not strand a
// VALIDATED restore forever with its backup, staging, and image trees leaked.
// The validation passed and the process survived — recovery completes the
// commit the vanished caller never will.
func TestRegressionRecoverCompletesStandingValidatedRestore(t *testing.T) {
	fixture := newLazyFixture(t, &lazyTestEngine{})
	manifest := stoppedCheckpoint(t, fixture)
	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != RestoreValidated {
		t.Fatalf("prepare ends validated, got %s", record.State)
	}
	// No Commit: simulate the agent dying here.
	reopened, err := OpenRestorer(fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reopened.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != RestoreCommitted {
		t.Fatalf("recovery must complete the commit for a live validated restore, got %s", stored.State)
	}
	for name, path := range map[string]string{
		"backup":  record.BackupRoot,
		"staging": record.StagingRoot,
	} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the committed restore must not leak its %s tree: %s still exists", name, path)
		}
	}
	if _, err := os.Stat(record.RestoreDirectory); !os.IsNotExist(err) {
		t.Fatalf("an eager restore consumes its image set at commit, %s still exists", record.RestoreDirectory)
	}
}

// TestRegressionLazyDaemonDeathWithLiveWorkloadIsLogged is the MEDIUM
// lazy-restore fault-handling regression: the daemon dying while the restored
// workload still runs is a fault — the workload cannot satisfy its next page
// fault — and must surface as a loud error, not pass as an ordinary cleanup.
func TestRegressionLazyDaemonDeathWithLiveWorkloadIsLogged(t *testing.T) {
	engine := &lazyTestEngine{}
	fixture := newLazyFixture(t, engine)
	logs := &syncBuffer{}
	fixture.restorer.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	manifest := stoppedCheckpoint(t, fixture)
	record, err := fixture.restorer.Prepare(context.Background(), manifest.ID, PrepareOptions{Lazy: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.restorer.Commit(record.ID); err != nil {
		t.Fatal(err)
	}
	// The workload (a live sleep loop) keeps running; its daemon dies.
	if err := engine.daemons[0].command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		text := logs.String()
		if strings.Contains(text, "lazy-pages daemon exited while the restored workload is still running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon death under a live workload must be logged loudly, got logs: %q", text)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The retained image set is still cleaned up: with the daemon gone it is
	// derived state however the fault resolves.
	end := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(record.RestoreDirectory); os.IsNotExist(err) {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("the retained image set must be removed once the daemon is gone, %s still exists", record.RestoreDirectory)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
