// End-to-end harness: real agents, real CRIU, real workloads.
//
// These tests run the actual agent.Service the shift-agent binary runs —
// Unix-socket local API, TLS peer listener, encrypted chunk store, signed
// manifests — against real processes. They skip, with an explicit reason,
// when the environment cannot run them (no CRIU, not root, SHIFT_TEST_E2E
// unset); they never fake a pass.
//
// Run with:
//
//	SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2E -v
//
// The stress suite adds SHIFT_TEST_STRESS=1 for the heavyweight scenarios.

package integration

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/agent"
	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
)

// requireE2E skips unless the environment can really checkpoint and restore.
func requireE2E(t testing.TB) {
	t.Helper()
	recordLaneOutcome(t)
	if os.Getenv("SHIFT_TEST_E2E") != "1" {
		t.Skip("set SHIFT_TEST_E2E=1 to run end-to-end tests with real CRIU checkpoint/restore")
	}
	if os.Geteuid() != 0 {
		t.Skip("end-to-end tests run as root: CRIU dump/restore requires it")
	}
	engine, err := checkpoint.NewCRIU("")
	if err != nil {
		t.Skipf("CRIU is not available: %v", err)
	}
	if err := engine.Check(context.Background()); err != nil {
		t.Skipf("CRIU is not healthy: %v", err)
	}
}

// requireStress gates the heavyweight scenarios behind their own flag so a
// default E2E run stays minutes, not hours.
func requireStress(t testing.TB) {
	t.Helper()
	requireE2E(t)
	if os.Getenv("SHIFT_TEST_STRESS") != "1" {
		t.Skip("set SHIFT_TEST_STRESS=1 to run stress scenarios")
	}
}

// agentProcess is one real agent: a running Service plus a client connected
// to its local Unix-socket API.
type agentProcess struct {
	client   *agentclient.Client
	stateDir string
	socket   string
	peerPort int
	cancel   context.CancelFunc
	done     chan error
	stopOnce sync.Once
}

// startAgent boots a full agent against a temporary state directory. The
// local API listens on a Unix socket; the peer API on a loopback TLS port
// with a self-signed development certificate — the same configuration two
// development agents use for a real encrypted migration.
func startAgent(t testing.TB, name string) *agentProcess {
	t.Helper()
	return startAgentAt(t, name, t.TempDir(), t.TempDir())
}

// startAgentAt boots an agent over an explicit state directory, so a test can
// stop it and boot a successor over the same state — the machine-restart
// scenarios. Everything on disk (identity, workloads, checkpoints, chunk
// store, migration journal) is recovered by the new process.
func startAgentAt(t testing.TB, name, stateDir, socketDir string) *agentProcess {
	t.Helper()
	return startAgentWith(t, name, stateDir, socketDir, nil)
}

