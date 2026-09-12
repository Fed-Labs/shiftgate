// End-to-end scenarios from the spec: run real workloads — shell process,
// Python process, Node.js application, web server, multi-process workload,
// containerized (PID-namespaced) workload — through the real agent, real
// CRIU, real encrypted chunk store. The
// assertion that matters in every one of them is the same: the process kept
// its state across the operation, evidenced by output only a surviving
// process could produce (a counter that does not restart).

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

// counterScript writes one line per second to progress.log, carrying a
// monotonically increasing counter. Line count is the survival evidence.
const counterScript = `#!/bin/sh
i=0
while true; do
  i=$((i+1))
  echo "$i" >> progress.log
  sleep 1
done
`

// TestE2EShellCheckpointRestore: start a shell workload → checkpoint it →
// restore from the checkpoint → the counter continues instead of restarting.
func TestE2EShellCheckpointRestore(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "shell-agent")
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "shell-counter", root, script)
	logPath := filepath.Join(root, "progress.log")

	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(true),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Stop the workload and prove the counter stops, so a post-restore line
	// count above the pre-checkpoint count is evidence of continuation, not
	// of a second writer.
	stopped, err := agentProc.client.WorkloadAction(ctx, workload.Spec.ID, "stop", struct{}{})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if stopped.Status != model.WorkloadStopped {
		t.Fatalf("workload did not stop: %s", stopped.Status)
	}
	baseline := fileLineCount(t, logPath)
	time.Sleep(2500 * time.Millisecond)
	if after := fileLineCount(t, logPath); after != baseline {
		t.Fatalf("counter still running after stop: %d → %d", baseline, after)
	}

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s), want %s", record.State, record.Error, checkpoint.RestoreCommitted)
	}
	if record.PID == 0 {
		t.Fatal("committed restore reports no pid")
	}
	restored := waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	// The restored process must resume the counter where it left off: the
	// line count must keep growing past the baseline, and the first line
	// written after restore must continue the sequence.
	waitUntil(t, 60*time.Second, "restored counter to write more lines", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	if restored.Spec.ID != workload.Spec.ID {
		t.Fatalf("restore created a different workload: %s", restored.Spec.ID)
	}
}

