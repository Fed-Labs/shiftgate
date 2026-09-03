package securestore

import (
	"bytes"
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
