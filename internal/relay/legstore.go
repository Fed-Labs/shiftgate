package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileLegStore is a durable single-node LegStore: every claim and consume is
// written to disk (temp file + rename + fsync) before it is acknowledged, so
// one-use ticket semantics survive a relay restart. It is the leg store a
// standalone relay runs with when no PostgreSQL URL is provisioned; production
// deployments that also meter bandwidth or share ticket state with the control
// plane pass database.Open instead. The store is derived entirely from
// verified ticket presentations — a forged ticket never reaches it, because
// the HMAC and audience checks run first.
type FileLegStore struct {
	path    string
	mu      sync.Mutex
	tickets map[string]*fileTicket
}

// fileTicket is one presented ticket's durable record. It mirrors the state
// the SQL leg store keeps for a ticket row.
type fileTicket struct {
	Nonce                string     `json:"nonce"`
	TokenHash            string     `json:"token_hash"`
	OrganizationID       string     `json:"organization_id"`
	MigrationID          string     `json:"migration_id"`
	SourceMachineID      string     `json:"source_machine_id"`
	DestinationMachineID string     `json:"destination_machine_id"`
	ExpiresUnix          int64      `json:"expires_unix"`
	State                string     `json:"state"`
	RelayID              string     `json:"relay_id,omitempty"`
	SourceClaimedAt      *time.Time `json:"source_claimed_at,omitempty"`
	DestinationClaimedAt *time.Time `json:"destination_claimed_at,omitempty"`
	ConsumedAt           *time.Time `json:"consumed_at,omitempty"`
}

// legStoreDocument is the on-disk envelope.
type legStoreDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	Tickets       map[string]*fileTicket `json:"tickets"`
}

const legStoreSchemaVersion = 1

// OpenFileLegStore loads (or creates) a durable leg store at path. The
// containing directory is created owner-only, and every write keeps the file
// owner-only too: it records which tickets have been burned, which is
// operational state, not a secret — but it need not be world-readable.
func OpenFileLegStore(path string) (*FileLegStore, error) {
	if path == "" {
		return nil, errors.New("a file leg store requires a path")
	}
	store := &FileLegStore{path: path, tickets: map[string]*fileTicket{}}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return store, store.persistLocked()
	case err != nil:
		return nil, err
	}
	var document legStoreDocument
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	if document.SchemaVersion != legStoreSchemaVersion {
		return nil, errors.New("unsupported leg store schema")
	}
	if document.Tickets != nil {
		store.tickets = document.Tickets
	}
	return store, nil
}

func ticketTokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// ClaimRelayLeg binds one certified machine leg of a migration pair to this
// relay, mirroring the SQL store's contract exactly: a claim is refused for a
// consumed, revoked, expired, or mismatched ticket, and a leg whose claim is
// older than two minutes may not claim again. Unlike the SQL store, the first
// claim of a standalone relay records the ticket — the durable control-plane
// row lives only when the two share a database — so every later presentation
// meets the same one-use state.
func (store *FileLegStore) ClaimRelayLeg(_ context.Context, org, migrationID, source, destination, nonce, token, machine, relayID string) error {
	if machine != source && machine != destination {
		return ErrTicketUsed
	}
	claims, err := Decode(token)
	if err != nil {
		return ErrTicketUsed
	}
	// The presentation must describe exactly the leg being claimed: a token
	// for another nonce, pair, or migration is a forged claim even if the
	// surrounding request re-uses a live ticket's parts.
	if claims.Nonce != nonce || claims.OrganizationID != org || claims.MigrationID != migrationID ||
		claims.SourceMachineID != source || claims.DestinationMachineID != destination {
		return ErrTicketUsed
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := time.Now().UTC()
	record := store.tickets[nonce]
	if record == nil {
		record = &fileTicket{
			Nonce:                nonce,
			TokenHash:            ticketTokenHash(token),
			OrganizationID:       org,
			MigrationID:          migrationID,
			SourceMachineID:      source,
			DestinationMachineID: destination,
			ExpiresUnix:          claims.ExpiresUnix,
			State:                "issued",
		}
	} else {
		if record.State != "issued" || record.TokenHash != ticketTokenHash(token) ||
			record.OrganizationID != org || record.MigrationID != migrationID ||
			record.SourceMachineID != source || record.DestinationMachineID != destination {
			return ErrTicketUsed
		}
		if !time.Unix(record.ExpiresUnix, 0).After(now) {
			return ErrTicketUsed
		}
	}
	claimed := record.SourceClaimedAt
	if machine == destination {
		claimed = record.DestinationClaimedAt
	}
	if claimed != nil && !claimed.After(now.Add(-2*time.Minute)) {
		return ErrTicketUsed
	}
	stamp := now
	if machine == destination {
		if record.DestinationClaimedAt == nil {
			record.DestinationClaimedAt = &stamp
		}
	} else if record.SourceClaimedAt == nil {
		record.SourceClaimedAt = &stamp
	}
	record.RelayID = relayID
	store.tickets[nonce] = record
	return store.persistLocked()
}

// ConsumeRelayTicket burns one ticket presentation at pair teardown, making a
// captured ticket one-use for its whole TTL. It is idempotent: consuming an
// already-consumed ticket reports ErrTicketUsed without changing state.
func (store *FileLegStore) ConsumeRelayTicket(_ context.Context, nonce, token, relayID string) error {
	if nonce == "" || token == "" || relayID == "" {
		return errors.New("relay consume fields are required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	record := store.tickets[nonce]
	if record == nil || record.State != "issued" || record.TokenHash != ticketTokenHash(token) {
		return ErrTicketUsed
	}
	now := time.Now().UTC()
	record.State = "consumed"
	record.ConsumedAt = &now
	record.RelayID = relayID
	return store.persistLocked()
}

// persistLocked writes the whole store atomically. The caller holds the mutex.
func (store *FileLegStore) persistLocked() error {
	document := legStoreDocument{SchemaVersion: legStoreSchemaVersion, Tickets: store.tickets}
	content, err := json.Marshal(document)
	if err != nil {
		return err
	}
	temporary := store.path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Rename(temporary, store.path)
}
