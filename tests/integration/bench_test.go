// Benchmarks behind docs/benchmarks.md: real agents, real CRIU, real TLS
// peer channel — the same harness the functional tests use — reporting the
// metrics the system itself measured, never numbers a poller approximated.
//
// They only run when explicitly requested, one iteration at a time; repeat
// with -count N for run-to-run spread:
//
//	SHIFT_TEST_E2E=1 go test ./tests/integration/ -bench BenchmarkE2E -benchtime 1x -v
//
// Every scenario uses the same workload shape — a python process holding
// ~96 MiB of live compressible heap plus a 64 MiB mixed-compressibility
// data directory — so the tables in the documentation compare like with
// like.

package integration

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// benchPayloadBytes is the data directory every benchmark workload carries:
// 64 MiB mixing text-shaped and incompressible blocks.
const benchPayloadBytes = 64 << 20

// benchStateScript holds ~96 MiB of compressible heap (768 × 128 KiB
// blocks) while writing one progress line per second — memory pre-copy can
// move while it barely changes, plus a liveness log.
const benchStateScript = `import time
blocks = []
for i in range(768):
    blocks.append((("bench-block-%d " % i).encode() * 20000)[:131072])
log = open("progress.log", "a", 1)
n = 0
while True:
    n += 1
    log.write("%d\n" % n)
    time.sleep(1)
`

// benchIncompressibleScript holds the same ~96 MiB heap shape as
// benchStateScript, but every block is random: zstd cannot shrink it, so the
// memory image dominates the transfer the way it does for real binary state
// (databases, ML weights, caches). It is the A/B that isolates what pre-copy
// actually buys — with the compressible heap the frozen window barely
// differs because compression already made memory cheap; with this one the
// passes move the bulk before the freeze and only the dirtied delta pays
// the frozen window.
const benchIncompressibleScript = `import os, time
blocks = [os.urandom(131072) for _ in range(768)]
log = open("progress.log", "a", 1)
n = 0
while True:
    n += 1
    log.write("%d\n" % n)
    time.sleep(1)
`

// writeMixedFile writes size bytes of three-quarters text-shaped and
// one-quarter incompressible 256 KiB blocks — a data directory that is
// neither best-case nor worst-case for compression.
func writeMixedFile(path string, size int64) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	block := int64(256 << 10)
	line := []byte("order inventory customer transaction audit record entry line\n")
	text := make([]byte, block)
	for offset := 0; offset+len(line) <= len(text); offset += len(line) {
		copy(text[offset:], line)
	}
	random := make([]byte, block)
	for written := int64(0); written < size; {
		count := block
		if remaining := size - written; remaining < count {
			count = remaining
		}
		source := text[:count]
		if (written/block)%4 == 3 {
			// Every incompressible block is freshly random: one shared
			// random block would collapse under content addressing and
			// report dedup savings no real dataset has.
			if _, err := rand.Read(random[:count]); err != nil {
				return err
			}
			source = random[:count]
		}
		if _, err := file.Write(source); err != nil {
			return err
		}
		written += count
	}
	return nil
}

