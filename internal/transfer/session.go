package transfer

import (
	"errors"
	"fmt"
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
	SessionRestored      SessionState = "RESTORED"
	SessionCommitted     SessionState = "COMMITTED"
	SessionRolledBack    SessionState = "ROLLED_BACK"
	SessionFailed        SessionState = "FAILED"
)

type Session struct {
	ID              string       `json:"id"`
	SourceMachineID string       `json:"source_machine_id"`
	WorkloadID      string       `json:"workload_id"`
	CheckpointID    string       `json:"checkpoint_id,omitempty"`
	KeyVersion      uint32       `json:"key_version,omitempty"`
	State           SessionState `json:"state"`
	EstimatedBytes  int64        `json:"estimated_bytes"`
	ReceivedBytes   int64        `json:"received_bytes"`
	RestoreID       string       `json:"restore_id,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	ExpiresAt       time.Time    `json:"expires_at"`
	FailureReason   string       `json:"failure_reason,omitempty"`
}

type Sessions struct {
	mu      sync.Mutex
	records *securestore.EncryptedCollection[Session]
}

func OpenSessions(stateDir string, keys *securestore.Manager) (*Sessions, error) {
	records, err := securestore.OpenEncryptedCollection[Session](filepath.Join(stateDir, "metadata", "incoming-transfers.enc.json"), "incoming-transfers-v1", keys)
	if err != nil {
		return nil, err
	}
	sessions := &Sessions{records: records}
	for _, session := range records.List() {
		if session.ExpiresAt.Before(time.Now()) && session.State != SessionCommitted && session.State != SessionRolledBack {
			session.State = SessionFailed
			session.FailureReason = "incoming transfer reservation expired"
			session.UpdatedAt = time.Now().UTC()
			_ = records.Put(session.ID, session)
		}
	}
	return sessions, nil
}

func (s *Sessions) Reserve(session Session) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session.ID == "" || session.SourceMachineID == "" || session.WorkloadID == "" {
		return Session{}, errors.New("session id, source machine, and workload are required")
	}
	if existing, err := s.records.Get(session.ID); err == nil {
		if existing.SourceMachineID != session.SourceMachineID || existing.WorkloadID != session.WorkloadID {
			return Session{}, errors.New("transfer id is already bound to another source or workload")
		}
		return existing, nil
	}
	now := time.Now().UTC()
	session.State = SessionReserved
	session.CreatedAt = now
	session.UpdatedAt = now
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
		if session.ExpiresAt.Before(time.Now()) && session.State != SessionCommitted && session.State != SessionRolledBack {
			return session, errors.New("transfer reservation expired")
		}
		if err := update(&session); err != nil {
			return session, err
		}
		session.UpdatedAt = time.Now().UTC()
		return session, nil
	})
}

func (s *Sessions) Get(id, sourceMachineID string) (Session, error) {
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
