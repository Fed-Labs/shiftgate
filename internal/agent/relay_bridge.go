package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/relay"
)

type relayJoin struct {
	Ticket               string    `json:"ticket"`
	Endpoint             string    `json:"endpoint"`
	SourceMachineID      string    `json:"source_machine_id"`
	DestinationMachineID string    `json:"destination_machine_id"`
	MigrationID          string    `json:"migration_id"`
	ExpiresAt            time.Time `json:"expires_at"`
}

func (s *Service) relayTLS() (*tls.Config, error) {
	pem, err := os.ReadFile(s.config.Relay.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load relay trust: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("relay CA has no certificates")
	}
	certificate, err := tls.LoadX509KeyPair(s.config.TLS.CertificateFile, s.config.TLS.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: s.config.Relay.ServerName}, nil
}

type relayStart struct {
	WorkloadID     string              `json:"workload_id"`
	Destination    model.Destination   `json:"destination"`
	Mode           model.MigrationMode `json:"mode"`
	PreCopyPasses  int                 `json:"pre_copy_passes,omitempty"`
	TimeoutSeconds int                 `json:"timeout_seconds,omitempty"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
}

// handleRelayCommand accepts only the two narrow peer-transfer commands. It
// never exposes local workload/exec routes through the relay or command poll.
func (s *Service) handleRelayCommand(ctx context.Context, command controlplane.AgentCommand) error {
	if !s.config.Relay.Enabled {
		return errors.New("relay is not enabled")
	}
	if command.Action == "relay.start" {
		var start relayStart
		if json.Unmarshal(command.Payload, &start) != nil || start.WorkloadID == "" || start.Destination.Route != "relay" || start.Destination.RelayTicket == "" {
			return errors.New("relay start binding invalid")
		}
		// The command channel is another admission point for live moves, and
		// it is gated exactly like the local API: live mode is a paid
		// entitlement, and a relayed live migration without one must fail
		// here rather than run the paid feature for a free-tier org.
		if start.Mode == model.MigrationLive && !s.liveMigrationAllowed() {
			return fmt.Errorf("%s: %s", billing.LiveMigrationPlanRequiredCode, billing.LiveMigrationPlanRequiredMessage)
		}
		_, err := s.migrations.Create(ctx, migration.CreateRequest{WorkloadID: start.WorkloadID, IdempotencyKey: start.IdempotencyKey, Destination: start.Destination, Mode: start.Mode, PreCopyPasses: start.PreCopyPasses, Timeout: time.Duration(start.TimeoutSeconds) * time.Second})
		return err
	}
	if command.Action != "relay.join" {
		return errors.New("unsupported agent command")
	}

	var join relayJoin
	if json.Unmarshal(command.Payload, &join) != nil || join.Ticket == "" || !relay.SameEndpoint(join.Endpoint, s.config.Relay.Endpoint) || join.DestinationMachineID != s.identity.Machine.ID || join.MigrationID == "" || !join.ExpiresAt.After(time.Now()) {
		return errors.New("relay join binding invalid")
	}
	claims, err := relay.Decode(join.Ticket)
	if err != nil || claims.DestinationMachineID != join.DestinationMachineID || claims.SourceMachineID != join.SourceMachineID || claims.MigrationID != join.MigrationID || claims.OrganizationID != s.config.ControlPlane.OrganizationID {
		return errors.New("relay join claims invalid")
	}
	outer, err := s.relayTLS()
	if err != nil {
		return err
	}
	inner, err := s.tlsConfig(true)
	if err != nil {
		return err
	}
	s.relayMu.Lock()
	if s.relayContext == nil {
		s.relayMu.Unlock()
		return errors.New("relay agent is not running")
	}
	if _, ok := s.relayJoins[join.MigrationID]; ok {
		// A redelivery of work already running is acknowledged as the no-op
		// it is: the migration id is the idempotency key, not the command id.
		s.relayMu.Unlock()
		return nil
	}
	if len(s.relayJoins) >= 4 {
		s.relayMu.Unlock()
		return errors.New("relay bridge capacity exhausted")
	}
	relayContext := s.relayContext
	s.relayJoins[join.MigrationID] = func() {} // reservation; the real cancel replaces it
	s.relayMu.Unlock()

	// The initial dial is part of the command: its outcome is what the
	// completion record reflects, so a join that never reached the relay is a
	// failed command — redelivered while the ticket is live — never a silent
	// local log line behind an ack that said success.
	budget := 20 * time.Second
	if remaining := time.Until(join.ExpiresAt); remaining < budget {
		budget = remaining
	}
	dialContext, stopDial := context.WithTimeout(relayContext, budget)
	raw, err := relay.DialTLS(dialContext, join.Endpoint, s.config.Relay.ServerName, outer, join.Ticket, s.identity.Machine.ID)
	stopDial()
	if err != nil {
		s.relayMu.Lock()
		delete(s.relayJoins, join.MigrationID)
		s.relayMu.Unlock()
		return fmt.Errorf("relay join for migration %s did not reach the relay: %w", join.MigrationID, err)
	}

	s.relayMu.Lock()
	if s.relayContext != relayContext {
		s.relayMu.Unlock()
		_ = raw.Close()
		return errors.New("relay agent stopped before the join could be bridged")
	}
	ctx, cancel := context.WithTimeout(relayContext, 2*time.Hour)
	s.relayJoins[join.MigrationID] = cancel
	s.relayWG.Add(1)
	s.relayMu.Unlock()
	go func() {
		defer s.relayWG.Done()
		defer cancel()
		defer func() { s.relayMu.Lock(); delete(s.relayJoins, join.MigrationID); s.relayMu.Unlock() }()
		// Metered bridge: the bytes this relayed session moves are recorded
		// at session close; the reporter carries them to the control plane.
		var meter relay.TransferMeter
		if s.reporter != nil {
			meter = s.reporter
		}
		if err := relay.ServePeerConnMetered(ctx, raw, inner, s.peerServer.Handler(), meter, s.config.ControlPlane.OrganizationID); err != nil && ctx.Err() == nil {
			s.logger.Warn("relay peer bridge closed", "migration_id", join.MigrationID)
		}
	}()
	return nil
}

func (s *Service) startRelayLifecycle(ctx context.Context) {
	s.relayMu.Lock()
	s.relayContext = ctx
	s.relayJoins = map[string]context.CancelFunc{}
	s.relayMu.Unlock()
}
func (s *Service) stopRelayLifecycle() {
	s.relayMu.Lock()
	s.relayContext = nil
	for _, cancel := range s.relayJoins {
		cancel()
	}
	s.relayMu.Unlock()
	s.relayWG.Wait()
}

// A one-use ticket never authorizes a second network connection. net/http can
// retry HTTP on the existing stream, but cannot replay a consumed CONNECT.
func singleRelayDial(dial func(context.Context) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	var once sync.Once
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		var c net.Conn
		err := errors.New("relay tunnel lost; start a fresh migration attempt")
		once.Do(func() { c, err = dial(ctx) })
		return c, err
	}
}