// startBenchWorkload boots an agent and the benchmark workload, and waits
// until the process is provably running (two progress lines). The state
// script is the caller's choice — the compressible and incompressible heap
// variants share everything else.
func startBenchWorkload(b *testing.B, name, stateScript string) (*agentProcess, model.Workload, string) {
	b.Helper()
	if !interpreterAvailable("python3") {
		b.Skip("python3 is not on PATH")
	}
	agentProc := startAgent(b, name)
	root := b.TempDir()
	script := filepath.Join(root, "state.py")
	if err := os.WriteFile(script, []byte(stateScript), 0o644); err != nil {
		b.Fatal(err)
	}
	payload := filepath.Join(root, "data", "payload.bin")
	if err := os.MkdirAll(filepath.Dir(payload), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := writeMixedFile(payload, benchPayloadBytes); err != nil {
		b.Fatal(err)
	}
	workload := createWorkload(b, agentProc.client, name, root, "python3", script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(b, 30*time.Second, "bench workload to make progress", func() (bool, string) {
		return fileLineCount(b, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(b, logPath))
	})
	return agentProc, workload, logPath
}

// BenchmarkE2ECheckpointRestore measures a full stopped checkpoint and a
// restore of the benchmark workload, reporting the manifest's own metrics —
// sizes, dedup, chunk count, system-measured checkpoint duration — plus the
// wall-clock of the synchronous restore call.
func BenchmarkE2ECheckpointRestore(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		agentProc, workload, logPath := startBenchWorkload(b, fmt.Sprintf("bench-cp-%d", iteration), benchStateScript)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		b.ResetTimer()
		// The agent API defaults LeaveRunning to true; this benchmark
		// measures a full stopped checkpoint, so it must say so explicitly
		// or the workload never reaches WorkloadCheckpointed.
		leaveRunning := func(v bool) *bool { return &v }
		manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID:     workload.Spec.ID,
			LeaveRunning:   leaveRunning(false),
			TimeoutSeconds: 600,
		})
		if err != nil {
			b.Fatal(err)
		}
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
		baseline := fileLineCount(b, logPath)
		restoreStart := time.Now()
		record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 600})
		if err != nil {
			b.Fatal(err)
		}
		restoreWall := time.Since(restoreStart)
		b.StopTimer()
		if record.State != checkpoint.RestoreCommitted {
			b.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
		}
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
		waitUntil(b, 60*time.Second, "restored bench workload to keep counting", func() (bool, string) {
			return fileLineCount(b, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(b, logPath), baseline)
		})
		m := manifest.Metrics
		b.Logf("checkpoint: plain %d B, stored %d B (%.1f%%), deduplicated %d B, %d chunks, duration %s",
			m.PlainBytes, m.StoredBytes, 100*float64(m.StoredBytes)/float64(m.PlainBytes),
			m.DeduplicatedBytes, m.ChunkCount, m.Duration.Round(time.Millisecond))
		b.Logf("restore(wall): %s", restoreWall.Round(time.Millisecond))
	}
}

// BenchmarkE2ELazyRestore measures what lazy pages buy and what they defer.
// Each iteration restores the benchmark workload twice from equivalent
// stopped checkpoints — once lazily (the process starts while pages stream
// through userfaultfd), once eagerly — reporting both time-to-first-execution
// numbers and wall clocks side by side. The checkpoint taken between the two
// restores is also measured: a checkpoint of a lazily restored process reads
// its memory through the daemon, so its duration honestly includes the page-in
// the lazy restore deferred.
func BenchmarkE2ELazyRestore(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		agentProc, workload, logPath := startBenchWorkload(b, fmt.Sprintf("bench-lazy-%d", iteration), benchStateScript)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		leaveRunning := func(v bool) *bool { return &v }
		b.ResetTimer()
		first, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID:     workload.Spec.ID,
			LeaveRunning:   leaveRunning(false),
			TimeoutSeconds: 600,
		})
		if err != nil {
			b.Fatal(err)
		}
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
		baseline := fileLineCount(b, logPath)
		lazyStart := time.Now()
		lazyRecord, err := agentProc.client.Restore(ctx, first.ID, agentclient.RestoreRequest{TimeoutSeconds: 600, Lazy: true})
		if err != nil {
			b.Fatal(err)
		}
		lazyWall := time.Since(lazyStart)
		b.StopTimer()
		if lazyRecord.State != checkpoint.RestoreCommitted {
			b.Fatalf("lazy restore ended in %s (error: %s)", lazyRecord.State, lazyRecord.Error)
		}
		if !lazyRecord.Lazy || lazyRecord.TimeToFirstExecutionMS <= 0 {
			b.Fatalf("lazy restore record is not honest: %+v", lazyRecord)
		}
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
		waitUntil(b, 60*time.Second, "lazily restored bench workload to keep counting", func() (bool, string) {
			return fileLineCount(b, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(b, logPath), baseline)
		})
		b.ResetTimer()
		// The checkpoint after a lazy restore faults every unfaulted page in
		// through the daemon; its measured duration carries that deferred cost.
		second, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID:     workload.Spec.ID,
			LeaveRunning:   leaveRunning(false),
			TimeoutSeconds: 600,
		})
		if err != nil {
			b.Fatal(err)
		}
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
		eagerStart := time.Now()
		eagerRecord, err := agentProc.client.Restore(ctx, second.ID, agentclient.RestoreRequest{TimeoutSeconds: 600})
		if err != nil {
			b.Fatal(err)
		}
		eagerWall := time.Since(eagerStart)
		b.StopTimer()
		if eagerRecord.State != checkpoint.RestoreCommitted {
			b.Fatalf("eager restore ended in %s (error: %s)", eagerRecord.State, eagerRecord.Error)
		}
		if eagerRecord.Lazy || eagerRecord.TimeToFirstExecutionMS <= 0 {
			b.Fatalf("eager restore record is not honest: %+v", eagerRecord)
		}
		m := second.Metrics
		b.Logf("restore lazy:    time-to-first-execution %d ms, wall %s",
			lazyRecord.TimeToFirstExecutionMS, lazyWall.Round(time.Millisecond))
		b.Logf("restore eager:   time-to-first-execution %d ms, wall %s",
			eagerRecord.TimeToFirstExecutionMS, eagerWall.Round(time.Millisecond))
		b.Logf("checkpoint after lazy restore (pays the deferred page-in): duration %s, plain %d B",
			m.Duration.Round(time.Millisecond), m.PlainBytes)
	}
}

