package objectstore

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// hardening_test.go: the report 16 signed-credential and key-injection
// defenses — the STS answer must never extend its own lease or hand out an
// already-dead credential (clock skew tolerated only within
// stsExpirationSkew), and object keys never carry control characters that
// could corrupt request signing or log lines.

// TestObjectstoreRejectsExpiredSTSCredentials: a credential that expired
// (beyond the clock-skew allowance) is refused outright — handing it to an
// agent would fail mid-restore instead of at issuance.
func TestObjectstoreRejectsExpiredSTSCredentials(t *testing.T) {
	server := &fakeSTS{expiration: time.Now().UTC().Add(-time.Hour)}
	httptestServer := newTestSTS(t, server)
	_, err := AssumeRole(context.Background(), httptestServer.URL, "test-1", "parent", "parent-secret", `{"Version":"2012-10-17"}`, time.Hour, httptestServer.Client())
	if err == nil || !strings.Contains(err.Error(), "already-expired") {
		t.Fatalf("expired credentials were not refused: %v", err)
	}
}

// TestObjectstoreRejectsOverlongSTSCredentials: the expiration the service
// returns bounds the credential's real lifetime, so an answer outliving the
// requested session is refused — a signed credential must never be longer
// than the lease that was asked for.
func TestObjectstoreRejectsOverlongSTSCredentials(t *testing.T) {
	server := &fakeSTS{expiration: time.Now().UTC().Add(48 * time.Hour)}
	httptestServer := newTestSTS(t, server)
	_, err := AssumeRole(context.Background(), httptestServer.URL, "test-1", "parent", "parent-secret", `{"Version":"2012-10-17"}`, time.Hour, httptestServer.Client())
	if err == nil || !strings.Contains(err.Error(), "outliving") {
		t.Fatalf("over-long credentials were not refused: %v", err)
	}
}

// TestObjectstoreAcceptsSkewedSTSExpiration: clocks drift, so an answer a couple
// of minutes off the requested lifetime is accepted — the skew allowance is
// a tolerance, not a hole.
func TestObjectstoreAcceptsSkewedSTSExpiration(t *testing.T) {
	for _, offset := range []time.Duration{-2 * time.Minute, 2 * time.Minute} {
		server := &fakeSTS{expiration: time.Now().UTC().Add(time.Hour + offset)}
		httptestServer := newTestSTS(t, server)
		credentials, err := AssumeRole(context.Background(), httptestServer.URL, "test-1", "parent", "parent-secret", `{"Version":"2012-10-17"}`, time.Hour, httptestServer.Client())
		if err != nil {
			t.Fatalf("skewed expiration %v refused: %v", offset, err)
		}
		if credentials.AccessKeyID == "" {
			t.Fatalf("skewed expiration %v produced empty credentials", offset)
		}
	}
}

// TestObjectstoreBoundsRequestedSTSTTL: the requested lifetime itself is bounded
// to one minute .. seven days before anything is signed or sent.
func TestObjectstoreBoundsRequestedSTSTTL(t *testing.T) {
	server := &fakeSTS{}
	httptestServer := newTestSTS(t, server)
	for _, ttl := range []time.Duration{0, time.Second, 30 * 24 * time.Hour} {
		if _, err := AssumeRole(context.Background(), httptestServer.URL, "test-1", "parent", "parent-secret", `{"Version":"2012-10-17"}`, ttl, httptestServer.Client()); err == nil {
			t.Fatalf("TTL %v was accepted", ttl)
		}
	}
}

// TestObjectstoreKeyValidationRejectsControlCharactersAndTraversal: keys are chunk
// addresses and manifest names — control bytes (which could corrupt signed
// request canonicalization and logs) and traversal segments are refused, and
// the legitimate shapes pass.
func TestObjectstoreKeyValidationRejectsControlCharactersAndTraversal(t *testing.T) {
	for _, hostile := range []string{
		"", "/leading", "trailing//slash", "a//b", "./x", "x/./y", "a/../b", "..",
		"back\\slash", "nul\x00byte", "new\nline", "cr\rline", "tab\tkey", "esc\x1bkey", "del\x7fkey",
		strings.Repeat("x", 1025),
	} {
		if err := validateKey(hostile); err != ErrInvalidKey {
			t.Fatalf("key %q accepted with err %v", hostile, err)
		}
	}
	for _, good := range []string{"chunks/v1/abc123", "checkpoints/cp_1/manifest.enc.json", "a", "a/b/c/d"} {
		if err := validateKey(good); err != nil {
			t.Fatalf("key %q refused: %v", good, err)
		}
	}
}

// newTestSTS wraps a fake STS handler in an httptest server.
func newTestSTS(t *testing.T, server *fakeSTS) *httptest.Server {
	t.Helper()
	started := httptest.NewServer(server)
	t.Cleanup(started.Close)
	return started
}
