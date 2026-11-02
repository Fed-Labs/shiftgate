// Package relay carries an end-to-end peer TLS stream inside two outbound TLS
// CONNECT connections. It never terminates peer TLS or proxies the local API.
package relay

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shift.dev/shift/internal/endpoint"
)

var (
	ErrInvalidTicket = errors.New("invalid or expired relay ticket")
	ErrTicketUsed    = errors.New("relay ticket has already been consumed")
	ErrPairMismatch  = errors.New("relay ticket machine pair mismatch")
	ErrCapacity      = errors.New("relay capacity exhausted")
)

type TicketClaims struct {
	OrganizationID       string `json:"org"`
	SourceMachineID      string `json:"src"`
	DestinationMachineID string `json:"dst"`
	MigrationID          string `json:"migration"`
	Nonce                string `json:"nonce"`
	ExpiresUnix          int64  `json:"exp"`
	WorkloadID           string `json:"workload,omitempty"`
	Audience             string `json:"audience,omitempty"`
}

type Signer struct{ Key []byte }

func validClaims(c TicketClaims) bool {
	return c.OrganizationID != "" && c.SourceMachineID != "" && c.DestinationMachineID != "" && c.SourceMachineID != c.DestinationMachineID && c.MigrationID != "" && c.Nonce != "" && c.ExpiresUnix > time.Now().Unix() && c.ExpiresUnix <= time.Now().Add(10*time.Minute).Unix()
}
func (s Signer) Sign(c TicketClaims) (string, error) {
	if len(s.Key) < 32 || !validClaims(c) {
		return "", ErrInvalidTicket
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, s.Key)
	_, _ = m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}
func Decode(token string) (TicketClaims, error) {
	if len(token) > 4096 {
		return TicketClaims{}, ErrInvalidTicket
	}
	p := strings.Split(token, ".")
	if len(p) != 2 {
		return TicketClaims{}, ErrInvalidTicket
	}
	b, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return TicketClaims{}, ErrInvalidTicket
	}
	var c TicketClaims
	if json.Unmarshal(b, &c) != nil || !validClaims(c) {
		return TicketClaims{}, ErrInvalidTicket
	}
	return c, nil
}
func (s Signer) Verify(token string) (TicketClaims, error) {
	c, err := Decode(token)
	if err != nil || len(s.Key) < 32 {
		return TicketClaims{}, ErrInvalidTicket
	}
	p := strings.Split(token, ".")
	m := hmac.New(sha256.New, s.Key)
	_, _ = m.Write([]byte(p[0]))
	sig, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil || !hmac.Equal(sig, m.Sum(nil)) {
		return TicketClaims{}, ErrInvalidTicket
	}
	return c, nil
}
func NewClaims(org, source, destination, migration string, ttl time.Duration) (TicketClaims, error) {
	if org == "" || source == "" || destination == "" || migration == "" || source == destination {
		return TicketClaims{}, ErrPairMismatch
	}
	if ttl <= 0 || ttl > 10*time.Minute {
		return TicketClaims{}, ErrInvalidTicket
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return TicketClaims{}, err
	}
	return TicketClaims{OrganizationID: org, SourceMachineID: source, DestinationMachineID: destination, MigrationID: migration, Nonce: base64.RawURLEncoding.EncodeToString(nonce[:]), ExpiresUnix: time.Now().Add(ttl).Unix()}, nil
}

// LegStore atomically claims each certified machine leg and burns the ticket
// when the pair is torn down. Production uses PostgreSQL; there is no
// in-memory/restart-unsafe production fallback. The claim binds the ticket to
// one leg of one migration pair (a replay of a consumed or revoked ticket is
// refused), and the consume makes a ticket one-use for its whole TTL.
type LegStore interface {
	ClaimRelayLeg(context.Context, string, string, string, string, string, string, string, string) error
	ConsumeRelayTicket(ctx context.Context, nonce, token, relayID string) error
}

