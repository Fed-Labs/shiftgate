package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
)

// TestProgressPrinterPrintsLinesOnNonTerminals proves progress rendering stays
// greppable when stdout is not a terminal: one line per event, no in-place
// redraws.
func TestProgressPrinterPrintsLinesOnNonTerminals(t *testing.T) {
	var output bytes.Buffer
	printer := newProgressPrinter(&output)
	printer.event(model.MigrationTransfer, model.MigrationEvent{Progress: 0.5, Message: "transferring pages", BytesDone: 1024, BytesTotal: 2048})
	printer.event(model.MigrationCommit, model.MigrationEvent{Progress: 1, Message: "committing"})
	printer.end()
	if !strings.Contains(output.String(), "50%") || !strings.Contains(output.String(), "transferring pages") {
		t.Fatalf("progress line missing data: %q", output.String())
	}
	if !strings.Contains(output.String(), "1.0 KiB / 2.0 KiB") {
		t.Fatalf("byte counters missing: %q", output.String())
	}
	if strings.Contains(output.String(), "\r") {
		t.Fatalf("non-terminal output must not redraw: %q", output.String())
	}
}

func TestParseBytes(t *testing.T) {
	value, err := parseBytes("1.5GiB")
	if err != nil {
		t.Fatal(err)
	}
	if value != 1610612736 {
		t.Fatalf("got %d", value)
	}
}

func TestGPUValuesFlag(t *testing.T) {
	var gpus gpuValues
	if err := gpus.Set("vendor=NVIDIA,model=H100,memory=40GiB,compute=9.0,restore=true"); err != nil {
		t.Fatal(err)
	}
	if err := gpus.Set("vendor=amd"); err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected 2 GPU requirements, got %d", len(gpus))
	}
	first := gpus[0]
	if first.Vendor != "NVIDIA" || first.Model != "H100" || first.ComputeCapability != "9.0" {
		t.Fatalf("unexpected requirement: %+v", first)
	}
	if first.MemoryBytes != 40*1024*1024*1024 {
		t.Fatalf("memory = %d", first.MemoryBytes)
	}
	if !first.CheckpointRestore {
		t.Fatal("restore flag not parsed")
	}
	if gpus[1].Vendor != "amd" || gpus[1].CheckpointRestore || gpus[1].MemoryBytes != 0 {
		t.Fatalf("unexpected minimal requirement: %+v", gpus[1])
	}
	invalid := gpuValues{}
	if err := invalid.Set("model=H100"); err == nil {
		t.Fatal("a GPU requirement without a vendor was accepted")
	}
	if err := invalid.Set("vendor=NVIDIA,wat=1"); err == nil {
		t.Fatal("an unknown GPU field was accepted")
	}
	if err := invalid.Set("vendor=NVIDIA,memory=alot"); err == nil {
		t.Fatal("an unparseable memory size was accepted")
	}
}

func TestCheckpointMirrorCommand(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("Unix socket integration is unavailable in this environment: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/checkpoints/checkpoint-test/mirror" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(checkpoint.MirrorResult{CheckpointID: "checkpoint-test", Objects: 4, Bytes: 8192})
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		_ = server.Shutdown(context.Background())
		<-serveDone
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"--agent", "unix://" + socketPath, "--json", "checkpoint", "mirror", "checkpoint-test"}, &stdout, &stderr); err != nil {
		t.Fatalf("mirror command failed: %v: %s", err, stderr.String())
	}
	var result checkpoint.MirrorResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.CheckpointID != "checkpoint-test" || result.Objects != 4 || result.Bytes != 8192 {
		t.Fatalf("unexpected CLI result: %+v", result)
	}
}

// TestMachineCommandPrintsIdentity proves `machines` leads with the full
// machine id — the exact value a remote `migrate --machine-id` pin compares
// against, so it must never be truncated.
func TestMachineCommandPrintsIdentity(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("Unix socket integration is unavailable in this environment: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/machine" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(model.MachineCapabilities{
			MachineID: "983da9077690dfb819724b0173ee754ea1a716dcb83f991412991dfc83c36380",
			Hostname:  "workstation", OS: "linux", Architecture: "amd64",
		})
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		_ = server.Shutdown(context.Background())
		<-serveDone
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := run([]string{"--agent", "unix://" + socketPath, "machines"}, &stdout, &stderr); err != nil {
		t.Fatalf("machines command failed: %v: %s", err, stderr.String())
	}
	rendered := stdout.String()
	if !strings.Contains(rendered, "MACHINE 983da9077690dfb819724b0173ee754ea1a716dcb83f991412991dfc83c36380") {
		t.Fatalf("machines output must lead with the full machine id:\n%s", rendered)
	}
	if !strings.Contains(rendered, "workstation") {
		t.Fatalf("machines output missing the machine row:\n%s", rendered)
	}
}

// TestPrintPreflight proves the dry-run report renders the verdict, both
// machines, the network plan, and every warning and rejection with its
// remedy — the output an operator reads before deciding to migrate.
func TestPrintPreflight(t *testing.T) {
	var output bytes.Buffer
	printPreflight(&output, migration.PreflightResult{
		Workload:      model.WorkloadSpec{Name: "demo"},
		SourceMachine: model.MachineCapabilities{MachineID: "source-machine-1234", Hostname: "src", OS: "linux", Architecture: "amd64", Kernel: "6.12.38"},
		Destination: model.MachineCapabilities{
			MachineID: "destination-machine-5678", Hostname: "dst", OS: "linux", Architecture: "arm64",
			Kernel: "6.12.38", MemoryBytes: 32 << 30,
			CRIU: model.CRIUCapabilities{Installed: true, Version: "3.17.1", Healthy: true},
		},
		Mode:    model.MigrationLive,
		Network: model.NetworkPlan{Summary: "forwarders re-established on ports 8080"},
		Report: model.CompatibilityReport{Compatible: false, Issues: []model.CompatibilityIssue{
			{Code: "ARCH_MISMATCH", Severity: "error", Resource: "cpu", Description: "source is amd64 and destination is arm64", Adaptation: "select a destination with matching architecture"},
			{Code: "GPU_MODEL_DIFFERS", Severity: "warning", Resource: "gpu", Description: "workload used NVIDIA A100; destination offers NVIDIA H100", Adaptation: "validate accelerator-sensitive behavior after restore"},
		}},
	})
	rendered := output.String()
	for _, want := range []string{
		"Preflight for workload demo (mode live)",
		"destination: destinat on dst — linux/arm64",
		"source:      source-m on src — linux/amd64",
		"CRIU 3.17.1", "32.0 GiB memory",
		"forwarders re-established on ports 8080",
		"compatible:  no — the migration would be rejected",
		"rejections (1) — each of these fails the migration:",
		"ARCH_MISMATCH [cpu] source is amd64 and destination is arm64 — select a destination with matching architecture",
		"warnings (1) — the migration proceeds, but check these:",
		"GPU_MODEL_DIFFERS [gpu] workload used NVIDIA A100; destination offers NVIDIA H100",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, rendered)
		}
	}
}
