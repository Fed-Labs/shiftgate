package transfer

import (
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/securestore"
)

func newSessionStore(t *testing.T) *Sessions {
	t.Helper()
	keys, err := securestore.Open(t.TempDir() + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := OpenSessions(t.TempDir(), keys)
	if err != nil {
		t.Fatal(err)
	}
	return sessions
}

// TestReserveDefaultsZeroEstimate: a peer that declares no estimate gets the
// default reservation rather than an unbounded session, and the estimate is
// always positive so every ingest path has a real ceiling to enforce.
func TestReserveDefaultsZeroEstimate(t *testing.T) {
	sessions := newSessionStore(t)
	session, err := sessions.Reserve(Session{
		ID: "zero-estimate", SourceMachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkloadID: "workload-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.EstimatedBytes != defaultEstimatedBytes {
		t.Fatalf("zero estimate must receive the default reservation, got %d", session.EstimatedBytes)
	}
	if quotaLimit(0) <= 0 || attemptLimit(0) <= quotaLimit(0) {
		t.Fatalf("quota helpers must stay positive and layered: quota %d attempt %d", quotaLimit(0), attemptLimit(0))
	}
}

// TestExpiredSessionsRejectIngest: expiry is enforced at Get — the gate every
// ingest handler passes through — so an expired reservation stops accepting
// bytes instead of storing first and accounting later. Teardown stays
// possible through the cleanup accessors.
func TestExpiredSessionsRejectIngest(t *testing.T) {
	sessions := newSessionStore(t)
	session, err := sessions.Reserve(Session{
		ID: "expired", SourceMachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkloadID: "workload-1", ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Get(session.ID, session.SourceMachineID); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("Get on an expired session must refuse, got %v", err)
	}
	if _, err := sessions.Update(session.ID, session.SourceMachineID, func(value *Session) error {
		value.ReceivedBytes += 1
		return nil
	}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("Update on an expired session must refuse, got %v", err)
	}
	cleanup, err := sessions.GetForCleanup(session.ID, session.SourceMachineID)
	if err != nil {
		t.Fatalf("cleanup access must survive expiry: %v", err)
	}
	rolled, err := sessions.UpdateCleanup(session.ID, session.SourceMachineID, func(value *Session) error {
		value.State = SessionRolledBack
		return nil
	})
	if err != nil || rolled.State != SessionRolledBack {
		t.Fatalf("rollback bookkeeping must survive expiry: %v", err)
	}
	if cleanup.ID != session.ID {
		t.Fatalf("cleanup returned the wrong session: %+v", cleanup)
	}
}

func TestReapExpiredMarksSessionsFailed(t *testing.T) {
	sessions := newSessionStore(t)
	expired, err := sessions.Reserve(Session{
		ID: "reap-me", SourceMachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkloadID: "workload-1", ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sessions.Reserve(Session{
		ID: "keep-me", SourceMachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkloadID: "workload-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reaped := sessions.ReapExpired(); reaped != 1 {
		t.Fatalf("exactly the expired session must be reaped, got %d", reaped)
	}
	failed, err := sessions.records.Get(expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != SessionFailed || failed.FailureReason == "" {
		t.Fatalf("reaped session must be failed with a reason: %+v", failed)
	}
	kept, err := sessions.records.Get(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State != SessionReserved {
		t.Fatalf("the live session must be untouched: %+v", kept)
	}
}

func TestWrongSourceMachineIsNeverItsSession(t *testing.T) {
	sessions := newSessionStore(t)
	session, err := sessions.Reserve(Session{
		ID: "bound", SourceMachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		WorkloadID: "workload-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Get(session.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("another machine must never reach the session")
	}
}
