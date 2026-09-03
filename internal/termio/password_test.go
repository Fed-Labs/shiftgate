package termio

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadPasswordReadsPipedInput proves the non-terminal path: a script
// piping the secret gets a plain line read with the newline stripped.
func TestReadPasswordReadsPipedInput(t *testing.T) {
	directory := t.TempDir()
	pipePath := filepath.Join(directory, "stdin")
	if err := os.WriteFile(pipePath, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(pipePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	value, err := ReadPassword(file)
	if err != nil {
		t.Fatal(err)
	}
	if value != "hunter2" {
		t.Fatalf("password read as %q", value)
	}
}

// TestReadPasswordHandlesEOFWithoutNewline proves a piped secret without a
// trailing newline still arrives whole.
func TestReadPasswordHandlesEOFWithoutNewline(t *testing.T) {
	directory := t.TempDir()
	pipePath := filepath.Join(directory, "stdin")
	if err := os.WriteFile(pipePath, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(pipePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	value, err := ReadPassword(file)
	if err != nil {
		t.Fatal(err)
	}
	if value != "hunter2" {
		t.Fatalf("password read as %q", value)
	}
}
