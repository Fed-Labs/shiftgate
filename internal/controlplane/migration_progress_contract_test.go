package controlplane

import (
	"encoding/json"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

// The migration progress document must carry exactly the keys the dashboard
// consumes (report 48 contract; F25 reads these names): current_stage,
// percent (0-100), downtime_ms, transfer_speed, deduplicated_bytes,
// transferred_bytes, total_state_bytes — plus the legacy stage/progress/
// agent_id keys older clients and the cancel path rely on.
func TestMigrationProgressContractKeys(t *testing.T) {
	result := model.Migration{
		ID: "agent-migration-1",
		Events: []model.MigrationEvent{
			{Sequence: 1, Stage: model.MigrationSnapshot, Progress: 0.25},
			{Sequence: 2, Stage: model.MigrationTransfer, Progress: 0.75},
		},
		Stage: model.MigrationTransfer,
		Metrics: model.MigrationMetrics{
			TotalStateBytes:     4096,
			TransferredBytes:    2048,
			DeduplicatedBytes:   512,
			Downtime:            1500 * time.Millisecond,
			TransferBytesPerSec: 1024.5,
		},
	}
	progress := migrationProgress(result)
	for _, key := range []string{"stage", "current_stage", "progress", "percent", "agent_id", "downtime_ms", "transfer_speed", "deduplicated_bytes", "transferred_bytes", "total_state_bytes"} {
		if _, ok := progress[key]; !ok {
			t.Fatalf("progress document is missing contract key %q: %v", key, progress)
		}
	}
	if progress["current_stage"] != string(model.MigrationTransfer) || progress["stage"] != string(model.MigrationTransfer) {
		t.Fatalf("current_stage = %v/%v, want %q", progress["current_stage"], progress["stage"], model.MigrationTransfer)
	}
	if progress["percent"] != 75.0 {
		t.Fatalf("percent = %v, want 75 (0-100 scale)", progress["percent"])
	}
	if progress["progress"] != 0.75 {
		t.Fatalf("progress = %v, want 0.75 (0..1 fraction)", progress["progress"])
	}
	if progress["agent_id"] != "agent-migration-1" {
		t.Fatalf("agent_id = %v, want the agent migration id", progress["agent_id"])
	}
	if progress["downtime_ms"] != int64(1500) {
		t.Fatalf("downtime_ms = %v, want 1500", progress["downtime_ms"])
	}
	if progress["transfer_speed"] != 1024.5 {
		t.Fatalf("transfer_speed = %v, want 1024.5 bytes/sec", progress["transfer_speed"])
	}
	if progress["deduplicated_bytes"] != int64(512) || progress["transferred_bytes"] != int64(2048) || progress["total_state_bytes"] != int64(4096) {
		t.Fatalf("byte counters = %v/%v/%v, want 512/2048/4096", progress["deduplicated_bytes"], progress["transferred_bytes"], progress["total_state_bytes"])
	}

	result.Stage = model.MigrationCompleted
	done := migrationProgress(result)
	if done["percent"] != 100.0 || done["progress"] != 1.0 {
		t.Fatalf("completed percent/progress = %v/%v, want 100/1", done["percent"], done["progress"])
	}
}

func TestMigrationStageProgressCarriesContractKeys(t *testing.T) {
	encoded := migrationStageProgress("CANCELLED")
	var progress map[string]any
	if err := json.Unmarshal(encoded, &progress); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stage", "current_stage", "progress", "percent", "agent_id", "downtime_ms", "transfer_speed", "deduplicated_bytes", "transferred_bytes", "total_state_bytes"} {
		if _, ok := progress[key]; !ok {
			t.Fatalf("stage progress document is missing contract key %q: %v", key, progress)
		}
	}
	if progress["current_stage"] != "CANCELLED" || progress["percent"] != 0.0 {
		t.Fatalf("cancelled progress = %v", progress)
	}
}
