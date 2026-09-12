package update

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"shift.dev/shift/internal/identity"
)

// algorithmEd25519 is the only signature algorithm this implementation accepts.
// Refusing unknown algorithm names keeps an attacker from downgrading a release
// to something weaker by relabelling the signature.
const algorithmEd25519 = "ed25519"

// ErrUntrustedRelease is returned when a release does not carry enough valid
// signatures from trusted keys. An update is never applied on a release that
// failed this check.
var ErrUntrustedRelease = errors.New("release is not signed by enough trusted keys")

// TrustedKey is one release-signing key an installer accepts. The identifier is
// derived from the key material itself, so a feed cannot point a known key
// identifier at a different key.
type TrustedKey struct {
	ID           string     `json:"id"`
	PublicKeyPEM string     `json:"public_key_pem"`
	Comment      string     `json:"comment,omitempty"`
	Revoked      bool       `json:"revoked,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// TrustedKeyID derives the identifier of a public key. The same derivation runs
// when a key ring is built, so an identifier printed here is the identifier an
// installer will expect in a signature.
func TrustedKeyID(publicKeyPEM string) (string, error) {
	return identity.PublicKeyID(publicKeyPEM)
}

// KeyRing is the set of keys whose signatures an installer honors, and how many
// of them must agree. A threshold above one lets an operator require two
// signers, so a single compromised signing key cannot ship a release on its own.
type KeyRing struct {
	keys      map[string]TrustedKey
	threshold int
}

// NewKeyRing builds a key ring. Every key's recorded identifier is checked
// against its key material, and revoked or expired keys are refused at load time
// rather than at verification time, so a misconfigured ring fails loudly.
func NewKeyRing(keys []TrustedKey, threshold int, now time.Time) (*KeyRing, error) {
	if threshold < 1 {
		return nil, errors.New("a key ring requires a signature threshold of at least one")
	}
	ring := &KeyRing{keys: make(map[string]TrustedKey, len(keys)), threshold: threshold}
	for index, key := range keys {
		key.ID = strings.ToLower(strings.TrimSpace(key.ID))
		key.PublicKeyPEM = strings.TrimSpace(key.PublicKeyPEM) + "\n"
		derived, err := identity.PublicKeyID(key.PublicKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("trusted key %d: %w", index, err)
		}
		if key.ID == "" {
			key.ID = derived
		}
		if key.ID != derived {
			return nil, fmt.Errorf("trusted key %d records identifier %q but its key material is %q", index, key.ID, derived)
		}
		if key.Revoked {
			continue
		}
		if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
			continue
		}
		if _, duplicate := ring.keys[key.ID]; duplicate {
			return nil, fmt.Errorf("trusted key %s is listed twice", key.ID)
		}
		ring.keys[key.ID] = key
	}
	if len(ring.keys) < threshold {
		return nil, fmt.Errorf("key ring holds %d usable keys but requires %d signatures", len(ring.keys), threshold)
	}
	return ring, nil
}

// Threshold is how many distinct trusted signatures a release needs.
func (ring *KeyRing) Threshold() int {
	return ring.threshold
}

// KeyIDs lists the identifiers of the usable keys, for diagnostics. It never
// exposes key material beyond the public identifiers a feed already carries.
func (ring *KeyRing) KeyIDs() []string {
	identifiers := make([]string, 0, len(ring.keys))
	for id := range ring.keys {
		identifiers = append(identifiers, id)
	}
	return identifiers
}

// Verify checks a signed release against the ring and returns the identifiers of
// the keys that signed it. Signatures from keys the ring does not hold are
// ignored rather than trusted, and a release only passes when at least the
// threshold number of distinct trusted keys verified over its canonical bytes.
func (ring *KeyRing) Verify(signed SignedRelease) ([]string, error) {
	message, err := CanonicalBytes(signed.Release)
	if err != nil {
		return nil, err
	}
	verified := make([]string, 0, len(signed.Signatures))
	seen := make(map[string]bool, len(signed.Signatures))
	for _, signature := range signed.Signatures {
		keyID := strings.ToLower(strings.TrimSpace(signature.KeyID))
		if seen[keyID] {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(signature.Algorithm), algorithmEd25519) {
			continue
		}
		key, trusted := ring.keys[keyID]
		if !trusted {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signature.Value))
		if err != nil || len(raw) != ed25519.SignatureSize {
			continue
		}
		if err := identity.Verify(key.PublicKeyPEM, message, raw); err != nil {
			continue
		}
		seen[keyID] = true
		verified = append(verified, keyID)
	}
	if len(verified) < ring.threshold {
		return verified, fmt.Errorf("%w: %d of %d required signatures verified",
			ErrUntrustedRelease, len(verified), ring.threshold)
	}
	return verified, nil
}

// Signer produces release signatures. It is the publishing half of the same
// scheme the installer verifies, so a release can be signed and checked without
// two implementations of the canonical form drifting apart.
type Signer struct {
	keyID   string
	private ed25519.PrivateKey
}

// NewSigner loads a PKCS#8 Ed25519 private key. The key never leaves this
// process and is never logged.
func NewSigner(privateKeyPEM string) (*Signer, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("invalid signing key PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("signing key is not Ed25519")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(private.Public())
	if err != nil {
		return nil, fmt.Errorf("marshal signing public key: %w", err)
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	keyID, err := identity.PublicKeyID(string(publicPEM))
	if err != nil {
		return nil, err
	}
	return &Signer{keyID: keyID, private: private}, nil
}

// KeyID is the identifier a verifier will match this signer's signatures by.
func (signer *Signer) KeyID() string {
	return signer.keyID
}

// PublicKeyPEM is the key material to publish in a trusted key ring.
func (signer *Signer) PublicKeyPEM() (string, error) {
	publicDER, err := x509.MarshalPKIXPublicKey(signer.private.Public())
	if err != nil {
		return "", fmt.Errorf("marshal signing public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})), nil
}

// Sign signs a release. The release is validated first: signing a malformed
// release would produce a document no installer can use.
func (signer *Signer) Sign(release Release) (Signature, error) {
	if err := release.Validate(); err != nil {
		return Signature{}, err
	}
	message, err := CanonicalBytes(release)
	if err != nil {
		return Signature{}, err
	}
	return Signature{
		KeyID:     signer.keyID,
		Algorithm: algorithmEd25519,
		Value:     base64.StdEncoding.EncodeToString(ed25519.Sign(signer.private, message)),
	}, nil
}

// SignRelease validates, signs, and returns the release ready for publication.
func (signer *Signer) SignRelease(release Release) (SignedRelease, error) {
	if err := release.Validate(); err != nil {
		return SignedRelease{}, err
	}
	signature, err := signer.Sign(release)
	if err != nil {
		return SignedRelease{}, err
	}
	return SignedRelease{Release: release, Signatures: []Signature{signature}}, nil
}
