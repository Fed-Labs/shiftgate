package securestore

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedCollectionDoesNotExposeValues(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("id", "sensitive-command-and-environment"); err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("id2", "sensitive-command-and-environment"); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".journal"} {
		encoded, err := os.ReadFile(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "sensitive-command") {
			t.Fatalf("%s exposed plaintext", filepath.Base(candidate))
		}
	}
	reopened, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get("id")
	if err != nil || value != "sensitive-command-and-environment" {
		t.Fatalf("unexpected reopened value %q: %v", value, err)
	}
	second, err := reopened.Get("id2")
	if err != nil || second != "sensitive-command-and-environment" {
		t.Fatalf("journal replay lost the second record: %q %v", second, err)
	}
}

func TestEncryptedCollectionJournalCompactsAndSurvivesTornTail(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("first", "one"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < journalCompactEntries; index++ {
		if err := collection.Put("bulk", "value"); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() <= snapshot.Size() {
		t.Fatal("compaction must fold the journal into a fresh snapshot")
	}
	if _, err := os.Stat(path + ".journal"); !os.IsNotExist(err) {
		t.Fatal("compaction must clear the journal")
	}
	if err := collection.Put("torn", "value"); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(path+".journal", os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString(`{"format_version":1,"truncated`); err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	reopened, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get("torn")
	if err != nil || value != "value" {
		t.Fatalf("torn journal tail must be dropped, prior entries kept: %q %v", value, err)
	}
}

// fabricateJournalEntry builds one journal line exactly as appendEntry would
// have written it, so tests can plant specific journal histories.
func fabricateJournalEntry(t *testing.T, manager *Manager, label string, revision uint64, op, id, value string) []byte {
	t.Helper()
	keyVersion, key, err := manager.GetOrCreateWorkloadKey(systemMetadataKeyID)
	if err != nil {
		t.Fatal(err)
	}
	var plaintext []byte
	if value != "" {
		plaintext, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	aad := []byte(fmt.Sprintf("%s\n%d\n%s\n%s", label, revision, op, id))
	nonce, ciphertext, err := Encrypt(key, systemMetadataKeyID, "metadata-collection-entry", plaintext, aad)
	if err != nil {
		t.Fatal(err)
	}
	entry := collectionJournalEntry{
		FormatVersion: 1, KeyVersion: keyVersion, Revision: revision,
		Op: op, ID: id,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

// TestJournalTornTailIsCutAwayBeforeTheNextAppend: a crash can leave a torn
// trailing fragment in the journal. It must be cut away at open — the next
// append would otherwise merge with it into one unparseable line and lose
// the new mutation on every later replay.
func TestJournalTornTailIsCutAwayBeforeTheNextAppend(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("first", "one"); err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("second", "two"); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(path+".journal", os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString(`{"format_version":1,"truncated`); err != nil {
		t.Fatal(err)
	}
	_ = journal.Close()
	reopened, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatalf("a torn journal tail must not fail the open: %v", err)
	}
	if err := reopened.Put("third", "three"); err != nil {
		t.Fatal(err)
	}
	final, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"first": "one", "second": "two", "third": "three"} {
		got, err := final.Get(id)
		if err != nil || got != want {
			t.Fatalf("record %s = %q %v, want %q (a torn tail must not swallow later entries)", id, got, err, want)
		}
	}
}

// TestJournalUnparseableEntryStopsReplayButNotTheOpen: a merged torn line
// mid-journal is dropped along with everything after it, but the collection
// still opens on the state it can prove — and the next mutation folds the
// damaged journal away instead of appending behind the damage.
func TestJournalUnparseableEntryStopsReplayButNotTheOpen(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("first", "one"); err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("second", "two"); err != nil {
		t.Fatal(err)
	}
	merged := append([]byte(`{"torn`), fabricateJournalEntry(t, manager, "test-records", 3, "put", "second", "two")...)
	if err := os.WriteFile(path+".journal", merged, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatalf("an unparseable journal entry must not fail the open: %v", err)
	}
	if value, err := reopened.Get("first"); err != nil || value != "one" {
		t.Fatalf("snapshot state must survive: %q %v", value, err)
	}
	if _, err := reopened.Get("second"); err == nil {
		t.Fatal("entries behind the damaged line were not dropped")
	}
	if err := reopened.Put("third", "three"); err != nil {
		t.Fatalf("mutation after damage must heal the journal: %v", err)
	}
	if _, err := os.Stat(path + ".journal"); !os.IsNotExist(err) {
		t.Fatal("the damaged journal must be folded away")
	}
	final, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := final.Get("third"); err != nil || value != "three" {
		t.Fatalf("the healing mutation must be durable: %q %v", value, err)
	}
}

// TestFailedAppendRollsBackCleanlyAndNeverReusesRevisions: an append that
// fails mid-write must leave neither a torn line nor a consumed revision —
// a reused revision would make replay skip the real entry that follows it.
// The write is forced to fail on /dev/full, whose truncation also fails, so
// both the truncate-back and the burn-the-revision paths are exercised.
func TestFailedAppendRollsBackCleanlyAndNeverReusesRevisions(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("first", "one"); err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("second", "two"); err != nil {
		t.Fatal(err)
	}
	journalPath := path + ".journal"
	healthy, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/full", journalPath); err != nil {
		t.Skipf("cannot force a journal write failure here: %v", err)
	}
	if err := collection.Put("doomed", "value"); err == nil {
		t.Fatal("a journal write that fails must fail the mutation")
	}
	if value, err := collection.Get("doomed"); err == nil {
		t.Fatalf("the failed mutation must be rolled back in memory, got %q", value)
	}
	if collection.Revision() < 3 {
		t.Fatalf("a failed append must not release its revision for reuse, got %d", collection.Revision())
	}
	if err := os.Remove(journalPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, healthy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("after", "kept"); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	revisions := make([]uint64, 0)
	ids := make([]string, 0)
	for _, line := range bytes.Split(encoded, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry collectionJournalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("journal holds a torn line after a failed append: %v", err)
		}
		revisions = append(revisions, entry.Revision)
		ids = append(ids, entry.ID)
	}
	for index := 1; index < len(revisions); index++ {
		if revisions[index] <= revisions[index-1] {
			t.Fatalf("journal revisions must be unique and increasing, got %v", revisions)
		}
	}
	if strings.Contains(strings.Join(ids, ","), "doomed") {
		t.Fatalf("a rolled-back entry must not stay in the journal, got %v", ids)
	}
	reopened, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"first": "one", "second": "two", "after": "kept"} {
		if value, err := reopened.Get(id); err != nil || value != want {
			t.Fatalf("record %s = %q %v, want %q", id, value, err, want)
		}
	}
	if value, err := reopened.Get("doomed"); err == nil {
		t.Fatalf("the failed mutation must not be resurrected, got %q", value)
	}
}

// TestReplayAppliesEntriesSharingARevisionInOrder: journals written before
// revision uniqueness could be violated may contain two entries with the
// same revision — a rolled-back one and the real one. Replay must apply them
// in order rather than letting the first consume the revision and skip the
// second, which would silently drop the real mutation.
func TestReplayAppliesEntriesSharingARevisionInOrder(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	journal := append(
		fabricateJournalEntry(t, manager, "test-records", 5, "put", "rolled-back", "ghost"),
		fabricateJournalEntry(t, manager, "test-records", 5, "put", "real", "kept")...,
	)
	if err := os.WriteFile(path+".journal", journal, 0o600); err != nil {
		t.Fatal(err)
	}
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := collection.Get("real"); err != nil || value != "kept" {
		t.Fatalf("the real entry must not be skipped: %q %v", value, err)
	}
}

// TestOpenEncryptedCollectionExistingRefusesAMissingCollection: a missing
// collection file must be an error, not an empty collection — a verifier
// pointed at a wiped tree must see the damage.
func TestOpenEncryptedCollectionExistingRefusesAMissingCollection(t *testing.T) {
	root := t.TempDir()
	manager, err := Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	path := root + "/records.enc.json"
	if _, err := OpenEncryptedCollectionExisting[string](path, "test-records", manager); err == nil {
		t.Fatal("a missing collection must be refused")
	}
	collection, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	if err := collection.Put("id", "value"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenEncryptedCollectionExisting[string](path, "test-records", manager)
	if err != nil {
		t.Fatalf("an existing collection must open: %v", err)
	}
	if value, err := reopened.Get("id"); err != nil || value != "value" {
		t.Fatalf("existing state must stay readable: %q %v", value, err)
	}
}
