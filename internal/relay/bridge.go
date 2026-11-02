package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// ServePeerConn feeds an outbound tunnel to the existing peer server. Passing
// the *tls.Conn itself into http.Server preserves request.TLS and peer identity.
func ServePeerConn(ctx context.Context, raw net.Conn, configuration *tls.Config, handler http.Handler) error {
	return ServePeerConnMetered(ctx, raw, configuration, handler, nil, "")
}

// ServePeerConnMetered is ServePeerConn with bandwidth metering: when meter is
// non-nil, the bytes the bridged session moves are recorded to the named
// organization when the session closes. The agent wires its usage meter here
// so relayed transfers land in usage_records alongside the storage gauges.
func ServePeerConnMetered(ctx context.Context, raw net.Conn, configuration *tls.Config, handler http.Handler, meter TransferMeter, organizationID string) error {
	if raw == nil || configuration == nil || handler == nil {
		return errors.New("peer bridge needs a connection, TLS config, and handler")
	}
	secure := tls.Server(&countingConn{Conn: raw}, configuration.Clone())
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	defer raw.Close()
	handshake, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := secure.HandshakeContext(handshake)
	cancel()
	if err != nil {
		return err
	}
	listener := &singleConnListener{conn: secure, closed: make(chan struct{})}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute,
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				_ = listener.Close()
			}
		}}
	stopServer := context.AfterFunc(ctx, func() { _ = server.Close(); _ = listener.Close() })
	defer stopServer()
	defer server.Close()
	serveErr := server.Serve(listener)
	if meter != nil && organizationID != "" {
		if counted, ok := secure.NetConn().(*countingConn); ok && counted.written > 0 {
			_ = meter.RecordTransferBytes(ctx, organizationID, TransferChannelRelay, counted.written)
		}
	}
	if errors.Is(serveErr, net.ErrClosed) || errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

type singleConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *singleConnListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *singleConnListener) Addr() net.Addr { return relayAddr("outbound-peer") }

type relayAddr string

func (r relayAddr) Network() string { return "relay" }
func (r relayAddr) String() string  { return string(r) }