// BenchmarkE2EIncrementalCheckpoint measures a second, incremental
// checkpoint against a leave-running parent on unchanged state — the
// deduplication and delta behavior that makes repeated checkpointing cheap.
func BenchmarkE2EIncrementalCheckpoint(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		agentProc, workload, logPath := startBenchWorkload(b, fmt.Sprintf("bench-inc-%d", iteration), benchStateScript)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		leaveRunning := func(v bool) *bool { return &v }
		first, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID:   workload.Spec.ID,
			LeaveRunning: leaveRunning(true),
		})
		if err != nil {
			b.Fatal(err)
		}
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
		baseline := fileLineCount(b, logPath)
		b.ResetTimer()
		second, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
			WorkloadID:   workload.Spec.ID,
			ParentID:     first.ID,
			LeaveRunning: leaveRunning(true),
		})
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		waitForWorkload(b, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
		waitUntil(b, 30*time.Second, "workload to keep counting through both checkpoints", func() (bool, string) {
			return fileLineCount(b, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(b, logPath), baseline)
		})
		full, incremental := first.Metrics, second.Metrics
		b.Logf("full:        plain %d B, stored %d B, deduplicated %d B, duration %s",
			full.PlainBytes, full.StoredBytes, full.DeduplicatedBytes, full.Duration.Round(time.Millisecond))
		b.Logf("incremental: plain %d B, stored %d B, deduplicated %d B (%.1f%% of plain), duration %s",
			incremental.PlainBytes, incremental.StoredBytes, incremental.DeduplicatedBytes,
			100*float64(incremental.DeduplicatedBytes)/float64(incremental.PlainBytes),
			incremental.Duration.Round(time.Millisecond))
	}
}

