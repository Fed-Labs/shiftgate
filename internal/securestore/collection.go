package securestore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"shift.dev/shift/internal/persistence"
)

const systemMetadataKeyID = "__shift_system_metadata__"

// journalCompactEntries and journalCompactBytes bound the write-ahead journal
// a collection accumulates between full rewrites. Mutations append one
// encrypted entry instead of re-encrypting the whole collection, and the
// journal is folded back into the snapshot when it grows past its budget, so
// a progress tick costs its own entry rather than the entire history.
const (
	journalCompactEntries = 128
	journalCompactBytes   = int64(256 << 10)
)

type encryptedCollectionEnvelope struct {
	FormatVersion int    `json:"format_version"`
	KeyVersion    uint32 `json:"key_version"`
	Nonce         string `json:"nonce"`
	Ciphertext    string `json:"ciphertext"`
}

type encryptedCollectionData[T any] struct {
	FormatVersion int          `json:"format_version"`
	Revision      uint64       `json:"revision"`
	Items         map[string]T `json:"items"`
}

type collectionJournalEntry struct {
	FormatVersion int    `json:"format_version"`
	KeyVersion    uint32 `json:"key_version"`
	Revision      uint64 `json:"revision"`
	Op            string `json:"op"`
	ID            string `json:"id"`
	Nonce         string `json:"nonce"`
	Ciphertext    string `json:"ciphertext"`
}

type EncryptedCollection[T any] struct {
	mu            sync.RWMutex
	path          string
	journalPath   string
	label         string
	manager       *Manager
	data          encryptedCollectionData[T]
	journalEntryN int
	journalBytes  int64
	// journalDamaged marks a journal replay stopped short of the end:
	// corruption or a torn tail that could not be cut away. No entry may be
	// appended to such a journal — a mutation could be lost behind the
	// damage on the next replay — so the next mutation must first fold the
	// journal into a fresh snapshot.
	journalDamaged bool
}

// OpenEncryptedCollectionExisting opens a collection that must already exist.
// A missing file is an error rather than an empty collection: a verification
// tool pointed at a damaged or wiped tree must never mistake "no state" for
// "empty but healthy state".
func OpenEncryptedCollectionExisting[T any](path, label string, manager *Manager) (*EncryptedCollection[T], error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("encrypted collection %s is missing", label)
		}
		return nil, err
	}
	return OpenEncryptedCollection[T](path, label, manager)
}

func OpenEncryptedCollection[T any](path, label string, manager *Manager) (*EncryptedCollection[T], error) {
	if manager == nil {
		return nil, errors.New("secure store manager is required")
	}
	if label == "" {
		return nil, errors.New("encrypted collection label is required")
	}
	collection := &EncryptedCollection[T]{
		path: path, journalPath: path + ".journal", label: label, manager: manager,
		data: encryptedCollectionData[T]{FormatVersion: 1, Items: make(map[string]T)},
	}
	var envelope encryptedCollectionEnvelope
	if err := persistence.ReadJSON(path, &envelope); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else {
		if envelope.FormatVersion != 1 {
			return nil, fmt.Errorf("unsupported encrypted collection format %d", envelope.FormatVersion)
		}
		key, err := manager.WorkloadKey(systemMetadataKeyID, envelope.KeyVersion)
		if err != nil {
			return nil, err
		}
		nonce, err := base64.RawStdEncoding.DecodeString(envelope.Nonce)
		if err != nil {
			return nil, err
		}
		ciphertext, err := base64.RawStdEncoding.DecodeString(envelope.Ciphertext)
		if err != nil {
			return nil, err
		}
		plaintext, err := Decrypt(key, systemMetadataKeyID, "metadata-collection", nonce, ciphertext, []byte(label))
		if err != nil {
			return nil, fmt.Errorf("decrypt collection %s: %w", label, err)
		}
		if err := json.Unmarshal(plaintext, &collection.data); err != nil {
			return nil, fmt.Errorf("decode collection %s: %w", label, err)
		}
		if collection.data.FormatVersion != 1 {
			return nil, fmt.Errorf("unsupported collection payload format %d", collection.data.FormatVersion)
		}
		if collection.data.Items == nil {
			collection.data.Items = make(map[string]T)
		}
	}
	if err := collection.replayJournal(); err != nil {
		return nil, err
	}
	return collection, nil
}

