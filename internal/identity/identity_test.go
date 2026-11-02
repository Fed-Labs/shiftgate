package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureMintsIdentityOnce(t *testing.T) {
	directory := t.TempDir()
	first, err := Ensure(directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(directory)
	if err != nil {
		t.Fatal(err)
	}
	if first.Machine.ID != second.Machine.ID {
		t.Fatalf("identity churned across calls: %s -> %s", first.Machine.ID, second.Machine.ID)
	}
	if id, err := PublicKeyID(first.Machine.PublicKeyPEM); err != nil || id != first.Machine.ID {
		t.Fatalf("PublicKeyID(%v) = %q, %v; want %s", first.Machine.PublicKeyPEM, id, err, first.Machine.ID)
	}
}

// TestEnsureDerivesPublicKeyFromSurvivingPrivateKey is the partial-restore
// guard: losing only identity-public.pem must not change the machine id.
func TestEnsureDerivesPublicKeyFromSurvivingPrivateKey(t *testing.T) {
	directory := t.TempDir()
	original, err := Ensure(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, "identity-public.pem")); err != nil {
		t.Fatal(err)
	}
	recovered, err := Ensure(directory)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Machine.ID != original.Machine.ID {
		t.Fatalf("machine id changed after losing the public half: %s -> %s", original.Machine.ID, recovered.Machine.ID)
	}
	signature := recovered.Sign([]byte("message"))
	if !recovered.Verify([]byte("message"), signature) {
		t.Fatal("recovered identity cannot sign with its own key")
	}
	if _, err := os.Stat(filepath.Join(directory, "identity-public.pem")); err != nil {
		t.Fatalf("public key was not rewritten: %v", err)
	}
}

// TestEnsureRefusesToMintOverLostPrivateKey: a public key without its private
// half still names a machine; minting a new pair would silently re-register
// the host under a new id.
func TestEnsureRefusesToMintOverLostPrivateKey(t *testing.T) {
	directory := t.TempDir()
	if _, err := Ensure(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, "identity-key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(directory); err == nil {
		t.Fatal("a new identity was minted over an existing machine id")
	}
}

func TestEnsureConcurrentCallsAgree(t *testing.T) {
	directory := t.TempDir()
	const workers = 8
	ids := make([]string, workers)
	errors := make([]error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	start := make(chan struct{})
	var done sync.WaitGroup
	done.Add(workers)
	for index := 0; index < workers; index++ {
		go func(index int) {
			defer done.Done()
			ready.Done()
			<-start
			identity, err := Ensure(directory)
			errors[index] = err
			if err == nil {
				ids[index] = identity.Machine.ID
			}
		}(index)
	}
	ready.Wait()
	close(start)
	done.Wait()
	for index := 0; index < workers; index++ {
		if errors[index] != nil {
			t.Fatalf("concurrent Ensure failed: %v", errors[index])
		}
		if ids[index] != ids[0] {
			t.Fatalf("concurrent Ensure produced mismatched identities: %s vs %s", ids[index], ids[0])
		}
	}
}

// TestPublicKeyIDIsAFunctionOfKeyMaterial: the machine id derives from the
// canonical key encoding, so the same key can never claim two identities and
// a re-encoded PEM cannot claim a different one.
func TestPublicKeyIDIsAFunctionOfKeyMaterial(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	canonical := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	rewrapped := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: append([]byte(nil), der...)}))
	first, err := PublicKeyID(canonical)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PublicKeyID(rewrapped)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same key produced two ids: %s vs %s", first, second)
	}
}