// benchMigration runs one migration of the benchmark workload in the given
// mode and reports the migration record's own metrics — the numbers the
// orchestrator measured stage by stage.
func benchMigration(b *testing.B, iteration int, mode model.MigrationMode, passes int, stateScript string) {
	b.Helper()
	source, workload, logPath := startBenchWorkload(b, fmt.Sprintf("bench-src-%d", iteration), stateScript)
	destination := startAgent(b, fmt.Sprintf("bench-dst-%d", iteration))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	baseline := fileLineCount(b, logPath)

	b.ResetTimer()
	created, err := source.client.CreateMigration(ctx, agentclient.MigrationCreateRequest{
		WorkloadID: workload.Spec.ID,
		Destination: model.Destination{
			MachineID: destination.machineID(b),
			AgentURL:  destination.peerURL(),
		},
		Mode:           mode,
		PreCopyPasses:  passes,
		TimeoutSeconds: 600,
	})
	if err != nil {
		b.Fatal(err)
	}
	migration := waitForMigrationTerminal(b, source.client, created.ID)
	b.StopTimer()
	if migration.Stage != model.MigrationCompleted {
		b.Fatalf("migration ended in %s (%s: %s)", migration.Stage, migration.FailureCode, migration.FailureReason)
	}
	waitForWorkload(b, destination.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(b, 90*time.Second, "migrated bench workload to keep counting on the destination", func() (bool, string) {
		return fileLineCount(b, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(b, logPath), baseline)
	})
	m := migration.Metrics
	b.Logf("mode %s (passes %d): state %d B, transferred %d B (%.1f%% of state), deduplicated %d B",
		mode, passes, m.TotalStateBytes, m.TransferredBytes,
		100*float64(m.TransferredBytes)/float64(m.TotalStateBytes), m.DeduplicatedBytes)
	// The transfer split is the whole story of a live migration: how much of
	// the state moved before the freeze, and how little had to move after it.
	b.Logf("pre-copy (workload running) %d B, frozen window %d B",
		m.PreCopyTransferredBytes, m.TransferredBytes-m.PreCopyTransferredBytes)
	b.Logf("checkpoint %s, transfer %s, restore %s, downtime %s, transfer rate %.1f MiB/s",
		m.CheckpointDuration.Round(time.Millisecond), m.TransferDuration.Round(time.Millisecond),
		m.RestoreDuration.Round(time.Millisecond), m.Downtime.Round(time.Millisecond),
		m.TransferBytesPerSec/(1<<20))
	// The event timeline shows where the wall clock actually went — the stage
	// boundaries the summary metrics fold together, including any gap between
	// the restore finishing and the migration completing.
	if len(migration.Events) > 0 {
		started := migration.Events[0].Timestamp
		previous := time.Time{}
		for _, event := range migration.Events {
			step := ""
			if !previous.IsZero() {
				step = fmt.Sprintf(" (+%.2fs)", event.Timestamp.Sub(previous).Seconds())
			}
			b.Logf("t+%-7.2fs %-22s %s%s",
				event.Timestamp.Sub(started).Seconds(), event.Stage, event.Message, step)
			previous = event.Timestamp
		}
	}
}

// BenchmarkE2EColdMigration migrates the benchmark workload with the
// workload stopped for the entire capture-and-transfer window.
func BenchmarkE2EColdMigration(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		benchMigration(b, iteration, model.MigrationCold, 0, benchStateScript)
	}
}

// BenchmarkE2ELiveMigration migrates the same workload shape with up to four
// pre-copy passes while it keeps running, so only the final dump freezes it.
func BenchmarkE2ELiveMigration(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		benchMigration(b, iteration, model.MigrationLive, 4, benchStateScript)
	}
}

// BenchmarkE2EColdMigrationIncompressible and
// BenchmarkE2ELiveMigrationIncompressible repeat the migration pair over a
// heap zstd cannot shrink. The compressible pair above understates what
// pre-copy buys — compression had already made that heap nearly free to
// move, so the frozen window is dominated by the data directory in both
// modes. Here the heap itself is the bulk of the state, and the two numbers
// that matter diverge: cold must carry all of it inside the freeze, while
// live's passes move it beforehand and the frozen window pays only the
// dirtied delta plus the files.
func BenchmarkE2EColdMigrationIncompressible(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		benchMigration(b, iteration, model.MigrationCold, 0, benchIncompressibleScript)
	}
}

func BenchmarkE2ELiveMigrationIncompressible(b *testing.B) {
	requireE2E(b)
	for iteration := 0; iteration < b.N; iteration++ {
		benchMigration(b, iteration, model.MigrationLive, 4, benchIncompressibleScript)
	}
}