// TransferMeter records relayed bandwidth for billing. *database.Store
// implements it (RecordTransferBytes with channel "relay"), so a relay wired
// to a durable store meters every session where the bytes actually flow. The
// channel vocabulary mirrors internal/database/usage.go's TransferChannel*.
type TransferMeter interface {
	RecordTransferBytes(ctx context.Context, organizationID, channel string, bytes int64) error
}

// TransferChannelRelay is the metering channel for relay passthrough bytes.
// It mirrors database.TransferChannelRelay; the relay package stays free of a
// database import, so the two strings must agree.
const TransferChannelRelay = "relay"

type pair struct {
	claims  TicketClaims
	legs    map[string]net.Conn
	done    chan struct{}
	pairing bool
	// token is the ticket presentation this pair opened with. It is recorded
	// before the store claim runs, and pair teardown consumes it, so a
	// captured ticket is one-use: its nonce cannot re-open a leg for the rest
	// of the ticket's TTL — even when one leg's claim wins the race against
	// the other leg's failed claim and teardown.
	token string
}
type Server struct {
	Signer               Signer
	Store                LegStore
	Meter                TransferMeter
	ID                   string
	Audience             string
	RequireOuterIdentity bool
	MaxPairs             int
	MaxPerOrganization   int
	MaxBytes             int64
	MaxLifetime          time.Duration
	IdleTimeout          time.Duration
	mu                   sync.Mutex
	pairs                map[string]*pair
	closed               bool
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
}

