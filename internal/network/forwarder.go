package network

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"shift.dev/shift/internal/model"
)

const (
	// forwardDialTimeout bounds how long opening the upstream leg of a
	// forwarded connection may take, so a wedged workload listener cannot
	// pile up half-open client connections.
	forwardDialTimeout = 5 * time.Second
	// forwardConnectionLimit caps concurrent connections per forwarder. A
	// workload that accepts without limit would otherwise exhaust the
	// agent's file descriptors; beyond the cap new connections are closed
	// immediately rather than queued.
	forwardConnectionLimit = 4096
)

// Forwarder carries TCP traffic between a host port a workload is reachable on
// and the port its restored process actually listens on. It is a plain proxy:
// it terminates nothing, rewrites nothing, and inspects nothing. New
// connections are accepted while it runs; Drain stops accepting and lets
// established connections finish, which is the connection-draining half of the
// migration contract. The other half — the workload being told that its old
// connections did not survive — is the status document in plan.go.
type Forwarder struct {
	mapping  model.PortMapping
	upstream string
	log      *slog.Logger

	listener net.Listener
	mu       sync.Mutex
	closed   bool
	conns    map[*forwardedConn]struct{}
	slots    chan struct{}
	idle     sync.WaitGroup
}

// forwardedConn is one proxied connection pair.
type forwardedConn struct {
	client   net.Conn
	upstream net.Conn
}

// NewForwarder creates a forwarder for a mapping. The logger may be nil, in
// which case forwarding problems are silently tolerated at the log level but
// still visible through the error returns of Start and Drain.
func NewForwarder(mapping model.PortMapping, log *slog.Logger) *Forwarder {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Forwarder{
		mapping:  mapping,
		upstream: fmt.Sprintf("127.0.0.1:%d", mapping.ContainerPort),
		log:      log,
		conns:    make(map[*forwardedConn]struct{}),
		slots:    make(chan struct{}, forwardConnectionLimit),
	}
}

// Mapping reports what the forwarder forwards.
func (forwarder *Forwarder) Mapping() model.PortMapping {
	return forwarder.mapping
}

// Start binds the host port on every interface — the port is the workload's
// published address, not a loopback-only convenience — and serves connections
// until Stop or Drain is called. It returns an error rather than retrying: a
// port that cannot be bound is a failed mapping, and the caller must surface
// that instead of running the workload half-reachable.
func (forwarder *Forwarder) Start(ctx context.Context) error {
	listener, err := net.Listen(ProtocolTCP, fmt.Sprintf(":%d", forwarder.mapping.HostPort))
	if err != nil {
		return fmt.Errorf("bind host port %d: %w", forwarder.mapping.HostPort, err)
	}
	forwarder.mu.Lock()
	if forwarder.closed {
		forwarder.mu.Unlock()
		listener.Close()
		return fmt.Errorf("forwarder for %s was stopped before it started", forwarder.mapping)
	}
	forwarder.listener = listener
	forwarder.mu.Unlock()

	forwarder.idle.Add(1)
	go forwarder.acceptLoop(ctx, listener)
	return nil
}

// acceptLoop hands accepted connections to handle until the listener closes.
func (forwarder *Forwarder) acceptLoop(ctx context.Context, listener net.Listener) {
	defer forwarder.idle.Done()
	for {
		client, err := listener.Accept()
		if err != nil {
			forwarder.mu.Lock()
			closed := forwarder.closed
			forwarder.mu.Unlock()
			if closed || ctx.Err() != nil {
				return
			}
			forwarder.log.Warn("forwarder accept failed", slog.String("mapping", forwarder.mapping.String()), slog.String("error", err.Error()))
			time.Sleep(50 * time.Millisecond)
			continue
		}
		forwarder.idle.Add(1)
		go func(client net.Conn) {
			defer forwarder.idle.Done()
			forwarder.handle(client)
		}(client)
	}
}

// handle proxies one connection from start to finish: it holds a connection
// slot until the pair has fully closed, so the cap bounds live connections,
// not handoffs.
func (forwarder *Forwarder) handle(client net.Conn) {
	select {
	case forwarder.slots <- struct{}{}:
	default:
		// At the connection cap: refuse by closing. A queued connection
		// would appear accepted to the peer while carrying no traffic,
		// which is worse than an honest refusal.
		client.Close()
		return
	}
	defer func() { <-forwarder.slots }()

	upstream, err := net.DialTimeout(ProtocolTCP, forwarder.upstream, forwardDialTimeout)
	if err != nil {
		forwarder.log.Debug("forwarder could not reach workload listener",
			slog.String("mapping", forwarder.mapping.String()),
			slog.String("error", err.Error()))
		client.Close()
		return
	}
	pair := &forwardedConn{client: client, upstream: upstream}
	forwarder.mu.Lock()
	if forwarder.closed {
		forwarder.mu.Unlock()
		client.Close()
		upstream.Close()
		return
	}
	forwarder.conns[pair] = struct{}{}
	forwarder.mu.Unlock()

	copies := make(chan struct{}, 2)
	go func() {
		io.Copy(upstream, client)
		halfClose(upstream)
		copies <- struct{}{}
	}()
	go func() {
		io.Copy(client, upstream)
		halfClose(client)
		copies <- struct{}{}
	}()
	<-copies
	<-copies
	forwarder.untrack(pair)
	client.Close()
	upstream.Close()
}

// halfClose signals end-of-stream in one direction so a peer that shut down
// writes is not left waiting for a response that will never come.
func halfClose(connection net.Conn) {
	if tcp, ok := connection.(*net.TCPConn); ok {
		tcp.CloseWrite()
	}
}

// untrack removes a finished pair from the drain set.
func (forwarder *Forwarder) untrack(pair *forwardedConn) {
	forwarder.mu.Lock()
	delete(forwarder.conns, pair)
	forwarder.mu.Unlock()
}

// ActiveConnections reports how many pairs are currently being carried. It is
// for status output and drain progress, not for correctness decisions.
func (forwarder *Forwarder) ActiveConnections() int {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	return len(forwarder.conns)
}

// stopListening closes the listener so no new connections arrive. It is the
// shared first step of Stop and Drain.
func (forwarder *Forwarder) stopListening() error {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	forwarder.closed = true
	if forwarder.listener != nil {
		return forwarder.listener.Close()
	}
	return nil
}

// snapshot returns the tracked pairs under the lock.
func (forwarder *Forwarder) snapshot() []*forwardedConn {
	forwarder.mu.Lock()
	defer forwarder.mu.Unlock()
	pairs := make([]*forwardedConn, 0, len(forwarder.conns))
	for pair := range forwarder.conns {
		pairs = append(pairs, pair)
	}
	return pairs
}

// Drain stops accepting new connections and waits up to grace for established
// ones to finish on their own, then closes whatever remains. It returns the
// number of connections that were force-closed after the grace period, and no
// forwarding goroutine outlives it. Callers that want a hard stop pass a zero
// grace.
func (forwarder *Forwarder) Drain(grace time.Duration) int {
	if err := forwarder.stopListening(); err != nil {
		forwarder.log.Debug("forwarder listener close", slog.String("error", err.Error()))
	}
	deadline := time.Now().Add(grace)
	for len(forwarder.snapshot()) > 0 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	remaining := forwarder.snapshot()
	for _, pair := range remaining {
		pair.client.Close()
		pair.upstream.Close()
	}
	forwarder.idle.Wait()
	return len(remaining)
}

// Stop is Drain with no grace period: everything closes now.
func (forwarder *Forwarder) Stop() {
	forwarder.Drain(0)
}