// startAgentWith boots an agent whose configuration the caller can adjust
// before the service opens — the hosted-storage scenario, whose object store
// and control-plane reporter blocks cannot be expressed through the defaults.
func startAgentWith(t testing.TB, name, stateDir, socketDir string, mutate func(*config.Agent)) *agentProcess {
	t.Helper()
	socket := filepath.Join(socketDir, name+".sock")
	// The peer port is held until the agent's remote listener binds it —
	// released immediately before Run starts — so a sibling test's port
	// search cannot claim it in between.
	port, releasePort := reservePort(t)

	configuration := config.DefaultAgent()
	configuration.StateDir = stateDir
	configuration.Listen = "unix://" + socket
	configuration.RemoteListen = fmt.Sprintf("tcp://127.0.0.1:%d", port)
	configuration.InsecureDevelopment = true
	configuration.ObjectStore = config.DefaultAgent().ObjectStore
	configuration.LogLevel = "warn"
	configuration.Updates = config.DefaultAgent().Updates
	// Harness agents are detached (no control-plane identity), so the live
	// migration gate has no entitlement source to consult. The suite runs
	// live-mode scenarios on behalf of a paid-plan deployment; the config
	// assertion is the documented detached-agent escape hatch
	// (docs/limitations.md "Plan tiering").
	configuration.Entitlement.LiveMigration = true
	// Each agent gets its own cgroup root, the way two agents on two machines
	// have theirs on two filesystems: workload cgroup paths are derived from
	// the workload id alone, so a shared root would put both agents' trees for
	// one workload in a single directory — the migration the destination
	// restores would sit in the cgroup the source's cleanup is about to
	// remove, and a stop that should be instant would retry EBUSY instead.
	configuration.CgroupRoot = filepath.Join("/sys/fs/cgroup", "shift-e2e-"+strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(name))
	if mutate != nil {
		mutate(&configuration)
	}

	service, err := agent.Open(configuration, nil)
	if err != nil {
		t.Fatalf("open agent: %v", err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	releasePort()
	go func() { done <- service.Run(runContext) }()

	client, err := agentclient.New("unix://"+socket, 2*time.Minute)
	if err != nil {
		cancel()
		t.Fatalf("connect to agent: %v", err)
	}
	waitForAgent(t, client)

	p := &agentProcess{
		client:   client,
		stateDir: stateDir,
		socket:   socket,
		peerPort: port,
		cancel:   cancel,
		done:     done,
	}
	t.Cleanup(func() {
		p.stopOnce.Do(func() {
			p.cancel()
			select {
			case <-p.done:
			case <-time.After(60 * time.Second):
				t.Errorf("agent %s did not shut down within 60s", name)
			}
		})
	})
	// A workload outlives its agent — restart adopts it — so a test that ends
	// without stopping its trees would leave them running on the machine.
	// This runs while the agent still answers; an agent deliberately killed
	// mid-test has a successor (registered later, cleaned up first) owning
	// the workload, and a dead socket simply lists nothing.
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 90*time.Second)
		if workloads, listErr := p.client.Workloads(stopCtx); listErr == nil {
			for _, workload := range workloads {
				_, _ = p.client.WorkloadAction(stopCtx, workload.Spec.ID, "stop", struct{}{})
			}
		}
		stopCancel()
	})
	return p
}

// stop shuts the agent down and waits for Run to return; used by the
// disconnect scenarios that need a hard stop mid-flight. Run reports its
// outcome on the done channel exactly once, so the wait is guarded: the
// test's explicit stop and the cleanup registered at startup share one
// shutdown instead of the second waiter blocking for its full timeout.
func (p *agentProcess) stop(t testing.TB) {
	t.Helper()
	p.stopOnce.Do(func() {
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(60 * time.Second):
			t.Fatalf("agent did not shut down within 60s")
		}
	})
}

func waitForAgent(t testing.TB, client *agentclient.Client) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := client.Health(ctx)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("agent did not become healthy within 30s")
}

func freePort() (int, error) {
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// reservePort binds an ephemeral loopback port and holds the listener until
// release, so no other test can claim the port in the gap before this test's
// own consumer binds it. Callers release as late as they can — immediately
// before the agent's peer listener or the workload's server binds — and a
// double release is a no-op.
func reservePort(t testing.TB) (int, func()) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = listener.Close() }) }
	t.Cleanup(release)
	return listener.Addr().(*net.TCPAddr).Port, release
}

// requirePortDead re-checks that a port chosen as a dead destination is still
// unbound right before the probe that must be refused. A port recycled to
// some other listener between selection and use would otherwise turn the
// must-fail probe into a spurious success (and the test into a mystery).
func requirePortDead(t testing.TB, port int) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("port %d was claimed by another process before the dead-destination probe; the probe would be answered", port)
	}
}

// peerURL is the destination endpoint a migration from another agent uses.
func (p *agentProcess) peerURL() string {
	return fmt.Sprintf("https://127.0.0.1:%d", p.peerPort)
}

// machineID returns this agent's identity, as the other side sees it.
func (p *agentProcess) machineID(t testing.TB) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	identity, err := p.client.Identity(ctx)
	if err != nil {
		t.Fatalf("read agent identity: %v", err)
	}
	return identity.ID
}

