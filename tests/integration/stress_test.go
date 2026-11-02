// Stress scenarios from the spec, each gated behind SHIFT_TEST_STRESS=1 (in
// addition to SHIFT_TEST_E2E=1): huge memory states, thousands of processes,
// high network latency, source failure mid-migration, disk exhaustion, low
// memory. Every one runs the real machinery — there is no simulated failure
// here, only real ones the system must survive.

package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// runCommand runs a system command, returning its error.
func runCommand(name string, arguments ...string) error {
	command := exec.CommandContext(context.Background(), name, arguments...)
	command.Stdout = nil
	command.Stderr = nil
	return command.Run()
}

// stressBytes is the size of the huge-memory arena, overridable so a slow CI
// can still run a smaller variant of the same scenario. An override that is
// not a positive byte count fails loudly: a typo silently shrinking (or
// inflating) the arena would quietly change what the scenario proves.
func stressBytes(t testing.TB) int64 {
	t.Helper()
	if value := os.Getenv("SHIFT_TEST_STRESS_BYTES"); value != "" {
		var parsed int64
		if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil || parsed <= 0 {
			t.Fatalf("SHIFT_TEST_STRESS_BYTES=%q is not a positive byte count", value)
		}
		return parsed
	}
	return 512 << 20
}

// TestStressHugeMemoryState: a workload holds a multi-hundred-megabyte
// verified pattern in memory across checkpoint and restore. The pattern is
// written once at startup and the process verifies the whole arena before
// every rewrite pass — verify-then-write, never write-then-verify — so
// memory a restore lost or corrupted is found before anything can
// overwrite the evidence, and the logged `n` must continue past the
// checkpoint instead of restarting at 1.
func TestStressHugeMemoryState(t *testing.T) {
	requireStress(t)
	if !interpreterAvailable("python3") {
		t.Skip("python3 is not on PATH")
	}
	agentProc := startAgent(t, "memory-agent")
	root := t.TempDir()
	script := filepath.Join(root, "arena.py")
	source := fmt.Sprintf(`import time
SIZE = %d
arena = bytearray(SIZE)
step = 4096
n = 0
def pattern(offset):
    return (offset // step) %% 251
for offset in range(0, SIZE, step):
    arena[offset] = pattern(offset)
while True:
    for offset in range(0, SIZE, step):
        if arena[offset] != pattern(offset):
            raise SystemExit("memory corrupted")
    for offset in range(0, SIZE, step):
        arena[offset] = pattern(offset)
    n += 1
    with open("progress.log", "a") as fh:
        fh.write("%%d %%d\n" %% (n, len(arena)))
    time.sleep(1)
`, stressBytes(t))
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "huge-memory", root, "python3", script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 120*time.Second, "arena workload to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 1, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	// The restored process verifies the whole arena before its next rewrite
	// pass; a SystemExit("memory corrupted") would surface as the workload
	// dying. The logged `n` is the continuity signal: it must continue past
	// the pre-checkpoint count in one unbroken run — a re-executed script
	// restarts at 1, and line growth alone would pass it.
	waitUntil(t, 180*time.Second, "restored arena to keep writing verified passes", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline+2, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	assertCounterContinuity(t, logPath, baseline)
}

// TestStressThousandsOfProcesses: a supervisor spawns 1500 real processes,
// each writing heartbeats. CRIU must checkpoint and restore the entire tree
// — process count is asserted on both sides of the operation.
func TestStressThousandsOfProcesses(t *testing.T) {
	requireStress(t)
	agentProc := startAgent(t, "process-flood-agent")
	root := t.TempDir()
	script := filepath.Join(root, "flood.sh")
	source := `#!/bin/sh
i=0
while [ "$i" -lt 1500 ]; do
  i=$((i+1))
  (
    n=0
    while true; do
      n=$((n+1))
      echo "$i $n" >> heartbeats.log
      sleep 5
    done
  ) &
done
echo "spawned 1500" >> progress.log
while true; do sleep 60; done
`
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	spec := model.WorkloadSpec{
		Name:       "process-flood",
		Command:    []string{script},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
		// The pids limit must cover the tree as it really runs: 1500 worker
		// subshells, each of which forks an external /bin/sleep child every
		// five seconds — about 3000 concurrent processes plus the supervisor —
		// so 4096 is a real, enforced ceiling the workload lives under, not one
		// that would kill workers mid-spawn.
		Resources: model.ResourceRequirements{PIDs: 4096},
	}
	created, err := agentProc.client.CreateWorkload(ctx, spec)
	if err != nil {
		t.Fatalf("create workload: %v", err)
	}
	started, err := agentProc.client.WorkloadAction(ctx, created.Spec.ID, "start", struct{}{})
	if err != nil {
		t.Fatalf("start workload: %v", err)
	}
	workload := waitForWorkload(t, agentProc.client, started.Spec.ID, model.WorkloadRunning)

	distinct := func() int {
		data, err := os.ReadFile(filepath.Join(root, "heartbeats.log"))
		if err != nil {
			return 0
		}
		seen := make(map[string]bool)
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) == 2 {
				seen[fields[0]] = true
			}
		}
		return len(seen)
	}
	waitUntil(t, 120*time.Second, "all 1500 processes to write a heartbeat", func() (bool, string) {
		return distinct() == 1500, fmt.Sprintf("processes reporting=%d", distinct())
	})

	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
	before := make(map[string]int)
	for writer, values := range tagValues(t, filepath.Join(root, "heartbeats.log")) {
		before[writer] = len(values)
	}

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 600})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	// After restore the same 1500 processes must all still be heartbeating,
	// each from its own in-memory counter: every writer appends past the
	// count its checkpoint froze it at, and every writer's log stays its own
	// unbroken 1, 2, 3, ... run. A tree dropped and re-spawned from its
	// supervisor re-emits the same ids from 1 — invisible in a distinct-id
	// count, caught at each writer's first post-restore value.
	waitUntil(t, 300*time.Second, "all 1500 restored processes to keep heartbeating", func() (bool, string) {
		sequences := tagValues(t, filepath.Join(root, "heartbeats.log"))
		pending := 0
		for writer, want := range before {
			if len(sequences[writer]) <= want {
				pending++
			}
		}
		return pending == 0, fmt.Sprintf("writers still at pre-checkpoint count: %d of %d", pending, len(before))
	})
	assertTagContinuity(t, filepath.Join(root, "heartbeats.log"), before)
}

