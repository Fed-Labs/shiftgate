package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"shift.dev/shift/internal/gc"
)

func shutdownTestService(t *testing.T) (*Service, string) {
	t.Helper()
	directory := t.TempDir()
	stateLock, err := gc.AcquireStateLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	return &Service{
		stateLock: stateLock,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, directory
}

// TestShutdownWaitsForBackgroundLoopsBeforeReleasingTheStateLock covers the
// zombie-goroutine regression: shutdown used to drain only the HTTP servers,
// release the state lock, and return while background loops (update apply,
// policy sweeps, multipart cleanup) were still writing the state directory.
func TestShutdownWaitsForBackgroundLoopsBeforeReleasingTheStateLock(t *testing.T) {
	service, directory := shutdownTestService(t)
	runContext, cancelRun := context.WithCancel(context.Background())
	release := make(chan struct{})
	finished := make(chan struct{})
	service.backgroundLoops.launch(runContext, "stuck-loop", func(ctx context.Context) {
		<-ctx.Done()
		<-release // still writing when shutdown wants to finish
		close(finished)
	})
	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- service.shutdown(context.Background(), cancelRun)
	}()
	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown returned %v while a background loop was still running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before the background loop finished")
	}
	if _, err := gc.AcquireStateLock(directory); err != nil {
		t.Fatalf("the state lock must be released after the loops stop: %v", err)
	}
}

// TestShutdownReportsLoopsThatOutliveTheDeadline covers the bounded-wait
// half: on timeout the daemon must name the loops still running instead of
// silently releasing the lock under them.
func TestShutdownReportsLoopsThatOutliveTheDeadline(t *testing.T) {
	service, _ := shutdownTestService(t)
	runContext, cancelRun := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	service.backgroundLoops.launch(runContext, "wedged-loop", func(ctx context.Context) {
		<-release // ignores cancellation entirely
	})
	shutdownContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := service.shutdown(shutdownContext, cancelRun); err == nil {
		t.Fatal("shutdown must report the timeout when a loop outlives it")
	}
	pending := service.backgroundLoops.pending()
	if len(pending) != 1 || pending[0] != "wedged-loop" {
		t.Fatalf("pending loops = %v, want [wedged-loop]", pending)
	}
}