// createWorkload registers a workload through the real local API and waits
// for it to reach the running state.
func createWorkload(t testing.TB, client *agentclient.Client, name string, root string, command ...string) model.Workload {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spec := model.WorkloadSpec{
		Name:       name,
		Command:    command,
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
	}
	created, err := client.CreateWorkload(ctx, spec)
	if err != nil {
		t.Fatalf("create workload: %v", err)
	}
	started, err := client.WorkloadAction(ctx, created.Spec.ID, "start", struct{}{})
	if err != nil {
		t.Fatalf("start workload: %v", err)
	}
	waitForWorkload(t, client, started.Spec.ID, model.WorkloadRunning)
	return started
}

// waitForWorkload polls until the workload reports the wanted status.
func waitForWorkload(t testing.TB, client *agentclient.Client, workloadID string, want model.WorkloadStatus) model.Workload {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last model.Workload
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		workload, err := client.Workload(ctx, workloadID)
		cancel()
		if err != nil {
			t.Fatalf("read workload %s: %v", workloadID, err)
		}
		last = workload
		if workload.Status == want {
			return workload
		}
		if workload.Status == model.WorkloadFailed {
			t.Fatalf("workload entered failed state (last error: %q)", workload.LastError)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// A workload that never reached the wanted state is undiagnosable from
	// the status alone: the status it settled in, its recorded error, and
	// the tail of its own output usually say exactly what happened.
	detail := ""
	logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
	logs, logErr := client.Logs(logCtx, workloadID, 15)
	logCancel()
	if logErr == nil && strings.TrimSpace(logs) != "" {
		detail = "\nworkload log tail:\n" + logs
	}
	t.Fatalf("workload %s did not reach %s within 60s (last status %s, last error %q)%s",
		workloadID, want, last.Status, last.LastError, detail)
	return model.Workload{}
}

// waitUntil polls a condition with a deadline and a failure message.
func waitUntil(t testing.TB, timeout time.Duration, description string, condition func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, detail := condition()
		if ok {
			return
		}
		time.Sleep(250 * time.Millisecond)
		_ = detail
	}
	_, detail := condition()
	t.Fatalf("timed out after %s waiting for %s (%s)", timeout, description, detail)
}

// interpreterAvailable reports whether a real interpreter exists on PATH.
func interpreterAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// migrationTerminal reports whether the stage admits no further transitions.
func migrationTerminal(stage model.MigrationStage) bool {
	switch stage {
	case model.MigrationCompleted, model.MigrationFailed, model.MigrationRolledBack, model.MigrationCancelled:
		return true
	}
	return false
}

