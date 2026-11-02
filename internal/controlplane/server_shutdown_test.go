package controlplane

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSchemaMigrationFailureAttrsIncludeCause is the regression guard for the
// cause-less migration failure log: a deploy whose migration fails must name
// the migration and the underlying error in the log line.
func TestSchemaMigrationFailureAttrsIncludeCause(t *testing.T) {
	err := fmt.Errorf("apply migration 015_billing_v2.sql: %w", errors.New("violates foreign key constraint"))
	attrs := schemaMigrationFailureAttrs(err)
	joined := fmt.Sprint(attrs...)
	if !strings.Contains(joined, "apply migration 015_billing_v2.sql") {
		t.Fatalf("failure attrs dropped the failing migration: %v", attrs)
	}
	if !strings.Contains(joined, "violates foreign key constraint") {
		t.Fatalf("failure attrs dropped the underlying cause: %v", attrs)
	}
	if !strings.Contains(joined, "error_class") {
		t.Fatalf("failure attrs dropped the error class: %v", attrs)
	}
}

// TestAwaitWorkersDrainsAndTimesOut covers the shutdown drain: finished
// workers report drained, wedged workers cannot block exit past the bound.
func TestAwaitWorkersDrainsAndTimesOut(t *testing.T) {
	var finished sync.WaitGroup
	finished.Add(1)
	finished.Done()
	if !awaitWorkers(&finished, time.Second) {
		t.Fatal("finished workers reported as timed out")
	}
	var stuck sync.WaitGroup
	stuck.Add(1)
	defer stuck.Done()
	if awaitWorkers(&stuck, 10*time.Millisecond) {
		t.Fatal("stuck workers reported as drained")
	}
}
