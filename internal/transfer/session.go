package transfer

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"shift.dev/shift/internal/securestore"
)

type SessionState string

const (
	SessionReserved      SessionState = "RESERVED"
	SessionKeyReady      SessionState = "KEY_READY"
	SessionManifestReady SessionState = "MANIFEST_READY"
	SessionVerified      SessionState = "VERIFIED"
	// SessionHeld is a replication session the standby accepted and parked:
	// its manifest is imported, its chunks are stored, and the checkpoint is
	// restorable at any time. The standby's duty record — not the session —
	// is the operational truth from here on; the session is the transport
	// envelope and expires on its own clock.
	SessionHeld       SessionState = "HELD"
	SessionRestored   SessionState = "RESTORED"
	SessionCommitted  SessionState = "COMMITTED"
	SessionRolledBack SessionState = "ROLLED_BACK"
	SessionFailed     SessionState = "FAILED"
)

// PurposeReplication marks a session as a warm-standby replication push
// rather than a migration. The empty purpose means a migration.
const PurposeReplication = "replication"

// validPurpose reports whether a reserve request's purpose is one this
// build understands. An unknown purpose is refused rather than treated as a
// migration: a future purpose this agent cannot honor must fail loudly, not
// silently mis-handle.
func validPurpose(purpose string) bool {
	return purpose == "" || purpose == PurposeReplication
}

