package migration

import (
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

func TestAppendEventBoundsHistory(t *testing.T) {
	migration := model.Migration{Stage: model.MigrationTransfer}
	for index := 0; index < 300; index++ {
		appendEvent(&migration, "progress", float64(index)/300, int64(index), 300)
	}
	if len(migration.Events) != 256 {
		t.Fatalf("event history length %d, want 256", len(migration.Events))
	}
	if migration.Events[len(migration.Events)-1].Timestamp.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("event timestamp is stale")
	}
}

func TestCompatibilityFailureIncludesOnlyErrors(t *testing.T) {
	err := compatibilityFailure(model.CompatibilityReport{Issues: []model.CompatibilityIssue{
		{Code: "WARN", Severity: "warning", Description: "warning"},
		{Code: "ERR", Severity: "error", Description: "failure"},
	}})
	if err.Error() != "ERR: failure" {
		t.Fatalf("unexpected compatibility error %q", err)
	}
}