// replayJournal folds the write-ahead journal into the loaded snapshot. Only
// complete lines are trusted: a torn trailing entry from a crash mid-append is
// cut away so the next append cannot merge with it, and every entry must
// authenticate against this collection's label. Entries already covered by a
// newer snapshot are skipped, so a crash between snapshot and journal cleanup
// replays harmlessly. A bad line anywhere else stops replay at that point —
// what follows an unparseable or unverifiable line is untrustworthy — but it
// never fails the open: the collection comes up on the state that could be
// proven, and the journal is folded away at the next mutation instead of
// leaving the agent with state it can never open again.
func (c *EncryptedCollection[T]) replayJournal() error {
	encoded, err := os.ReadFile(c.journalPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	c.journalBytes = int64(len(encoded))
	damaged := false
	tornTail := int64(-1)
	offset := int64(0)
	lines := bytes.Split(encoded, []byte("\n"))
	for index, line := range lines {
		lineStart := offset
		offset += int64(len(line)) + 1
		if len(line) == 0 {
			continue
		}
		if index == len(lines)-1 {
			// Torn tail: written by a crash mid-append before any newline
			// landed. It must be cut away now — the next append would merge
			// with it into one unparseable line and lose the new entry.
			tornTail = lineStart
			break
		}
		var entry collectionJournalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			damaged = true
			slog.Warn("encrypted collection journal has an unparseable entry; replay stopped there", "collection", c.label)
			break
		}
		if entry.FormatVersion != 1 || (entry.Op != "put" && entry.Op != "delete") || entry.ID == "" {
			damaged = true
			slog.Warn("encrypted collection journal has an invalid entry; replay stopped there", "collection", c.label)
			break
		}
		if entry.Revision < c.data.Revision {
			continue
		}
		key, err := c.manager.WorkloadKey(systemMetadataKeyID, entry.KeyVersion)
		if err != nil {
			damaged = true
			slog.Warn("encrypted collection journal entry cannot be decrypted; replay stopped there", "collection", c.label)
			break
		}
		nonce, err := base64.RawStdEncoding.DecodeString(entry.Nonce)
		if err != nil {
			damaged = true
			slog.Warn("encrypted collection journal entry has an invalid nonce; replay stopped there", "collection", c.label)
			break
		}
		ciphertext, err := base64.RawStdEncoding.DecodeString(entry.Ciphertext)
		if err != nil {
			damaged = true
			slog.Warn("encrypted collection journal entry has invalid ciphertext; replay stopped there", "collection", c.label)
			break
		}
		aad := []byte(fmt.Sprintf("%s\n%d\n%s\n%s", c.label, entry.Revision, entry.Op, entry.ID))
		plaintext, err := Decrypt(key, systemMetadataKeyID, "metadata-collection-entry", nonce, ciphertext, aad)
		if err != nil {
			damaged = true
			slog.Warn("encrypted collection journal entry failed authentication; replay stopped there", "collection", c.label)
			break
		}
		if entry.Op == "put" {
			var value T
			if err := json.Unmarshal(plaintext, &value); err != nil {
				damaged = true
				slog.Warn("encrypted collection journal entry has an undecodable value; replay stopped there", "collection", c.label)
				break
			}
			c.data.Items[entry.ID] = value
		} else {
			delete(c.data.Items, entry.ID)
		}
		c.data.Revision = entry.Revision
		c.journalEntryN++
	}
	if tornTail >= 0 {
		if err := c.truncateJournal(tornTail); err != nil {
			damaged = true
		} else {
			c.journalBytes = tornTail
		}
	}
	if damaged {
		c.journalDamaged = true
	}
	return nil
}

func (c *EncryptedCollection[T]) Put(id string, value T) error {
	if id == "" {
		return errors.New("record id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, existed := c.data.Items[id]
	c.data.Items[id] = value
	if err := c.appendEntry("put", id, &value); err != nil {
		if existed {
			c.data.Items[id] = previous
		} else {
			delete(c.data.Items, id)
		}
		return err
	}
	return nil
}

func (c *EncryptedCollection[T]) Update(id string, update func(T) (T, error)) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	current, ok := c.data.Items[id]
	if !ok {
		return zero, persistence.ErrNotFound
	}
	next, err := update(current)
	if err != nil {
		return zero, err
	}
	c.data.Items[id] = next
	if err := c.appendEntry("put", id, &next); err != nil {
		c.data.Items[id] = current
		return zero, err
	}
	return next, nil
}

func (c *EncryptedCollection[T]) Get(id string) (T, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.data.Items[id]
	if !ok {
		var zero T
		return zero, persistence.ErrNotFound
	}
	return value, nil
}

func (c *EncryptedCollection[T]) List() []T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.data.Items))
	for key := range c.data.Items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make([]T, 0, len(keys))
	for _, key := range keys {
		values = append(values, c.data.Items[key])
	}
	return values
}

func (c *EncryptedCollection[T]) Delete(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.data.Items[id]
	if !ok {
		return persistence.ErrNotFound
	}
	delete(c.data.Items, id)
	if err := c.appendEntry("delete", id, nil); err != nil {
		c.data.Items[id] = current
		return err
	}
	return nil
}

func (c *EncryptedCollection[T]) Revision() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data.Revision
}

