package update

import (
	"errors"
	"testing"
	"time"
)

func TestSignedReleaseVerifiesAgainstItsKey(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)
	release := releaseFor("1.1.0", binaryArtifact(ComponentAgent, "https://releases.example/agent", versionScript("shift-agent", "1.1.0")))
	signed, err := signer.SignRelease(release)
	if err != nil {
		t.Fatal(err)
	}
	signedBy, err := ring.Verify(signed)
	if err != nil {
		t.Fatalf("a release signed by a trusted key must verify: %v", err)
	}
	if len(signedBy) != 1 || signedBy[0] != signer.KeyID() {
		t.Fatalf("expected the signing key %s, got %v", signer.KeyID(), signedBy)
	}
}

func TestTamperedReleaseFailsVerification(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)
	payload := versionScript("shift-agent", "1.1.0")
	signed, err := signer.SignRelease(releaseFor("1.1.0", binaryArtifact(ComponentAgent, "https://releases.example/agent", payload)))
	if err != nil {
		t.Fatal(err)
	}
	// Repointing the artifact at another digest is exactly the attack the
	// signature exists to stop.
	tampered := signed
	tampered.Release.Artifacts = append([]Artifact(nil), signed.Release.Artifacts...)
	tampered.Release.Artifacts[0].BinarySHA256 = digestOf(brokenScript())
	if _, err := ring.Verify(tampered); !errors.Is(err, ErrUntrustedRelease) {
		t.Fatalf("expected %v, got %v", ErrUntrustedRelease, err)
	}

	renamed := signed
	renamed.Release.Version = "9.9.9"
	if _, err := ring.Verify(renamed); !errors.Is(err, ErrUntrustedRelease) {
		t.Fatalf("a renamed release must not verify, got %v", err)
	}
}

func TestSignatureFromUnknownKeyIsIgnored(t *testing.T) {
	trusted := newSigner(t)
	attacker := newSigner(t)
	ring := keyRingFor(t, 1, trusted)
	signed, err := attacker.SignRelease(releaseFor("1.2.0", binaryArtifact(ComponentAgent, "https://releases.example/agent", versionScript("shift-agent", "1.2.0"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Verify(signed); !errors.Is(err, ErrUntrustedRelease) {
		t.Fatalf("a release signed by an untrusted key must be refused, got %v", err)
	}
}

func TestThresholdRequiresTwoDistinctSigners(t *testing.T) {
	first := newSigner(t)
	second := newSigner(t)
	ring := keyRingFor(t, 2, first, second)
	release := releaseFor("2.0.0", binaryArtifact(ComponentAgent, "https://releases.example/agent", versionScript("shift-agent", "2.0.0")))
	single, err := first.SignRelease(release)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Verify(single); !errors.Is(err, ErrUntrustedRelease) {
		t.Fatalf("one signature must not satisfy a threshold of two, got %v", err)
	}
	// The same key signing twice is still one signer.
	duplicated := single
	duplicated.Signatures = append(duplicated.Signatures, single.Signatures[0])
	if _, err := ring.Verify(duplicated); !errors.Is(err, ErrUntrustedRelease) {
		t.Fatalf("a repeated signature must not count twice, got %v", err)
	}
	secondSignature, err := second.Sign(release)
	if err != nil {
		t.Fatal(err)
	}
	both := single
	both.Signatures = append(append([]Signature(nil), single.Signatures...), secondSignature)
	signedBy, err := ring.Verify(both)
	if err != nil {
		t.Fatalf("two distinct signatures must satisfy the threshold: %v", err)
	}
	if len(signedBy) != 2 {
		t.Fatalf("expected two verified keys, got %v", signedBy)
	}
}

func TestRelabelledAlgorithmIsRefused(t *testing.T) {
	signer := newSigner(t)
	ring := keyRingFor(t, 1, signer)
	signed, err := signer.SignRelease(releaseFor("1.3.0", binaryArtifact(ComponentAgent, "https://releases.example/agent", versionScript("shift-agent", "1.3.0"))))
	if err != nil {
		t.Fatal(err)
	}
	signed.Signatures[0].Algorithm = "hmac-sha256"
	if _, err := ring.Verify(signed); !errors.Is(err, ErrUntrustedRelease) {
		t.Fatalf("an unknown algorithm label must be refused, got %v", err)
	}
}

func TestRevokedAndExpiredKeysAreNotUsable(t *testing.T) {
	signer := newSigner(t)
	revoked := trustedKeyFor(t, signer)
	revoked.Revoked = true
	if _, err := NewKeyRing([]TrustedKey{revoked}, 1, testNow); err == nil {
		t.Fatal("a ring whose only key is revoked must not load")
	}
	expiry := testNow.Add(-time.Hour)
	expired := trustedKeyFor(t, signer)
	expired.ExpiresAt = &expiry
	if _, err := NewKeyRing([]TrustedKey{expired}, 1, testNow); err == nil {
		t.Fatal("a ring whose only key has expired must not load")
	}
	future := testNow.Add(time.Hour)
	valid := trustedKeyFor(t, signer)
	valid.ExpiresAt = &future
	if _, err := NewKeyRing([]TrustedKey{valid}, 1, testNow); err != nil {
		t.Fatalf("a key that has not yet expired must load: %v", err)
	}
}

func TestKeyIdentifierMustMatchItsMaterial(t *testing.T) {
	signer := newSigner(t)
	key := trustedKeyFor(t, signer)
	key.ID = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := NewKeyRing([]TrustedKey{key}, 1, testNow); err == nil {
		t.Fatal("a key whose recorded identifier does not match its material must be refused")
	}
	derived := trustedKeyFor(t, signer)
	derived.ID = ""
	ring, err := NewKeyRing([]TrustedKey{derived}, 1, testNow)
	if err != nil {
		t.Fatalf("an identifier may be derived from the key material: %v", err)
	}
	if identifiers := ring.KeyIDs(); len(identifiers) != 1 || identifiers[0] != signer.KeyID() {
		t.Fatalf("expected the derived identifier %s, got %v", signer.KeyID(), identifiers)
	}
}

func TestKeyRingRefusesUnreachableThreshold(t *testing.T) {
	signer := newSigner(t)
	if _, err := NewKeyRing([]TrustedKey{trustedKeyFor(t, signer)}, 2, testNow); err == nil {
		t.Fatal("a threshold larger than the number of keys must be refused")
	}
	if _, err := NewKeyRing([]TrustedKey{trustedKeyFor(t, signer)}, 0, testNow); err == nil {
		t.Fatal("a threshold below one must be refused")
	}
}

func TestDuplicateTrustedKeyIsRefused(t *testing.T) {
	signer := newSigner(t)
	key := trustedKeyFor(t, signer)
	if _, err := NewKeyRing([]TrustedKey{key, key}, 1, testNow); err == nil {
		t.Fatal("the same key listed twice must be refused")
	}
}

func TestCanonicalBytesAreStableAndDomainSeparated(t *testing.T) {
	release := releaseFor("1.4.0", binaryArtifact(ComponentAgent, "https://releases.example/agent", versionScript("shift-agent", "1.4.0")))
	if err := release.Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalBytes(release)
	if err != nil {
		t.Fatal(err)
	}
	// Whitespace and a leading "v" are normalized away, so the same release
	// spelled differently signs identically.
	loose := release
	loose.Version = " v1.4.0 "
	second, err := CanonicalBytes(loose)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("normalization must produce one canonical form")
	}
	if got := string(first[:len(canonicalPrefix)]); got != canonicalPrefix {
		t.Fatalf("canonical bytes must be domain-separated, got prefix %q", got)
	}
}