// waitForMigrationTerminal polls the migration until it reaches a terminal
// stage and returns it, failing the test on the timeout instead of guessing.
// The timeout is the caller's: it must cover the migration's own
// TimeoutSeconds (and any latency the scenario deliberately adds), or the
// helper fails while the migration is legitimately still working.
func waitForMigrationTerminal(t testing.TB, client *agentclient.Client, migrationID string, timeout time.Duration) model.Migration {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		migration, err := client.Migration(ctx, migrationID)
		cancel()
		if err != nil {
			t.Fatalf("read migration %s: %v", migrationID, err)
		}
		if migrationTerminal(migration.Stage) {
			return migration
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("migration %s did not reach a terminal stage within %s", migrationID, timeout)
	return model.Migration{}
}

// fileLineCount counts lines in a file inside a workload root — the evidence
// that a process kept running across checkpoint/restore/migration.
func fileLineCount(t testing.TB, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

// writeRandomFile writes size bytes of crypto-random data to path — an
// incompressible payload that forces real chunk transfer work.
func writeRandomFile(t testing.TB, path string, size int64) error {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	buffer := make([]byte, 1<<20)
	for written := int64(0); written < size; {
		chunk := int64(len(buffer))
		if remaining := size - written; remaining < chunk {
			chunk = remaining
		}
		if _, err := rand.Read(buffer[:chunk]); err != nil {
			return err
		}
		if _, err := file.Write(buffer[:chunk]); err != nil {
			return err
		}
		written += chunk
	}
	return nil
}

// corruptFirstChunk flips bytes in the middle of the first chunk file it
// finds under root. The bytes corrupted are ciphertext, so the AEAD tag no
// longer matches and any honest reader must reject the chunk.
func corruptFirstChunk(t testing.TB, root string) (string, error) {
	t.Helper()
	var chunkPath string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || chunkPath != "" || entry.IsDir() || filepath.Ext(path) != ".chunk" {
			return walkErr
		}
		chunkPath = path
		return nil
	})
	if err != nil {
		return "", err
	}
	if chunkPath == "" {
		return "", os.ErrNotExist
	}
	file, err := os.OpenFile(chunkPath, os.O_RDWR, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	// Flip bits in the middle of the file: header bytes might be parsed
	// before authentication, but the body is authenticated ciphertext.
	offset := info.Size() / 2
	buffer := make([]byte, 32)
	if _, err := file.ReadAt(buffer, offset); err != nil {
		return "", err
	}
	for i := range buffer {
		buffer[i] ^= 0xFF
	}
	if _, err := file.WriteAt(buffer, offset); err != nil {
		return "", err
	}
	return chunkPath, nil
}

// counterLines reads a workload's progress log as complete non-empty lines,
// tolerating a writer caught mid-append on its final partial line (the tail
// is dropped; the next poll sees it whole).
func counterLines(t testing.TB, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read counter log %s: %v", path, err)
	}
	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		if cut := strings.LastIndex(text, "\n"); cut >= 0 {
			text = text[:cut+1]
		} else {
			text = ""
		}
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertCounterContinuity pins the evidence a counter log must carry across
// any checkpoint, restore, migration, clone, or failover: the log is the
// unbroken counting sequence 1, 2, 3, ... with no restart and no gap, and
// the first line appended past the `before` lines written before the
// operation continues at before+1. A restore that re-executed the workload
// command instead of resuming its state re-logs 1 and breaks the sequence at
// exactly that line; log growth alone cannot tell the two apart.
func assertCounterContinuity(t testing.TB, path string, before int) {
	t.Helper()
	lines := counterLines(t, path)
	if len(lines) <= before {
		t.Fatalf("counter log %s has %d lines, want more than the %d written before the operation", path, len(lines), before)
	}
	values := make([]int, len(lines))
	for i, line := range lines {
		var value int
		if _, err := fmt.Sscanf(line, "%d", &value); err != nil {
			t.Fatalf("counter log line %d (%q) carries no counter value", i+1, line)
		}
		values[i] = value
	}
	if values[before] != before+1 {
		t.Fatalf("counter did not continue across the operation: the first line written after it is line %d with value %d, want %d (a restarted counter re-logs 1; growth alone hides it)",
			before+1, values[before], before+1)
	}
	for i, value := range values {
		if value != i+1 {
			t.Fatalf("counter log is not the unbroken sequence: line %d carries %d, want %d", i+1, value, i+1)
		}
	}
}

// tagValues reads a "writer value" log (a process tree whose workers each
// tag their lines) into per-writer counter sequences, in write order.
func tagValues(t testing.TB, path string) map[string][]int {
	t.Helper()
	sequences := make(map[string][]int)
	for _, line := range counterLines(t, path) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("tagged counter line %q is not \"writer value\"", line)
		}
		var value int
		if _, err := fmt.Sscanf(fields[1], "%d", &value); err != nil {
			t.Fatalf("tagged counter line %q carries no counter value", line)
		}
		sequences[fields[0]] = append(sequences[fields[0]], value)
	}
	return sequences
}

// assertTagContinuity is the per-writer form of assertCounterContinuity:
// every writer's own counter must be its own unbroken 1, 2, 3, ... sequence,
// and every writer must have appended past the line count (`before`) it held
// when the operation began. A dropped-and-respawned tree re-emits the same
// writer ids from 1 and is caught at each writer's first post-operation
// line; whole-log growth or distinct-id counts cannot tell the two apart.
func assertTagContinuity(t testing.TB, path string, before map[string]int) {
	t.Helper()
	sequences := tagValues(t, path)
	for writer, want := range before {
		values := sequences[writer]
		if len(values) <= want {
			t.Fatalf("writer %q stopped at its pre-operation count: %d lines before, %d now — want at least one line appended past it", writer, want, len(values))
		}
		for i, value := range values {
			if value != i+1 {
				t.Fatalf("writer %q counter restarted or skipped: its line %d carries %d, want %d", writer, i+1, value, i+1)
			}
		}
	}
}