// TestE2EIncrementalCheckpointRestores proves an incremental checkpoint —
// whose CRIU image set only carries its delta against a retained parent —
// restores through the materialized parent chain. The final image set
// references unchanged pages in its parent through a symlink, so restore must
// place the ancestor image sets beside it as sibling directories; overlaying
// them into one directory would let their same-named page files shadow each
// other and the restore would fail to find pages it needs.
func TestE2EIncrementalCheckpointRestores(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "incremental-agent")
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "incremental-counter", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	parent, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(true),
	})
	if err != nil {
		t.Fatalf("full checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 30*time.Second, "counter to advance past the full checkpoint", func() (bool, string) {
		return fileLineCount(t, logPath) >= 4, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	child, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		Kind:         model.CheckpointIncremental,
		ParentID:     parent.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("incremental checkpoint: %v", err)
	}
	if child.Kind != model.CheckpointIncremental || child.ParentID != parent.ID || !child.Engine.ParentImages {
		t.Fatalf("unexpected incremental manifest: kind=%s parent=%s engine=%+v", child.Kind, child.ParentID, child.Engine)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
	baseline := fileLineCount(t, logPath)
	time.Sleep(2500 * time.Millisecond)
	if after := fileLineCount(t, logPath); after != baseline {
		t.Fatalf("counter still running after checkpoint: %d → %d", baseline, after)
	}

	record, err := agentProc.client.Restore(ctx, child.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore incremental checkpoint: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s), want %s", record.State, record.Error, checkpoint.RestoreCommitted)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
	waitUntil(t, 60*time.Second, "restored counter to write more lines", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
}

// TestE2EPeriodicCheckpointPolicy proves the agent's periodic-checkpoint
// loop against real CRIU: a workload whose spec carries a policy is
// checkpointed every interval while it keeps running, and retention
// deletes the oldest snapshot once the kept count is exceeded. The first
// checkpoint's disappearance from the list — while two newer ones exist —
// is the pruning evidence.
func TestE2EPeriodicCheckpointPolicy(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "policy-agent")
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte(counterScript), 0o755); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "policed-counter", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})
	startLines := fileLineCount(t, logPath)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	policed, err := agentProc.client.SetCheckpointPolicy(ctx, workload.Spec.ID, 10, 2)
	if err != nil {
		t.Fatalf("set checkpoint policy: %v", err)
	}
	if policed.Spec.CheckpointPolicy == nil || policed.Spec.CheckpointPolicy.IntervalSeconds != 10 || policed.Spec.CheckpointPolicy.KeepLast != 2 {
		t.Fatalf("policy was not applied: %+v", policed.Spec.CheckpointPolicy)
	}

	// The loop ticks every 5 seconds and snapshots every 10, so the first
	// periodic checkpoint appears within roughly 15 seconds.
	waitUntil(t, 90*time.Second, "first periodic checkpoint", func() (bool, string) {
		summaries, err := agentProc.client.Checkpoints(ctx, workload.Spec.ID)
		if err != nil {
			return false, err.Error()
		}
		return len(summaries) >= 1, fmt.Sprintf("checkpoints=%d", len(summaries))
	})
	summaries, err := agentProc.client.Checkpoints(ctx, workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstID := summaries[0].ID

	// A second snapshot arrives one interval later; the third pushes the
	// first past the retention count, so the first must disappear while
	// two newer checkpoints remain.
	waitUntil(t, 90*time.Second, "retention to replace the first checkpoint", func() (bool, string) {
		summaries, err := agentProc.client.Checkpoints(ctx, workload.Spec.ID)
		if err != nil {
			return false, err.Error()
		}
		for _, summary := range summaries {
			if summary.ID == firstID {
				return false, fmt.Sprintf("checkpoints=%d, first still retained", len(summaries))
			}
		}
		return len(summaries) == 2, fmt.Sprintf("checkpoints=%d", len(summaries))
	})

	summaries, err = agentProc.client.Checkpoints(ctx, workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("retention must keep exactly two checkpoints, got %d", len(summaries))
	}
	for _, summary := range summaries {
		if summary.Kind != model.CheckpointFull {
			t.Fatalf("periodic checkpoints are full snapshots, got %s", summary.Kind)
		}
		if summary.CreatedAt.Before(policed.Spec.UpdatedAt) {
			t.Fatalf("checkpoint %s predates the policy: %s", summary.ID, summary.CreatedAt)
		}
	}

	// Policy checkpoints never stop the workload, and the workload must
	// outlive several intervals of freezing and pruning.
	running := waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)
	if running.Status != model.WorkloadRunning {
		t.Fatalf("workload must keep running under its policy: %s", running.Status)
	}
	waitUntil(t, 30*time.Second, "counter to keep advancing", func() (bool, string) {
		return fileLineCount(t, logPath) > startLines+5, fmt.Sprintf("lines=%d start=%d", fileLineCount(t, logPath), startLines)
	})
}

