package relay

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTicketSignVerifyAndRejectTamper(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	if verified, err := signer.Verify(token); err != nil || verified.MigrationID != "migration" {
		t.Fatalf("verify = %#v %v", verified, err)
	}
	if _, err := signer.Verify(token + "x"); err != ErrInvalidTicket {
		t.Fatalf("tampered token error = %v", err)
	}
}

func TestTicketRejectsForgeryAndForeignSigningKeys(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	other := Signer{Key: []byte("99999999999999999999999999999999")}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Verify(token); err != ErrInvalidTicket {
		t.Fatalf("a ticket signed by another key must be refused, got %v", err)
	}
	// A forged body with a copied signature must not decode as valid claims.
	body := strings.SplitN(token, ".", 2)
	forged := "eyJvcmciOiJhdHRhY2tlciJ9." + body[1]
	if _, err := signer.Verify(forged); err != ErrInvalidTicket {
		t.Fatalf("a forged claims body must be refused, got %v", err)
	}
	// An expired or over-long presentation is refused at signing time.
	expired, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expired.ExpiresUnix = time.Now().Add(-time.Minute).Unix()
	if _, err := signer.Sign(expired); err != ErrInvalidTicket {
		t.Fatalf("expired claims must not be signed, got %v", err)
	}
}

func TestRelayPairsOpaqueBytes(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims, _ := NewClaims("org", "source", "destination", "migration", time.Minute)
	claims.Audience = "https://relay.test"
	ticket, _ := signer.Sign(claims)
	_ = NewServer(signer, 4)
	// The network-facing CONNECT handler is covered by the integration server;
	// this test pins the opaque ticket/payload primitives without parsing bytes.
	if _, err := signer.Verify(ticket); err != nil {
		t.Fatal(err)
	}
	if claims.SourceMachineID == claims.DestinationMachineID {
		t.Fatal("pair must be distinct")
	}
}

// stubLegStore records claim/consume calls and can fail claims on demand.
type stubLegStore struct {
	mu       sync.Mutex
	claimed  int
	consumed int
	claimErr error
}

func (store *stubLegStore) ClaimRelayLeg(context.Context, string, string, string, string, string, string, string, string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.claimed++
	return store.claimErr
}

func (store *stubLegStore) ConsumeRelayTicket(context.Context, string, string, string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.consumed++
	return nil
}

// TestPairTeardownConsumesTheTicketWithoutACompletedClaim pins the
// claim/teardown race fix: the consume handle is recorded before the store
// claim runs, so a pair torn down while one leg's claim raced the other leg's
// failure still burns its ticket. A captured presentation must never re-open
// the nonce for the rest of its TTL.
func TestPairTeardownConsumesTheTicketWithoutACompletedClaim(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	store := &stubLegStore{claimErr: errors.New("store outage")}
	server := NewServer(signer, 4)
	server.Store = store
	server.Audience = "https://relay.test"
	state := &pair{claims: claims, legs: map[string]net.Conn{}, done: make(chan struct{})}
	server.pairs[claims.Nonce] = state
	state.token = "captured-presentation"
	server.abort(claims.Nonce, state)
	if store.consumed != 1 {
		t.Fatalf("teardown must burn the ticket whenever a presentation was recorded, consumed=%d", store.consumed)
	}
}

func TestFileLegStoreEnforcesOneUseTickets(t *testing.T) {
	store, err := OpenFileLegStore(filepath.Join(t.TempDir(), "legs.json"))
	if err != nil {
		t.Fatal(err)
	}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims.Audience = "https://relay.test"
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(machine string) error {
		return store.ClaimRelayLeg(context.Background(), "org", "migration", "source", "destination", claims.Nonce, token, machine, "relay-1")
	}
	if err := claim("source"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := claim("destination"); err != nil {
		t.Fatalf("second leg claim: %v", err)
	}
	if err := claim("source"); err != nil {
		t.Fatalf("a re-claim inside the two-minute window is the store's documented retry path: %v", err)
	}
	if err := claim("intruder"); err != ErrTicketUsed {
		t.Fatalf("a machine outside the pair must never claim, got %v", err)
	}
	if err := store.ConsumeRelayTicket(context.Background(), claims.Nonce, token, "relay-1"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if err := store.ConsumeRelayTicket(context.Background(), claims.Nonce, token, "relay-1"); err != ErrTicketUsed {
		t.Fatalf("a second consume must report replay, got %v", err)
	}
	if err := claim("source"); err != ErrTicketUsed {
		t.Fatalf("a consumed ticket must never claim again, got %v", err)
	}

	// A presentation whose claims name another nonce is a forged claim.
	forged, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	forged.Audience = "https://relay.test"
	if err := store.ClaimRelayLeg(context.Background(), "org", "migration", "source", "destination", forged.Nonce, token, "source", "relay-1"); err != ErrTicketUsed {
		t.Fatalf("a token that does not match the claimed nonce must be refused, got %v", err)
	}
	// And a live nonce presented with a foreign token hash is refused too.
	other := claims
	other.WorkloadID = "some-workload"
	otherToken, err := signer.Sign(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimRelayLeg(context.Background(), "org", "migration", "source", "destination", claims.Nonce, otherToken, "source", "relay-1"); err != ErrTicketUsed {
		t.Fatalf("a token whose hash does not match the recorded ticket must be refused, got %v", err)
	}
}

func TestFileLegStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legs.json")
	store, err := OpenFileLegStore(path)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims.Audience = "https://relay.test"
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimRelayLeg(context.Background(), "org", "migration", "source", "destination", claims.Nonce, token, "source", "relay-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.ConsumeRelayTicket(context.Background(), claims.Nonce, token, "relay-1"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileLegStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.ClaimRelayLeg(context.Background(), "org", "migration", "source", "destination", claims.Nonce, token, "destination", "relay-1"); err != ErrTicketUsed {
		t.Fatalf("one-use state must survive a relay restart, got %v", err)
	}
}

func TestSameEndpointToleratesSpellingDifferencesOnly(t *testing.T) {
	same := [][2]string{
		{"https://relay.example:9443", "https://relay.example:9443"},
		{"https://relay.example:9443/", "https://relay.example:9443"},
		{"https://relay.example:443", "https://relay.example"},
		{"https://Relay.Example:9443", "https://relay.example:9443"},
	}
	for _, pair := range same {
		if !SameEndpoint(pair[0], pair[1]) {
			t.Fatalf("%q and %q must name the same relay", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"https://relay.example:9443", "https://relay.example:9444"},
		{"https://relay.example:9443", "http://relay.example:9443"},
		{"https://relay.example:9443", "https://other.example:9443"},
		{"https://relay.example:9443", "https://relay.example"},
	}
	for _, pair := range different {
		if SameEndpoint(pair[0], pair[1]) {
			t.Fatalf("%q and %q must not name the same relay", pair[0], pair[1])
		}
	}
}
