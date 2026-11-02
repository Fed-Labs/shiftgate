package checkpoint

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRegressionTailFileSurvivesLongLines is the LOW regression: a CRIU log
// with a huge single line (hex dumps are common exactly in the failure logs
// diagnosis needs) must not truncate the tail to nothing. The old 64 KiB
// scanner aborted on the first oversized line and silently dropped everything
// after it.
func TestRegressionTailFileSurvivesLongLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restore.log")
	var content strings.Builder
	content.WriteString(strings.Repeat("X", tailFileLineCap+4096))
	content.WriteString("\nfirst-after-huge\n")
	content.WriteString("second-after-huge\n")
	content.WriteString("third-after-huge\n")
	content.WriteString("fourth-after-huge\n")
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := tailFile(path, 3)
	for _, want := range []string{"second-after-huge", "third-after-huge", "fourth-after-huge"} {
		if !strings.Contains(tail, want) {
			t.Fatalf("the tail must survive a huge earlier line; missing %q in %q", want, tail)
		}
	}
	if strings.Contains(tail, strings.Repeat("X", 4096)) {
		t.Fatal("an oversized retained line must be capped, not copied wholesale")
	}

	// The tail of the huge line itself is still available for diagnosis.
	head := tailFile(path, 10)
	if !strings.Contains(head, "...") || !strings.Contains(head, "XXXX") {
		t.Fatalf("the huge line's tail must remain visible, got %q", head)
	}

	empty := tailFile(filepath.Join(t.TempDir(), "missing.log"), 5)
	if empty != "" {
		t.Fatalf("a missing log must produce an empty tail, got %q", empty)
	}
}

// TestRegressionCommandTimeoutScalesWithSize is the LOW regression: the
// default pipeline deadline must scale with the bytes at stake instead of
// silently capping every huge checkpoint at ten minutes.
func TestRegressionCommandTimeoutScalesWithSize(t *testing.T) {
	baseCtx, cancelBase := commandTimeout(context.Background(), 0)
	defer cancelBase()
	baseDeadline, ok := baseCtx.Deadline()
	if !ok {
		t.Fatal("the default command timeout must carry a deadline")
	}
	base := time.Until(baseDeadline)
	if base < 9*time.Minute || base > 11*time.Minute {
		t.Fatalf("the base default should stay ~10 minutes, got %v", base)
	}

	scaledCtx, cancelScaled := commandTimeout(context.Background(), 0, 8<<30)
	defer cancelScaled()
	scaledDeadline, _ := scaledCtx.Deadline()
	scaled := time.Until(scaledDeadline)
	if scaled < base+15*time.Minute {
		t.Fatalf("8 GiB of hinted size must widen the default past the flat cap, got %v", scaled)
	}

	explicitCtx, cancelExplicit := commandTimeout(context.Background(), 3*time.Second, 1<<40)
	defer cancelExplicit()
	explicitDeadline, _ := explicitCtx.Deadline()
	if until := time.Until(explicitDeadline); until > 5*time.Second {
		t.Fatalf("an explicit timeout must be honored as given, got %v", until)
	}

	hugeCtx, cancelHuge := commandTimeout(context.Background(), 0, 1<<50)
	defer cancelHuge()
	hugeDeadline, _ := hugeCtx.Deadline()
	if until := time.Until(hugeDeadline); until > maxCommandTimeout+time.Minute {
		t.Fatalf("the scaled default must stay capped, got %v", until)
	}
}

// TestRegressionCRIURestoreKillsLeakedTreeOnFailure covers the CRIU process
// leak: a restore that fails after its tree detached must not leave an
// unadopted restored process running forever. Whatever the pid file names is
// killed before the error returns.
func TestRegressionCRIURestoreKillsLeakedTreeOnFailure(t *testing.T) {
	images := t.TempDir()
	pidFile := filepath.Join(images, "work", "restore.pid")
	script := filepath.Join(t.TempDir(), "fake-criu")
	body := "#!/bin/sh\n" +
		"sleep 300 </dev/null >/dev/null 2>&1 &\n" +
		"echo $! > " + strconv.Quote(pidFile) + "\n" +
		"exit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	engine := &CRIU{binary: script, helper: os.Args[0], verbosity: 2}
	if _, err := engine.Restore(context.Background(), RestoreOptions{ImagesDirectory: images}); err == nil {
		t.Fatal("a failing restore must surface its failure")
	}
	content, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fake restore must have recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restored tree (pid %d) leaked after a failed restore", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