// TestE2EPythonProcess runs a real Python interpreter workload through
// checkpoint and restore. The script keeps state in a Python variable and
// appends to a log, so a surviving process produces increasing values while
// a restarted one would produce duplicate sequence numbers.
func TestE2EPythonProcess(t *testing.T) {
	requireE2E(t)
	if !interpreterAvailable("python3") {
		t.Skip("python3 is not on PATH")
	}
	agentProc := startAgent(t, "python-agent")
	root := t.TempDir()
	script := filepath.Join(root, "counter.py")
	source := `import time
n = 0
while True:
    n += 1
    with open("progress.log", "a") as fh:
        fh.write("%d\n" % n)
    time.sleep(1)
`
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "python-counter", root, "python3", script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "python counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
	baseline := fileLineCount(t, logPath)

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	// The restored interpreter resumes with its `n` variable intact, so the
	// next appended value is baseline+1 — not 1 again.
	waitUntil(t, 60*time.Second, "restored python counter to append", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	last := lines[len(lines)-1]
	if last != fmt.Sprint(len(lines)) {
		t.Fatalf("counter restarted instead of resuming: last=%s lines=%d", last, len(lines))
	}
}

// TestE2ENodeApplication runs a real Node.js application through checkpoint
// and restore. Like the Python case, the process holds a counter variable;
// resumption is proven by the sequence continuing without a duplicate.
func TestE2ENodeApplication(t *testing.T) {
	requireE2E(t)
	if !interpreterAvailable("node") {
		t.Skip("node is not on PATH")
	}
	agentProc := startAgent(t, "node-agent")
	root := t.TempDir()
	script := filepath.Join(root, "counter.js")
	source := `const fs = require("fs");
let n = 0;
setInterval(() => {
  n += 1;
  fs.appendFileSync("progress.log", n + "\n");
}, 1000);
`
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	// libuv — the runtime library under Node — creates an io_uring ring for
	// its synchronous file operations, and CRIU 4.2 cannot dump io_uring
	// mappings: the dump aborts on the anon_inode:[io_uring] VMA. libuv 1.50
	// ignores its UV_USE_IO_URING switch, so no environment change can help.
	// This test therefore relies on the platform contract instead: a
	// privileged agent launches every workload behind a seccomp filter that
	// fails the three io_uring syscalls with EPERM, and libuv treats that
	// failure as "no io_uring on this kernel" and falls back to plain
	// syscalls — so any Node workload is checkpointable, not one configured
	// to be.
	workload := createWorkload(t, agentProc.client, "node-counter", root, "node", script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "node counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)
	baseline := fileLineCount(t, logPath)

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	waitUntil(t, 60*time.Second, "restored node counter to append", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if lines[len(lines)-1] != fmt.Sprint(len(lines)) {
		t.Fatalf("node counter restarted instead of resuming: last=%s lines=%d", lines[len(lines)-1], len(lines))
	}
}

// TestE2EWebServer runs a real web server as a workload with a declared port,
// serves real HTTP requests, checkpoints, restores, and serves again. The
// handler counts requests in memory, so after restore the counter continues
// from its pre-checkpoint value — the request numbers prove state transfer.
func TestE2EWebServer(t *testing.T) {
	requireE2E(t)
	if !interpreterAvailable("python3") {
		t.Skip("python3 is not on PATH")
	}
	agentProc := startAgent(t, "web-agent")
	root := t.TempDir()
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "server.py")
	source := fmt.Sprintf(`import http.server

hits = 0

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global hits
        hits += 1
        body = ("hit %%d\n" %% hits).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", %d), Handler).serve_forever()
`, port)
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	spec := model.WorkloadSpec{
		Name:       "web-server",
		Command:    []string{"python3", script},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
		Ports:      []model.PortSpec{{Protocol: "tcp", ContainerPort: port, HostPort: port}},
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

	fetch := func() string {
		client := &http.Client{Timeout: 5 * time.Second}
		request, requestErr := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/", port), http.NoBody)
		if requestErr != nil {
			return ""
		}
		response, err := client.Do(request)
		if err != nil {
			return ""
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return strings.TrimSpace(string(body))
	}

	waitUntil(t, 30*time.Second, "web server to accept requests", func() (bool, string) {
		response := fetch()
		return response == "hit 1", "first response was " + response
	})
	for i := 2; i <= 3; i++ {
		want := fmt.Sprintf("hit %d", i)
		waitUntil(t, 10*time.Second, "response "+want, func() (bool, string) {
			response := fetch()
			return response == want, "last response was " + response
		})
	}

	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
	}

	// In-memory hit counter resumed from its checkpointed value: the first
	// response after restore continues the sequence (a number above 3)
	// instead of restarting at 1. A connection refused while the restored
	// process rebinds is expected and retried.
	var resumed int
	waitUntil(t, 60*time.Second, "restored server to answer with a continuing hit counter", func() (bool, string) {
		response := fetch()
		if _, err := fmt.Sscanf(response, "hit %d", &resumed); err != nil {
			return false, "last response was " + response
		}
		return resumed > 3, fmt.Sprintf("last response was %q (want a hit number above 3)", response)
	})
	if resumed <= 3 {
		t.Fatalf("hit counter restarted at %d after restore instead of continuing past 3", resumed)
	}
}

// TestE2EMultiProcessWorkload checkpoints a whole process tree — one parent
// and three background workers, each tagging its output lines. CRIU must
// carry every process; after restore each of the four processes keeps
// writing. A partial restore would leave tags missing from the log's tail.
func TestE2EMultiProcessWorkload(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "tree-agent")
	root := t.TempDir()
	script := filepath.Join(root, "tree.sh")
	source := `#!/bin/sh
worker() {
  tag="$1"
  i=0
  while true; do
    i=$((i+1))
    echo "$tag $i" >> progress.log
    sleep 0.5
  done
}
worker parent &
worker child-a &
worker child-b &
wait
`
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}

	workload := createWorkload(t, agentProc.client, "process-tree", root, script)
	logPath := filepath.Join(root, "progress.log")

	allTags := func() map[string]int {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return nil
		}
		counts := make(map[string]int)
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				counts[fields[0]]++
			}
		}
		return counts
	}

	waitUntil(t, 60*time.Second, "all three processes to write", func() (bool, string) {
		counts := allTags()
		return len(counts) >= 3 && counts["parent"] > 0 && counts["child-a"] > 0 && counts["child-b"] > 0,
			fmt.Sprintf("counts=%v", counts)
	})
	baseline := allTags()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s)", record.State, record.Error)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	waitUntil(t, 90*time.Second, "every process in the restored tree to keep writing", func() (bool, string) {
		counts := allTags()
		grew := counts["parent"] > baseline["parent"] &&
			counts["child-a"] > baseline["child-a"] &&
			counts["child-b"] > baseline["child-b"]
		return grew, fmt.Sprintf("baseline=%v now=%v", baseline, counts)
	})
}