type Session struct {
	ID              string `json:"id"`
	SourceMachineID string `json:"source_machine_id"`
	WorkloadID      string `json:"workload_id"`
	// OrganizationID is stamped at reservation time from the serving agent's
	// configuration: the completion meter records bandwidth against it.
	OrganizationID string       `json:"organization_id,omitempty"`
	CheckpointID   string       `json:"checkpoint_id,omitempty"`
	KeyVersion     uint32       `json:"key_version,omitempty"`
	State          SessionState `json:"state"`
	EstimatedBytes int64        `json:"estimated_bytes"`
	ReceivedBytes  int64        `json:"received_bytes"`
	// AttemptedBytes counts every chunk put to this session, including
	// retries and deduplicated uploads. It is the absolute ceiling that
	// keeps a peer from consuming unbounded disk and CPU through repeated
	// uploads even when nothing new is stored.
	AttemptedBytes int64  `json:"attempted_bytes,omitempty"`
	RestoreID      string `json:"restore_id,omitempty"`
	// Purpose distinguishes a migration session from a warm-standby
	// replication push. Empty means a migration.
	Purpose string `json:"purpose,omitempty"`
	// SourceAgentURL is the replication source's advertised peer listener,
	// recorded when the standby accepts a held session so its supervisor
	// knows where to probe the source's liveness. Empty for migrations, and
	// possibly empty for a replication whose source advertises no dialable
	// URL — such a standby can never confirm death automatically and only an
	// operator-commanded failover is possible.
	SourceAgentURL string `json:"source_agent_url,omitempty"`
	// KeepLast is the standby retention the source asked for with its held
	// replication session. Zero keeps every replicated checkpoint.
	KeepLast        int       `json:"keep_last,omitempty"`
	FencingProvider string    `json:"fencing_provider,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	FailureReason   string    `json:"failure_reason,omitempty"`
}

type Sessions struct {
	mu      sync.Mutex
	records *securestore.EncryptedCollection[Session]
	logger  *slog.Logger
	// reapRestoreHandler tears down the restored copy an expired session
	// left behind, so a reservation clock can never strand a second live
	// workload. See SetReapRestoreHandler.
	reapRestoreHandler func(Session)
}

// defaultEstimatedBytes is the reservation a session gets when the peer
// declares no estimate. A zero estimate used to disable the size quota
// entirely; instead it now reserves this default, so ingest is always bounded
// and actual bytes are accounted against a real ceiling.
const defaultEstimatedBytes = int64(1 << 30)

// quotaLimit is how far past its reservation a session may reach before
// further chunks are refused: a 5% tolerance for estimate error.
func quotaLimit(estimated int64) int64 {
	if estimated <= 0 {
		estimated = defaultEstimatedBytes
	}
	return estimated + estimated/20
}

// attemptLimit is the absolute ceiling on put attempts (retries and dedup
// included) for one session: three times the reservation plus one maximum
// chunk. The client may retry each chunk several times on a flaky link, and
// every attempt counts — a legitimate transfer of about the estimate must not
// trip the quota mid-flight because of retries.
func attemptLimit(estimated int64) int64 {
	if estimated <= 0 {
		estimated = defaultEstimatedBytes
	}
	return 3*estimated + (96 << 20)
}

func sessionExpired(session Session, now time.Time) bool {
	return session.ExpiresAt.Before(now) && session.State != SessionCommitted && session.State != SessionRolledBack
}

// terminalState reports whether a session has reached a state no further
// ingest can leave: a reservation retried against one of these must be
// refused, not echoed back as a success.
func terminalState(state SessionState) bool {
	switch state {
	case SessionCommitted, SessionRolledBack, SessionFailed:
		return true
	}
	return false
}

func OpenSessions(stateDir string, keys *securestore.Manager) (*Sessions, error) {
	records, err := securestore.OpenEncryptedCollection[Session](filepath.Join(stateDir, "metadata", "incoming-transfers.enc.json"), "incoming-transfers-v1", keys)
	if err != nil {
		return nil, err
	}
	sessions := &Sessions{records: records, logger: slog.Default()}
	sessions.ReapExpired()
	return sessions, nil
}

// SetLogger routes the session store's bookkeeping warnings to the agent's
// logger. Persistence failures on quota counters and expiry marks are never
// silently swallowed again.
func (s *Sessions) SetLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logger = logger
}

// SetReapRestoreHandler routes reaped sessions that left a restore behind to
// the restorer. An expired non-terminal session must not keep a restored
// workload running on the destination: the source may still consider itself
// live, and two copies of one workload are exactly what the migration
// invariant forbids. The handler runs outside the store lock.
func (s *Sessions) SetReapRestoreHandler(handler func(Session)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reapRestoreHandler = handler
}

// ReapExpired marks every expired non-terminal session failed and returns how
// many it marked. Boot runs it once; the server runs it on new reservations so
// abandoned sessions are retired while the agent lives, not only at restart.
// Sessions that had restored a workload are handed to the restore handler so
// the restored copy is rolled back with them.
func (s *Sessions) ReapExpired() int {
	s.mu.Lock()
	reaped := 0
	var restored []Session
	now := time.Now()
	for _, session := range s.records.List() {
		if !sessionExpired(session, now) {
			continue
		}
		session.State = SessionFailed
		session.FailureReason = "incoming transfer reservation expired"
		session.UpdatedAt = time.Now().UTC()
		if err := s.records.Put(session.ID, session); err != nil {
			s.logger.Warn("could not mark expired transfer session failed", "session_id", session.ID, "error", err)
			continue
		}
		reaped++
		if session.RestoreID != "" {
			restored = append(restored, session)
		}
	}
	handler := s.reapRestoreHandler
	s.mu.Unlock()
	// Outside the lock: the handler rolls restores back through the
	// checkpoint service and must never run inside the store's critical
	// section.
	if handler != nil {
		for _, session := range restored {
			handler(session)
		}
	}
	return reaped
}

func (s *Sessions) Reserve(session Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.ID == "" || session.SourceMachineID == "" || session.WorkloadID == "" {
		return Session{}, errors.New("session id, source machine, and workload are required")
	}
	if !validPurpose(session.Purpose) {
		return Session{}, fmt.Errorf("unknown transfer purpose %q", session.Purpose)
	}
	if existing, err := s.records.Get(session.ID); err == nil {
		if existing.SourceMachineID != session.SourceMachineID || existing.WorkloadID != session.WorkloadID {
			return Session{}, errors.New("transfer id is already bound to another source or workload")
		}
		if existing.Purpose != session.Purpose {
			return Session{}, errors.New("transfer id is already bound to another purpose")
		}
		// A dead reservation is not re-usable: answering 201 with a FAILED,
		// expired, or otherwise terminal session would hand the source a
		// success it can never ingest into. A live non-terminal session is
		// still returned unchanged — that is the idempotent retry contract.
		if sessionExpired(existing, time.Now()) || terminalState(existing.State) {
			return Session{}, errors.New("transfer id is expired or already used")
		}
		return existing, nil
	}
	now := time.Now().UTC()
	session.State = SessionReserved
	session.CreatedAt = now
	session.UpdatedAt = now
	if session.EstimatedBytes <= 0 {
		session.EstimatedBytes = defaultEstimatedBytes
	}
	if session.ExpiresAt.IsZero() || session.ExpiresAt.After(now.Add(24*time.Hour)) {
		session.ExpiresAt = now.Add(2 * time.Hour)
	}
	if err := s.records.Put(session.ID, session); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Sessions) Update(id, sourceMachineID string, update func(*Session) error) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records.Update(id, func(session Session) (Session, error) {
		if session.SourceMachineID != sourceMachineID {
			return session, errors.New("transfer belongs to another source machine")
		}
		if sessionExpired(session, time.Now()) {
			return session, errors.New("transfer reservation expired")
		}
		if err := update(&session); err != nil {
			return session, err
		}
		session.UpdatedAt = time.Now().UTC()
		return session, nil
	})
}

// Get returns a session to its authenticated source. An expired reservation
// is refused here — the same rule Update enforces — so every handler that
// reads through Get (including all ingest) rejects writes on expired
// sessions instead of storing bytes first and discovering the expiry after.
func (s *Sessions) Get(id, sourceMachineID string) (Session, error) {
	session, err := s.records.Get(id)
	if err != nil {
		return Session{}, err
	}
	if session.SourceMachineID != sourceMachineID {
		return Session{}, errors.New("transfer belongs to another source machine")
	}
	if sessionExpired(session, time.Now()) {
		return Session{}, errors.New("transfer reservation expired")
	}
	return session, nil
}

// UpdateCleanup mutates a session for teardown bookkeeping (rollback, failure
// marks) without the expiry gate. Source-machine binding still applies.
func (s *Sessions) UpdateCleanup(id, sourceMachineID string, update func(*Session) error) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records.Update(id, func(session Session) (Session, error) {
		if session.SourceMachineID != sourceMachineID {
			return session, errors.New("transfer belongs to another source machine")
		}
		if err := update(&session); err != nil {
			return session, err
		}
		session.UpdatedAt = time.Now().UTC()
		return session, nil
	})
}

// GetForCleanup returns a session for rollback bookkeeping. Unlike Get it
// ignores expiry: tearing down what an abandoned session left behind must
// stay possible after the reservation clock runs out. The source-machine
// binding still applies.
func (s *Sessions) GetForCleanup(id, sourceMachineID string) (Session, error) {
	session, err := s.records.Get(id)
	if err != nil {
		return Session{}, err
	}
	if session.SourceMachineID != sourceMachineID {
		return Session{}, errors.New("transfer belongs to another source machine")
	}
	return session, nil
}

func (s *Sessions) List() []Session {
	values := s.records.List()
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt.After(values[j].CreatedAt) })
	return values
}

func requireState(session *Session, allowed ...SessionState) error {
	for _, state := range allowed {
		if session.State == state {
			return nil
		}
	}
	return fmt.Errorf("transfer is in %s state", session.State)
}
