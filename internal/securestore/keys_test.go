package securestore

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadKeyRoundTripAndImport(t *testing.T) {
	source, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	version, key, err := source.GetOrCreateWorkloadKey("workload-1")
	if err != nil {
		t.Fatal(err)
	}
	destination, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.ImportWorkloadKey("workload-1", version, key); err != nil {
		t.Fatal(err)
	}
	restored, err := destination.WorkloadKey("workload-1", version)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, restored) {
		t.Fatal("imported workload key changed")
	}
}

func TestSealRejectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	nonce, ciphertext, err := Encrypt(key, "w", "test", []byte("state"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[0] ^= 0xff
	if _, err := Decrypt(key, "w", "test", nonce, ciphertext, []byte("aad")); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestRotateMasterKeyRewrapsAndRetires(t *testing.T) {
	manager, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	versionA, keyA, err := manager.GetOrCreateWorkloadKey("workload-1")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := manager.RotateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if rotated <= versionA {
		t.Fatalf("rotation must produce a new master version, got %d", rotated)
	}
	restoredA, err := manager.WorkloadKey("workload-1", versionA)
	if err != nil || !bytes.Equal(restoredA, keyA) {
		t.Fatalf("workload keys must survive rotation: %v", err)
	}
	versionB, _, err := manager.RotateWorkloadKey("workload-1")
	if err != nil {
		t.Fatal(err)
	}
	if versionB <= versionA {
		t.Fatal("workload key rotation must keep versioning")
	}
	if _, err := manager.WorkloadKey("workload-1", versionB); err != nil {
		t.Fatalf("the rotated workload key must resolve under the new master: %v", err)
	}
	reopened, err := Open(manager.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.WorkloadKey("workload-1", versionA); err != nil {
		t.Fatalf("rotated keyring must still resolve workload keys: %v", err)
	}
	for _, record := range reopened.keyring.Keys {
		if record.Version != reopened.keyring.ActiveVersion && record.RetiredAt == nil {
			t.Fatalf("superseded master key %d must be retired", record.Version)
		}
	}
}

func TestKeyfileProtectsMasterKeysAtRest(t *testing.T) {
	root := t.TempDir()
	keyFile := filepath.Join(t.TempDir(), "master.keyfile")
	if err := os.WriteFile(keyFile, bytes.Repeat([]byte{9}, 64), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := OpenWithKeyFile(root, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	version, key, err := manager.GetOrCreateWorkloadKey("workload-1")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(root, "master-keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "keyfile-v1") {
		t.Fatal("the master keyring must record its keyfile protection")
	}
	reopened, err := OpenWithKeyFile(root, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := reopened.WorkloadKey("workload-1", version)
	if err != nil || !bytes.Equal(restored, key) {
		t.Fatalf("keyfile-protected keyring must resolve workload keys: %v", err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("a sealed keyring must refuse to open without its keyfile")
	}
}

// TestOpenExistingRefusesToMintAKeyring: a missing master keyring must fail
// loudly. Minting one over a damaged tree makes the tree look complete — the
// failure mode that let corrupted backups verify green.
func TestOpenExistingRefusesToMintAKeyring(t *testing.T) {
	root := t.TempDir()
	if _, err := OpenExisting(root + "/keys"); err == nil {
		t.Fatal("a missing keyring must be refused, not minted")
	}
	if _, err := os.Stat(filepath.Join(root, "keys", "master-keyring.json")); !os.IsNotExist(err) {
		t.Fatal("OpenExisting must not create a keyring")
	}
	if _, err := Open(root + "/keys"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(root + "/keys")
	if err != nil {
		t.Fatalf("an existing keyring must open: %v", err)
	}
	if version, _, err := reopened.GetOrCreateWorkloadKey("w1"); err != nil || version == 0 {
		t.Fatalf("existing key material must stay usable: %d %v", version, err)
	}
}