func (c *EncryptedCollection[T]) persist() error {
	keyVersion, key, err := c.manager.GetOrCreateWorkloadKey(systemMetadataKeyID)
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(c.data)
	if err != nil {
		return err
	}
	nonce, ciphertext, err := Encrypt(key, systemMetadataKeyID, "metadata-collection", plaintext, []byte(c.label))
	if err != nil {
		return err
	}
	envelope := encryptedCollectionEnvelope{
		FormatVersion: 1, KeyVersion: keyVersion,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	return persistence.WriteJSON(c.path, envelope, 0o600)
}

// appendEntry durably records one mutation in the write-ahead journal: one
// encrypted, self-contained line instead of a full collection rewrite. The
// journal is the source of truth until the next compaction folds it into the
// snapshot, so a mutation is durable the moment the line is synced — and a
// compaction that cannot run right now is deferred, never allowed to undo a
// mutation that already landed. The entry's revision is assigned only after
// the line is durable, and a failed append is truncated back out of the
// journal: a rolled-back mutation leaves neither a torn line behind (which
// the next append would merge into and lose) nor a consumed revision (whose
// reuse would make replay skip the real entry that follows).
func (c *EncryptedCollection[T]) appendEntry(op, id string, value *T) error {
	next := c.data.Revision + 1
	if c.journalDamaged {
		// Replay stopped short of the journal's end at open time. Fold the
		// proven state — and this mutation — into a fresh snapshot instead
		// of appending after the damage, where a crash could lose the entry
		// behind it. The snapshot write is atomic, so once it lands the
		// mutation is durable and the damaged journal is covered by the
		// snapshot's revision on any future replay.
		previous := c.data.Revision
		c.data.Revision = next
		if err := c.persist(); err != nil {
			c.data.Revision = previous
			return err
		}
		_ = os.Remove(c.journalPath)
		c.journalEntryN = 0
		c.journalBytes = 0
		c.journalDamaged = false
		return nil
	}
	if _, err := os.Stat(c.path); errors.Is(err, os.ErrNotExist) {
		// First mutation: no snapshot exists yet, so the whole state is
		// written atomically instead of journalled. The write is atomic
		// (temp+rename), so a failure leaves nothing to roll the revision
		// back from.
		previous := c.data.Revision
		c.data.Revision = next
		if err := c.persist(); err != nil {
			c.data.Revision = previous
			return err
		}
		return nil
	}
	keyVersion, key, err := c.manager.GetOrCreateWorkloadKey(systemMetadataKeyID)
	if err != nil {
		return err
	}
	var plaintext []byte
	if value != nil {
		plaintext, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	aad := []byte(fmt.Sprintf("%s\n%d\n%s\n%s", c.label, next, op, id))
	nonce, ciphertext, err := Encrypt(key, systemMetadataKeyID, "metadata-collection-entry", plaintext, aad)
	if err != nil {
		return err
	}
	entry := collectionJournalEntry{
		FormatVersion: 1, KeyVersion: keyVersion, Revision: next,
		Op: op, ID: id,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, statErr := os.Stat(c.journalPath)
	freshJournal := errors.Is(statErr, os.ErrNotExist)
	if err := os.MkdirAll(filepath.Dir(c.journalPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(c.journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return statErr
	}
	preSize := info.Size()
	written, writeErr := file.Write(line)
	syncErr := file.Sync()
	closeErr := file.Close()
	appendErr := writeErr
	if appendErr == nil && written != len(line) {
		appendErr = io.ErrShortWrite
	}
	if appendErr == nil {
		appendErr = syncErr
	}
	// A close error after a successful sync cannot make the synced entry
	// undurable; it is the line's durability — not the descriptor's — that
	// decides whether the mutation happened.
	_ = closeErr
	if appendErr != nil {
		// Roll the bytes back out of the journal and only then let the
		// caller roll the in-memory mutation back: the file must never keep
		// a torn line or a complete entry the caller was told failed.
		if err := c.truncateJournal(preSize); err != nil {
			// The failed entry may be durable after all. Burn its revision
			// so no later entry can ever reuse it — a reused revision would
			// make replay skip the real entry that follows.
			c.data.Revision = next
		}
		return appendErr
	}
	c.data.Revision = next
	if freshJournal {
		if dir, err := os.Open(filepath.Dir(c.path)); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	c.journalEntryN++
	c.journalBytes += int64(len(line))
	c.maybeCompact()
	return nil
}

// truncateJournal cuts the journal back to a known-good length after a failed
// append (or a torn crash tail), syncing the cut so it survives a crash of
// its own.
func (c *EncryptedCollection[T]) truncateJournal(size int64) error {
	file, err := os.OpenFile(c.journalPath, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := file.Truncate(size); err != nil {
		return err
	}
	return file.Sync()
}

// maybeCompact folds the journal into the snapshot once it outgrows its
// budget. It is best-effort by design: the journal already holds the
// mutation durably, so a snapshot write that cannot run right now only
// defers compaction to a later mutation.
func (c *EncryptedCollection[T]) maybeCompact() {
	if c.journalEntryN < journalCompactEntries && c.journalBytes < journalCompactBytes {
		return
	}
	_ = c.compact()
}

func (c *EncryptedCollection[T]) compact() error {
	if err := c.persist(); err != nil {
		return err
	}
	if err := os.Remove(c.journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.journalEntryN = 0
	c.journalBytes = 0
	return nil
}
