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

// FAILED is a settled outcome: recovery skips it, and the stage machine
// refuses to drag it back through rollback — the root cause an operator reads
// must survive agent restarts.
func TestFailedIsTerminal(t *testing.T) {
	for _, stage := range []model.MigrationStage{model.MigrationCompleted, model.MigrationRolledBack, model.MigrationCancelled, model.MigrationFailed} {
		if !terminal(stage) {
			t.Fatalf("terminal(%s) = false, want true", stage)
		}
	}
	for _, stage := range []model.MigrationStage{model.MigrationCreated, model.MigrationTransfer, model.MigrationRollingBack} {
		if terminal(stage) {
			t.Fatalf("terminal(%s) = true, want false", stage)
		}
	}
	if model.CanTransition(model.MigrationFailed, model.MigrationRollingBack) {
		t.Fatal("FAILED must not transition back into ROLLING_BACK")
	}
	if !model.CanTransition(model.MigrationTransfer, model.MigrationRollingBack) {
		t.Fatal("pre-commit stages must still be able to roll back")
	}
	failed := model.Migration{Stage: model.MigrationTransfer}
	if err := failed.Transition(model.MigrationFailed, "settled", 0); err != nil {
		t.Fatal(err)
	}
	if failed.CompletedAt == nil {
		t.Fatal("FAILED must carry a completion timestamp like every terminal stage")
	}
}