// NewServer builds a relay that refuses every leg until its authorization
// surface is configured: RequireOuterIdentity defaults to on, so a CONNECT is
// only ever served for a ticket whose machine is proven by a client
// certificate, and connect additionally refuses to run without a durable
// one-use leg store and a bound audience. An embedder that deliberately wants
// header-only identity (tests, harnesses) must switch the flag off explicitly
// after construction; insecurity is never the default.
func NewServer(signer Signer, maxPairs int) *Server {
	if maxPairs < 1 {
		maxPairs = 128
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{Signer: signer, ID: hex.EncodeToString(id[:]), RequireOuterIdentity: true, MaxPairs: maxPairs, MaxPerOrganization: 16, MaxBytes: 256 << 30, MaxLifetime: 2 * time.Hour, IdleTimeout: 2 * time.Minute, pairs: map[string]*pair{}, ctx: ctx, cancel: cancel}
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/relay/connect", s.connect)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	for _, p := range s.pairs {
		for _, c := range p.legs {
			if c != nil {
				_ = c.Close()
			}
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func CertifiedMachine(r *http.Request) (string, error) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return "", errors.New("verified relay client certificate required")
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}
func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect || r.ProtoMajor != 1 {
		http.Error(w, "HTTP/1.1 CONNECT required", 405)
		return
	}
	// The authorization surface is not optional: without a durable leg store a
	// captured ticket is replayable, without an audience it is portable to any
	// relay sharing the signing key, and without a verified machine identity
	// the pair binds to a claimed header instead of a certified machine.
	if s.Store == nil || s.Audience == "" {
		http.Error(w, "relay authorization is not configured: a durable leg store and a ticket audience are required", 503)
		return
	}
	token := r.Header.Get("X-Shift-Relay-Ticket")
	claims, err := s.Signer.Verify(token)
	if err != nil || claims.Audience != s.Audience {
		http.Error(w, "relay ticket rejected", 401)
		return
	}
	machine := r.Header.Get("X-Shift-Machine-ID")
	if s.RequireOuterIdentity {
		verified, certErr := CertifiedMachine(r)
		if certErr != nil || verified != machine {
			http.Error(w, "relay identity mismatch", http.StatusForbidden)
			return
		}
	}
	if machine == "" || (machine != claims.SourceMachineID && machine != claims.DestinationMachineID) {
		http.Error(w, "relay identity mismatch", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "CONNECT unavailable", 501)
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		http.Error(w, "relay closed", 503)
		return
	}
	state := s.pairs[claims.Nonce]
	if state == nil {
		count := 0
		for _, p := range s.pairs {
			if p.claims.OrganizationID == claims.OrganizationID {
				count++
			}
		}
		if len(s.pairs) >= s.MaxPairs || count >= s.MaxPerOrganization {
			s.mu.Unlock()
			w.Header().Set("Retry-After", "2")
			http.Error(w, "relay capacity exhausted", 503)
			return
		}
		state = &pair{claims: claims, legs: map[string]net.Conn{}, done: make(chan struct{})}
		s.pairs[claims.Nonce] = state
	} else if state.claims != claims {
		// One nonce must never mix two claim sets: a second ticket reusing a
		// live nonce with different legs would splice the wrong streams.
		s.mu.Unlock()
		http.Error(w, "relay ticket claims mismatch", 409)
		return
	}
	if _, exists := state.legs[machine]; exists {
		s.mu.Unlock()
		http.Error(w, "relay leg already attached", 409)
		return
	}
	state.legs[machine] = nil // reserve before hijack; exactly one goroutine forwards
	// The consume handle is recorded before the store claim runs: teardown of
	// a pair burns its ticket even when one leg's successful claim races the
	// other leg's failure, so the one-use promise never depends on timing.
	if state.token == "" {
		state.token = token
	}
	s.mu.Unlock()
	if s.Store != nil {
		if err := s.Store.ClaimRelayLeg(r.Context(), claims.OrganizationID, claims.MigrationID, claims.SourceMachineID, claims.DestinationMachineID, claims.Nonce, token, machine, s.ID); err != nil {
			s.abort(claims.Nonce, state)
			http.Error(w, "relay ticket consumed, revoked, or wrong shard", 409)
			return
		}
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		s.abort(claims.Nonce, state)
		return
	}
	conn = &bufferedConn{Conn: conn, reader: rw.Reader}
	s.mu.Lock()
	if s.closed || s.pairs[claims.Nonce] != state {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	state.legs[machine] = conn
	src, dst := state.legs[claims.SourceMachineID], state.legs[claims.DestinationMachineID]
	if src != nil && dst != nil && !state.pairing {
		state.pairing = true
		s.wg.Add(1)
		go s.forwardPair(state, src, dst)
	}
	s.mu.Unlock()
	timer := time.NewTimer(time.Until(time.Unix(claims.ExpiresUnix, 0)))
	defer timer.Stop()
	select {
	case <-state.done:
	case <-timer.C:
		s.mu.Lock()
		pending := !state.pairing
		s.mu.Unlock()
		if pending {
			s.abort(claims.Nonce, state)
		} else {
			<-state.done
		}
	case <-s.ctx.Done():
	}
}
func (s *Server) abort(nonce string, state *pair) {
	s.mu.Lock()
	if s.pairs[nonce] != state {
		s.mu.Unlock()
		return
	}
	for _, c := range state.legs {
		if c != nil {
			_ = c.Close()
		}
	}
	delete(s.pairs, nonce)
	token := state.token
	close(state.done)
	s.mu.Unlock()
	// Pair teardown burns the ticket: one-use end to end, so a captured
	// presentation cannot re-open the nonce for the rest of its TTL. The
	// consume runs whenever the pair opened with a presentation — including
	// when one leg's claim lost the race against the other leg's teardown —
	// and is best-effort only against a store outage: a store that is down
	// cannot resurrect the torn-down pair, and the per-leg claim timestamps
	// reject any re-claim older than two minutes.
	if s.Store != nil && token != "" {
		_ = s.Store.ConsumeRelayTicket(s.ctx, nonce, token, s.ID)
	}
}
func (s *Server) forwardPair(p *pair, a, b net.Conn) {
	defer s.wg.Done()
	defer s.abort(p.claims.Nonce, p)
	deadline := time.Now().Add(10 * time.Second)
	_ = a.SetDeadline(deadline)
	_ = b.SetDeadline(deadline)
	// No 200 until both authenticated outbound legs are attached.
	if _, err := io.WriteString(a, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if _, err := io.WriteString(b, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	_ = a.SetDeadline(time.Time{})
	_ = b.SetDeadline(time.Time{})
	countedA, countedB := &countingConn{Conn: a}, &countingConn{Conn: b}
	_ = Forward(s.ctx, countedA, countedB, s.MaxBytes, s.MaxLifetime, s.IdleTimeout)
	// Session close is the metering point: both copy goroutines have ended,
	// so the totals are final. The increment is one insert per relayed pair.
	if s.Meter != nil {
		total := countedA.written + countedB.written
		if total > 0 {
			_ = s.Meter.RecordTransferBytes(s.ctx, p.claims.OrganizationID, TransferChannelRelay, total)
		}
	}
}

// countingConn tallies the bytes written through one relay leg. Forward
// joins both copy goroutines before returning, so the counters are read only
// after their writers have finished.
type countingConn struct {
	net.Conn
	written int64
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written += int64(n)
	return n, err
}

// CloseWrite preserves the half-close contract Forward relies on: the
// counting wrapper must forward CloseWrite to the leg underneath, or the
// peer would never see EOF on a finished direction.
func (c *countingConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
func Forward(ctx context.Context, a, b net.Conn, maxBytes int64, lifetime, idle time.Duration) error {
	if lifetime <= 0 {
		lifetime = 2 * time.Hour
	}
	if idle <= 0 {
		idle = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()
	defer a.Close()
	defer b.Close()
	stop := context.AfterFunc(ctx, func() { _ = a.Close(); _ = b.Close() })
	defer stop()
	done := make(chan error, 2)
	// One budget for the whole pair: a relay session may move maxBytes in
	// aggregate, not maxBytes per direction.
	var moved atomic.Int64
	copyStream := func(dst, src net.Conn) {
		buffer := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			n, err := src.Read(buffer)
			if n > 0 {
				if maxBytes > 0 && moved.Add(int64(n)) > maxBytes {
					done <- errors.New("relay byte budget exceeded")
					return
				}
				_ = dst.SetWriteDeadline(time.Now().Add(idle))
				if _, e := dst.Write(buffer[:n]); e != nil {
					done <- e
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					if cw, ok := dst.(interface{ CloseWrite() error }); ok {
						_ = cw.CloseWrite()
					}
					done <- nil
				} else {
					done <- err
				}
				return
			}
		}
	}
	go copyStream(b, a)
	go copyStream(a, b)
	first := <-done
	if first != nil {
		_ = a.Close()
		_ = b.Close()
		<-done
		return first
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = a.Close()
		_ = b.Close()
		<-done
		return ctx.Err()
	}
}
func Dial(ctx context.Context, url, ticket, machine string) (net.Conn, error) {
	return DialTLS(ctx, url, "", nil, ticket, machine)
}
func DialTLS(ctx context.Context, url, serverName string, configuration *tls.Config, ticket, machine string) (net.Conn, error) {
	parsed, err := endpoint.Parse(url)
	if err != nil {
		return nil, err
	}
	if strings.ContainsAny(ticket+machine, "\r\n") || len(ticket) > 4096 {
		return nil, ErrInvalidTicket
	}
	if configuration == nil {
		configuration = &tls.Config{}
	} else {
		configuration = configuration.Clone()
	}
	configuration.MinVersion = tls.VersionTLS13
	configuration.NextProtos = []string{"http/1.1"}
	if serverName != "" {
		configuration.ServerName = serverName
	}
	if configuration.ServerName == "" {
		configuration.ServerName = parsed.Hostname()
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), "443")
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{}, Config: configuration}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	request := &http.Request{Method: http.MethodConnect, URL: &parsed, Host: parsed.Host, Header: make(http.Header)}
	request.URL.Path = "/v1/relay/connect"
	request.Header.Set("X-Shift-Relay-Ticket", ticket)
	request.Header.Set("X-Shift-Machine-ID", machine)
	// Write origin-form CONNECT to the dedicated h1 relay path.
	_, err = fmt.Fprintf(conn, "CONNECT /v1/relay/connect HTTP/1.1\r\nHost: %s\r\nX-Shift-Relay-Ticket: %s\r\nX-Shift-Machine-ID: %s\r\n\r\n", parsed.Host, ticket, machine)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if response.StatusCode != 200 {
		_ = response.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("relay refused leg: HTTP %d", response.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}
