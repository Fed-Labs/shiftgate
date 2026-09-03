package securestore

import (
	"os"
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
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sensitive-command") {
		t.Fatal("encrypted collection exposed plaintext")
	}
	reopened, err := OpenEncryptedCollection[string](path, "test-records", manager)
	if err != nil {
		t.Fatal(err)
	}
	value, err := reopened.Get("id")
	if err != nil || value != "sensitive-command-and-environment" {
		t.Fatalf("unexpected reopened value %q: %v", value, err)
	}
}