// latencyProxy is a user-space TCP proxy that delays every byte it forwards.
// It creates real network latency — no privileges, no tc, no simulation: the
// migration genuinely waits on every chunk.
type latencyProxy struct {
	listener net.Listener
	target   string
	delay    time.Duration
	done     chan struct{}
}

func startLatencyProxy(t *testing.T, target string, delay time.Duration) *latencyProxy {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for latency proxy: %v", err)
	}
	proxy := &latencyProxy{listener: listener, target: target, delay: delay, done: make(chan struct{})}
	go proxy.acceptLoop()
	t.Cleanup(func() {
		_ = listener.Close()
		<-proxy.done
	})
	return proxy
}

func (p *latencyProxy) acceptLoop() {
	defer close(p.done)
	for {
		connection, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.handle(connection)
	}
}

func (p *latencyProxy) handle(client net.Conn) {
	defer client.Close()
	upstream, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	// Both directions charge the same delay; the delay is owed per byte
	// forwarded (see delayedCopy), so a multi-megabyte transfer accumulates
	// seconds of latency regardless of how the transport chunks it.
	go delayedCopy(upstream, client, p.delay)
	delayedCopy(client, upstream, p.delay)
}

// delayedCopy forwards bytes while charging `delay` per 64 KiB of payload —
// 150 ms per 64 KiB when called with 150ms. Sleeping a flat delay per read
// instead would make the total latency depend on the peer's record sizes
// (TLS records at ~16 KiB would quadruple it past the migration's own
// timeout); charging per byte bounds the accumulated delay by the bytes
// moved: a 64 MiB transfer adds about 154 s whichever way it is chunked.
func delayedCopy(destination io.Writer, source io.Reader, delay time.Duration) {
	buffer := make([]byte, 64<<10)
	for {
		read, err := source.Read(buffer)
		if read > 0 {
			time.Sleep(delay * time.Duration(read) / (64 << 10))
			if _, writeErr := destination.Write(buffer[:read]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *latencyProxy) url() string {
	return "https://" + p.listener.Addr().String()
}

// TestStressHighNetworkLatency: a migration whose entire path runs through a
// proxy adding 150ms of latency per 64KiB forwarded — about 154 s of
// accumulated delay for the 64 MiB payload, inside the migration's own 600s
// budget. The migration must still complete and resume the workload on the
// destination, with its counter continuing.
func TestStressHighNetworkLatency(t *testing.T) {
	requireStress(t)
	source := startAgent(t, "latency-source")
	destination := startAgent(t, "latency-destination")
	proxy := startLatencyProxy(t, fmt.Sprintf("127.0.0.1:%d", destination.peerPort), 150*time.Millisecond)

	root := t.TempDir()
	payload := filepath.Join(root, "payload.bin")
	if err := writeRandomFile(t, payload, 64<<20); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "high-latency", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	created, err := source.client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: destination.machineID(t),
			AgentURL:  proxy.url(),
		},
		Mode:           model.MigrationCold,
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	// The helper's deadline must cover the migration's own 600s timeout
	// plus the delay the proxy charges, or it fails while the migration is
	// legitimately still transferring.
	migration := waitForMigrationTerminal(t, source.client, created.ID, 12*time.Minute)
	if migration.Stage != model.MigrationCompleted {
		t.Fatalf("migration under latency ended in %s (%s: %s)", migration.Stage, migration.FailureCode, migration.FailureReason)
	}
	waitForWorkload(t, destination.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 120*time.Second, "migrated counter to keep counting through latency", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	assertCounterContinuity(t, logPath, baseline)
}

// TestStressSourceFailureMidMigration: the SOURCE agent dies mid-transfer.
// Its restart must recover the in-flight migration from the journal, roll it
// back against the destination, and resume the workload locally — a machine
// death during migration can never leave the application stranded.
func TestStressSourceFailureMidMigration(t *testing.T) {
	requireStress(t)
	stateDir := t.TempDir()
	socketDir := t.TempDir()
	source := startAgentAt(t, "source-failure-a", stateDir, socketDir)
	destination := startAgent(t, "source-failure-dest")

	root := t.TempDir()
	payload := filepath.Join(root, "payload.bin")
	if err := writeRandomFile(t, payload, 256<<20); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, source.client, "source-dies", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	baseline := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	created, err := source.client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: destination.machineID(t),
			AgentURL:  destination.peerURL(),
		},
		Mode:           model.MigrationCold,
		TimeoutSeconds: 480,
	})
	if err != nil {
		t.Fatalf("create migration: %v", err)
	}

	// Kill the source agent once the transfer is underway.
	killDeadline := time.Now().Add(2 * time.Minute)
	killed := false
	for time.Now().Before(killDeadline) {
		migration, err := source.client.Migration(ctx, created.ID)
		if err != nil {
			t.Fatalf("read migration: %v", err)
		}
		if migration.Stage == model.MigrationTransfer {
			source.stop(t)
			killed = true
			break
		}
		if migrationTerminal(migration.Stage) {
			t.Fatal("migration reached a terminal stage before the source could be killed; payload too small")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !killed {
		t.Fatal("never observed the transfer stage before the kill window closed")
	}

	// The machine comes back. Recover() replays the journal on startup.
	restarted := startAgentAt(t, "source-failure-b", stateDir, t.TempDir())
	recovered := waitForMigrationTerminal(t, restarted.client, created.ID, 10*time.Minute)
	if recovered.Stage == model.MigrationCompleted {
		t.Fatal("migration completed even though the source died mid-transfer")
	}
	if recovered.FailureCode != "AGENT_RESTARTED" {
		t.Fatalf("journal recovery reported failure code %q (%s), want AGENT_RESTARTED — the in-flight migration was not settled as an agent restart",
			recovered.FailureCode, recovered.FailureReason)
	}

	// The workload is running again on the source and its counter resumed
	// from its own state — the machine death cost it its progress, not its
	// identity: the log stays the unbroken sequence and the root survives.
	waitForWorkload(t, restarted.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 120*time.Second, "recovered source counter to keep counting", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	assertCounterContinuity(t, logPath, baseline)
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("failed move destroyed the source root: %v", statErr)
	}
}

// TestStressDiskExhaustion: the agent's state directory is a 128MiB tmpfs
// (a real, small disk) and the workload's state is far larger. The
// checkpoint must fail with a real out-of-space error, the agent must stay
// healthy, and the running workload must be untouched.
func TestStressDiskExhaustion(t *testing.T) {
	requireStress(t)
	if os.Geteuid() != 0 {
		t.Skip("disk exhaustion runs as root: it mounts a tmpfs")
	}
	if _, err := exec.LookPath("mount"); err != nil {
		t.Skip("mount is not available")
	}

	stateDir := t.TempDir()
	if err := runCommand("mount", "-t", "tmpfs", "-o", "size=128m", "shift-test", stateDir); err != nil {
		t.Skipf("could not mount a tmpfs for the test: %v", err)
	}
	t.Cleanup(func() {
		_ = runCommand("umount", stateDir)
	})

	agentProc := startAgentAt(t, "disk-agent", stateDir, t.TempDir())
	root := t.TempDir()
	payload := filepath.Join(root, "payload.bin")
	if err := writeRandomFile(t, payload, 512<<20); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, agentProc.client, "disk-hungry", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:     workload.Spec.ID,
		LeaveRunning:   func(v bool) *bool { return &v }(true),
		TimeoutSeconds: 240,
	})
	if err == nil {
		t.Fatal("checkpoint succeeded although state cannot fit on disk")
	}
	t.Logf("checkpoint failed as required: %v", err)

	// The agent survived: it still answers, and the workload it was
	// checkpointing is still alive and counting.
	healthCtx, healthCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer healthCancel()
	if _, healthErr := agentProc.client.Health(healthCtx); healthErr != nil {
		t.Fatalf("agent unhealthy after disk exhaustion: %v", healthErr)
	}
	waitUntil(t, 30*time.Second, "workload to keep running after disk exhaustion", func() (bool, string) {
		return fileLineCount(t, logPath) >= 3, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
}

// TestStressLowMemory: a workload with a 64MiB cgroup memory limit tries to
// allocate 512MiB. The kernel OOM-kills it inside its cgroup — the host is
// never at risk — and the agent must report the workload as failed while
// staying healthy itself.
func TestStressLowMemory(t *testing.T) {
	requireStress(t)
	if !interpreterAvailable("python3") {
		t.Skip("python3 is not on PATH")
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skip("cgroup v2 is not mounted on this host; the memory limit could not be enforced")
	}
	agentProc := startAgent(t, "low-memory-agent")
	root := t.TempDir()
	script := filepath.Join(root, "hog.py")
	source := `buffer = bytearray(512 * 1024 * 1024)
for offset in range(0, len(buffer), 4096):
    buffer[offset] = 1
with open("done.log", "a") as fh:
    fh.write("allocated\n")
while True:
    pass
`
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	spec := model.WorkloadSpec{
		Name:       "memory-hog",
		Command:    []string{"python3", script},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
		Resources:  model.ResourceRequirements{MemoryBytes: 64 << 20},
	}
	created, err := agentProc.client.CreateWorkload(ctx, spec)
	if err != nil {
		t.Fatalf("create workload: %v", err)
	}
	if _, err := agentProc.client.WorkloadAction(ctx, created.Spec.ID, "start", struct{}{}); err != nil {
		t.Fatalf("start workload: %v", err)
	}

	// The allocation exceeds the cgroup limit, so the kernel kills the
	// process; Reconcile records the death as a failed workload.
	failed := false
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		workload, err := agentProc.client.Workload(ctx, created.Spec.ID)
		if err != nil {
			t.Fatalf("read workload: %v", err)
		}
		if workload.Status == model.WorkloadFailed {
			t.Logf("workload failed as required: %s", workload.LastError)
			failed = true
			break
		}
		if workload.Status == model.WorkloadRunning && workload.Process != nil && time.Since(workload.Process.StartedAt) > 90*time.Second {
			t.Fatal("workload is still running 90s after exceeding its memory limit; cgroup limit not applied")
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !failed {
		t.Fatal("workload never reached the failed state after exceeding its memory limit")
	}

	healthCtx, healthCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer healthCancel()
	if _, healthErr := agentProc.client.Health(healthCtx); healthErr != nil {
		t.Fatalf("agent unhealthy after an OOM kill: %v", healthErr)
	}
}
