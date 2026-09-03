package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/agent"
	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
)

func TestCLIOverUnixSocket(t *testing.T) {
	stateDir := t.TempDir()
	socketPath := stateDir + "/agent.sock"
	probe, probeErr := net.Listen("unix", socketPath)
	if probeErr != nil {
		t.Skipf("Unix socket integration is unavailable in this environment: %v", probeErr)
	}
	_ = probe.Close()
	_ = os.Remove(socketPath)
	endpoint := "unix://" + socketPath
	configuration := config.DefaultAgent()
	configuration.StateDir = stateDir
	configuration.Listen = endpoint
	configuration.ShutdownTimeout = 2 * time.Second
	service, err := agent.Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	serverContext, cancelServer := context.WithCancel(context.Background())
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- service.Run(serverContext)
	}()

	client, err := agentclient.New(endpoint, time.Second)
	if err != nil {
		cancelServer()
		t.Fatal(err)
	}
	waitForAgent(t, client, serverResult)

	var workloadID string
	t.Cleanup(func() {
		if workloadID != "" {
			cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, _ = client.WorkloadAction(cleanupContext, workloadID, "stop", map[string]int{"timeout_seconds": 1})
			_ = client.DeleteWorkload(cleanupContext, workloadID)
			cleanupCancel()
		}
		cancelServer()
		select {
		case runErr := <-serverResult:
			if runErr != nil {
				t.Errorf("agent shutdown: %v", runErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("agent did not shut down")
		}
	})

	workloadRoot := t.TempDir()
	output := runTestCLI(t, endpoint, "workload", "create", "socket-smoke", "--path", workloadRoot, "--start", "--", "/bin/sh", "-c", "printf 'shift-smoke-ready\\n'; trap 'exit 0' TERM INT; while :; do sleep 1; done")
	var created model.Workload
	decodeTestCLIJSON(t, output, &created)
	workloadID = created.Spec.ID
	if workloadID == "" || created.Status != model.WorkloadRunning || created.Process == nil {
		t.Fatalf("unexpected created workload: %#v", created)
	}

	output = runTestCLI(t, endpoint, "workload", "inspect", workloadID)
	var inspected model.Workload
	decodeTestCLIJSON(t, output, &inspected)
	if inspected.Spec.Name != "socket-smoke" {
		t.Fatalf("unexpected inspected workload: %#v", inspected)
	}

	output = runTestCLI(t, endpoint, "workload", "list")
	var workloads []model.Workload
	decodeTestCLIJSON(t, output, &workloads)
	if len(workloads) != 1 || workloads[0].Spec.ID != workloadID {
		t.Fatalf("unexpected workload list: %#v", workloads)
	}

	runTestCLI(t, endpoint, "workload", "pause", workloadID)
	runTestCLI(t, endpoint, "workload", "resume", workloadID)
	waitForLogLine(t, endpoint, workloadID, "shift-smoke-ready")

	output = runTestCLI(t, endpoint, "workload", "stop", workloadID, "--timeout", "2")
	var stopped model.Workload
	decodeTestCLIJSON(t, output, &stopped)
	if stopped.Status != model.WorkloadStopped || stopped.Process != nil {
		t.Fatalf("unexpected stopped workload: %#v", stopped)
	}

	runTestCLI(t, endpoint, "workload", "delete", workloadID)
	workloadID = ""
	output = runTestCLI(t, endpoint, "workload", "list")
	decodeTestCLIJSON(t, output, &workloads)
	if len(workloads) != 0 {
		t.Fatalf("workload was not deleted: %#v", workloads)
	}
}

func waitForAgent(t *testing.T, client *agentclient.Client, serverResult <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		health, err := client.Health(ctx)
		cancel()
		if err == nil && health["status"] == "ok" {
			return
		}
		select {
		case runErr := <-serverResult:
			t.Fatalf("agent stopped during startup: %v", runErr)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("agent did not become ready")
}

func waitForLogLine(t *testing.T, endpoint, workloadID, expected string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, err := invokeTestCLI(endpoint, "logs", workloadID, "--tail", "20")
		if err == nil && strings.Contains(output, expected) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("workload logs did not contain %q", expected)
}

func runTestCLI(t *testing.T, endpoint string, arguments ...string) string {
	t.Helper()
	output, err := invokeTestCLI(endpoint, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func invokeTestCLI(endpoint string, arguments ...string) (string, error) {
	allArguments := []string{"--agent", endpoint, "--json", "--timeout", "3s"}
	allArguments = append(allArguments, arguments...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run(allArguments, &stdout, &stderr); err != nil {
		return stdout.String(), &cliTestError{err: err, stderr: stderr.String()}
	}
	return stdout.String(), nil
}

type cliTestError struct {
	err    error
	stderr string
}

func (e *cliTestError) Error() string {
	if e.stderr == "" {
		return e.err.Error()
	}
	return e.err.Error() + ": " + strings.TrimSpace(e.stderr)
}

func decodeTestCLIJSON(t *testing.T, output string, destination any) {
	t.Helper()
	if err := json.Unmarshal([]byte(output), destination); err != nil {
		t.Fatalf("decode CLI output %q: %v", output, err)
	}
}
