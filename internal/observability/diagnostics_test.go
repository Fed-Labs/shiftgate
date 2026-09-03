package observability

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticsRendersMigrationSeries(t *testing.T) {
	diagnostics := NewDiagnostics()
	diagnostics.MigrationFinished(OutcomeSuccess, 10*time.Second, 500*time.Millisecond)
	diagnostics.MigrationFinished(OutcomeSuccess, 20*time.Second, time.Second)
	diagnostics.MigrationFinished(OutcomeFailure, 5*time.Second, 0)
	diagnostics.CheckpointFinished(OutcomeSuccess)
	diagnostics.CheckpointFinished(OutcomeFailure)
	diagnostics.RestoreFinished(OutcomeFailure)
	diagnostics.Transferred("upload", 1024, 2*time.Second)
	diagnostics.Transferred("download", 2048, 4*time.Second)
	diagnostics.SetGauge("memory_available_bytes", 12345)
	var rendered bytes.Buffer
	diagnostics.RenderTo(&rendered)
	output := rendered.String()
	for _, expected := range []string{
		`shift_migration_outcomes_total{outcome="failure"} 1`,
		`shift_migration_outcomes_total{outcome="success"} 2`,
		`shift_migration_success_ratio 0.6666666666666666`,
		`shift_migration_duration_seconds_last 5`,
		`shift_migration_duration_seconds_sum 35`,
		`shift_migration_downtime_seconds_last 0`,
		`shift_migration_downtime_seconds_sum 1.5`,
		`shift_checkpoint_outcomes_total{outcome="failure"} 1`,
		`shift_checkpoint_outcomes_total{outcome="success"} 1`,
		`shift_restore_outcomes_total{outcome="failure"} 1`,
		`shift_transfer_bytes_total{direction="download"} 2048`,
		`shift_transfer_bytes_total{direction="upload"} 1024`,
		`shift_transfer_seconds_sum 6`,
		`shift_memory_available_bytes 12345`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("rendered output missing %q:\n%s", expected, output)
		}
	}
}

func TestDiagnosticsRendersNoRatioWithoutHistory(t *testing.T) {
	diagnostics := NewDiagnostics()
	diagnostics.SetGauge("workloads_tracked", 3)
	var rendered bytes.Buffer
	diagnostics.RenderTo(&rendered)
	output := rendered.String()
	if strings.Contains(output, "success_ratio") {
		t.Fatalf("empty history rendered a ratio:\n%s", output)
	}
	if strings.Contains(output, "migration_duration_seconds") {
		t.Fatalf("empty history rendered durations:\n%s", output)
	}
	if !strings.Contains(output, "shift_workloads_tracked 3") {
		t.Fatalf("gauges missing:\n%s", output)
	}
}

func TestDiagnosticsRenderIsDeterministic(t *testing.T) {
	diagnostics := NewDiagnostics()
	diagnostics.MigrationFinished(OutcomeCancelled, time.Second, 0)
	diagnostics.MigrationFinished(OutcomeFailure, time.Second, 0)
	diagnostics.MigrationFinished(OutcomeSuccess, time.Second, 0)
	diagnostics.Transferred("upload", 1, time.Second)
	diagnostics.Transferred("download", 1, time.Second)
	diagnostics.SetGauge("a_first", 1)
	diagnostics.SetGauge("z_last", 1)
	var first, second bytes.Buffer
	diagnostics.RenderTo(&first)
	diagnostics.RenderTo(&second)
	if first.String() != second.String() {
		t.Fatalf("two renders differ:\n%s\n---\n%s", first.String(), second.String())
	}
	if first.Len() == 0 {
		t.Fatal("nothing rendered")
	}
}

func TestDiagnosticsIgnoresEmptyTransfers(t *testing.T) {
	diagnostics := NewDiagnostics()
	diagnostics.Transferred("upload", 0, time.Second)
	diagnostics.Transferred("upload", -1, time.Second)
	diagnostics.Transferred("upload", 512, time.Second)
	var rendered bytes.Buffer
	diagnostics.RenderTo(&rendered)
	if !strings.Contains(rendered.String(), `shift_transfer_bytes_total{direction="upload"} 512`) {
		t.Fatalf("zero and negative byte counts were recorded:\n%s", rendered.String())
	}
	if !strings.Contains(rendered.String(), "shift_transfer_seconds_sum 1") {
		t.Fatalf("ignored transfers still counted time:\n%s", rendered.String())
	}
}
