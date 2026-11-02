package persistence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileRoundTripLeavesNoStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "doc.json")
	if err := WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("read back %q, %v", raw, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("staging files left behind: %v", entries)
	}
}

func TestWriteFileReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.json")
	if err := WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("v2-longer"), 0o640); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "v2-longer" {
		t.Fatalf("read back %q, %v", raw, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o, want 640", info.Mode().Perm())
	}
}

// TestWriteFilePreservesPreviousOnFailedCommit: a write that cannot commit
// must leave the previous document whole — the crash-mid-write guarantee is
// that the rename is the only moment anything changes, and a failed write is
// indistinguishable from a crash before it.
func TestWriteFilePreservesPreviousOnFailedCommit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the directory permissions used to fail the write")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "doc.json")
	if err := WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
	if err := WriteFile(path, []byte("v2"), 0o600); err == nil {
		t.Fatal("a write that cannot stage must fail")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "v1" {
		t.Fatalf("previous document must survive a failed write, got %q, %v", raw, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("staging files left behind after a failed write: %v", entries)
	}
}

func TestWriteFileCleansUpWhenCommitFails(t *testing.T) {
	directory := t.TempDir()
	blocked := filepath.Join(directory, "doc.json")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(blocked, []byte("v1"), 0o600); err == nil {
		t.Fatal("committing over a directory must fail")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".shift-write-") {
			t.Fatalf("staging file left behind: %s", entry.Name())
		}
	}
}

func TestReadJSONRejectsPartialAndTrailingInput(t *testing.T) {
	directory := t.TempDir()
	var target map[string]int

	partial := filepath.Join(directory, "partial.json")
	if err := os.WriteFile(partial, []byte(`{"a":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(partial, &target); err == nil {
		t.Fatal("a truncated document (crash mid-write) must not decode")
	}

	trailing := filepath.Join(directory, "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"a":1} {"b":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(trailing, &target); err == nil {
		t.Fatal("trailing JSON data must be refused")
	}

	unknown := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"a":1,"mystery":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(unknown, &target); err == nil {
		t.Fatal("unknown fields must be refused")
	}

	good := filepath.Join(directory, "good.json")
	if err := WriteJSON(good, map[string]int{"a": 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	target = nil
	if err := ReadJSON(good, &target); err != nil || target["a"] != 1 {
		t.Fatalf("round trip: %v %v", target, err)
	}
}

func TestCheckPrivateFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFileMode(path); err != nil {
		t.Fatalf("0600 must pass: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFileMode(path); err == nil {
		t.Fatal("group-readable config must be refused")
	}
	if err := os.Chmod(path, 0o604); err != nil {
		t.Fatal(err)
	}
	if err := CheckPrivateFileMode(path); err == nil {
		t.Fatal("world-readable config must be refused")
	}
}