// TestE2EContainerizedWorkload runs the workload inside its own PID namespace —
// the container model, where the workload is the init of a namespace the
// platform created for it, exactly as runc does — and proves the namespace
// travels with the process instead of being dropped. The script logs each
// counter value together with the inode of its PID namespace: before the
// checkpoint every line carries one namespace id that is not the host's;
// after the restore the count continues (state survived) inside a fresh
// namespace instance (CRIU recreated the namespace rather than parking the
// process back on the host). The agent gives every workload this namespace
// at spawn time; nesting a second namespace inside it — the `unshare --pid`
// wrapper an earlier revision used — produces a nested namespace tree that
// CRIU cannot dump at all.
func TestE2EContainerizedWorkload(t *testing.T) {
	requireE2E(t)
	agentProc := startAgent(t, "container-agent")
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	source := `#!/bin/sh
i=0
while true; do
  i=$((i+1))
  echo "$i $(readlink /proc/self/ns/pid)" >> progress.log
  sleep 1
done
`
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}

	// The agent spawns the workload as the init of a fresh PID namespace of
	// its own — the same process identity a runc container has — so a plain
	// `sh` is already the containerized workload this test exercises.
	workload := createWorkload(t, agentProc.client, "pidns-counter", root, "sh", script)
	logPath := filepath.Join(root, "progress.log")

	waitUntil(t, 30*time.Second, "namespaced counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	// The workload must genuinely run in its own namespace: every line carries
	// one namespace inode that differs from the host's.
	hostNamespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatal(err)
	}
	lines := progressLines(t, logPath)
	namespace := ""
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("unexpected progress line %q", line)
		}
		if fields[1] == hostNamespace {
			t.Fatalf("workload claims to run in the host PID namespace: %q", line)
		}
		if namespace == "" {
			namespace = fields[1]
		}
		if fields[1] != namespace {
			t.Fatalf("workload changed PID namespace mid-run: %q then %q", namespace, fields[1])
		}
	}
	baseline := len(lines)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, err := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(false),
	})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadCheckpointed)

	record, err := agentProc.client.Restore(ctx, manifest.ID, agentclient.RestoreRequest{TimeoutSeconds: 300})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if record.State != checkpoint.RestoreCommitted {
		t.Fatalf("restore ended in %s (error: %s), want %s", record.State, record.Error, checkpoint.RestoreCommitted)
	}
	waitForWorkload(t, agentProc.client, workload.Spec.ID, model.WorkloadRunning)

	// The count continues past the baseline — the state survived.
	waitUntil(t, 60*time.Second, "restored namespaced counter to keep writing", func() (bool, string) {
		return fileLineCount(t, logPath) > baseline, fmt.Sprintf("lines=%d baseline=%d", fileLineCount(t, logPath), baseline)
	})
	restored := progressLines(t, logPath)
	if len(restored) <= baseline {
		t.Fatalf("no lines appeared after the restore: %d <= %d", len(restored), baseline)
	}
	// Line N carries counter value N, so the first post-restore line must
	// continue the sequence exactly where the checkpoint left it.
	resumed := strings.Fields(restored[baseline])
	if len(resumed) != 2 || resumed[0] != fmt.Sprint(baseline+1) {
		t.Fatalf("counter restarted instead of resuming: line %d is %q", baseline+1, restored[baseline])
	}
	// And it runs inside a fresh PID namespace: not the host's, and not the
	// pre-checkpoint instance the dump captured.
	restoredNamespace := ""
	for _, line := range restored[baseline:] {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("unexpected progress line %q", line)
		}
		if fields[1] == hostNamespace {
			t.Fatalf("restored process runs in the host PID namespace: %q", line)
		}
		if fields[1] == namespace {
			t.Fatalf("restored process reuses the pre-checkpoint namespace instance %s instead of a fresh one", namespace)
		}
		if restoredNamespace == "" {
			restoredNamespace = fields[1]
		}
		if fields[1] != restoredNamespace {
			t.Fatalf("restored workload changed PID namespace mid-run: %q then %q", restoredNamespace, fields[1])
		}
	}
}

// progressLines reads the workload's progress log as non-empty lines.
func progressLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}
