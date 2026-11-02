package relay

import (
	"context"
	"crypto/tls"
	"io"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// relayTestServer builds a relay with its full authorization surface: a
// durable leg store, a bound audience, and (when certified is false) the
// explicit test-only opt-out from certificate-verified machine identity.
func relayTestServer(t *testing.T, signer Signer, certified bool) *Server {
	t.Helper()
	store, err := OpenFileLegStore(filepath.Join(t.TempDir(), "legs.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(signer, 4)
	server.Store = store
	server.Audience = "https://relay.test"
	server.RequireOuterIdentity = certified
	return server
}

func relayTestTicket(t *testing.T, signer Signer, claims TicketClaims) string {
	t.Helper()
	claims.Audience = "https://relay.test"
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestTLSRelayPairsOpaqueOutboundLegs(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := relayTestTicket(t, signer, claims)
	server := relayTestServer(t, signer, false)
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.EnableHTTP2 = false
	httpServer.StartTLS()
	defer httpServer.Close()
	defer server.Close()
	address := "https://" + httpServer.Listener.Addr().String()
	configuration := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true} // test relay certificate only
	var source, destination io.ReadWriteCloser
	var sourceErr, destinationErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		source, sourceErr = DialTLS(context.Background(), address, "", configuration, ticket, "source")
	}()
	go func() {
		defer wait.Done()
		destination, destinationErr = DialTLS(context.Background(), address, "", configuration, ticket, "destination")
	}()
	wait.Wait()
	if sourceErr != nil || destinationErr != nil {
		t.Fatalf("relay legs: source=%v destination=%v", sourceErr, destinationErr)
	}
	defer source.Close()
	defer destination.Close()
	message := []byte("inner peer TLS bytes remain opaque to the relay")
	go func() { _, _ = source.Write(message) }()
	read := make([]byte, len(message))
	if _, err := io.ReadFull(destination, read); err != nil {
		t.Fatal(err)
	}
	if string(read) != string(message) {
		t.Fatalf("read %q, want %q", read, message)
	}
}

func TestRelayRefusesConnectWithoutAnAuthorizationSurface(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := relayTestTicket(t, signer, claims)

	// The zero-value server is insecure and must refuse every leg.
	server := NewServer(signer, 4)
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.EnableHTTP2 = false
	httpServer.StartTLS()
	defer httpServer.Close()
	defer server.Close()
	address := "https://" + httpServer.Listener.Addr().String()
	configuration := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	if _, err := DialTLS(context.Background(), address, "", configuration, ticket, "source"); err == nil {
		t.Fatal("a relay without a leg store and audience must refuse every leg")
	}
}

func TestRelayRefusesATicketForAnotherAudience(t *testing.T) {
	signer := Signer{Key: []byte("01234567890123456789012345678901")}
	claims, err := NewClaims("org", "source", "destination", "migration", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims.Audience = "https://other-relay.test"
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	server := relayTestServer(t, signer, false)
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.EnableHTTP2 = false
	httpServer.StartTLS()
	defer httpServer.Close()
	defer server.Close()
	address := "https://" + httpServer.Listener.Addr().String()
	configuration := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	if _, err := DialTLS(context.Background(), address, "", configuration, token, "source"); err == nil {
		t.Fatal("a ticket minted for another relay must be refused here")
	}
}
