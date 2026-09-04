package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/objectstore"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/transfer"
	"shift.dev/shift/internal/update"
)

type Service struct {
	config      config.Agent
	logger      *slog.Logger
	metrics     *observability.Metrics
	diagnostics *observability.Diagnostics
	tracer      *observability.Tracer
	otlp        *observability.OTLPExporter
	identity    *identity.Identity
	keys        *securestore.Manager
	inventory   *linuxplatform.Inventory
	runtime     *shiftruntime.Manager
	chunks      *chunkstore.Store
	objectStore objectstore.Store
	checkpoints *checkpoint.Service
	repository  *checkpoint.Repository
	restorer    *checkpoint.Restorer
	forker      *checkpoint.Forker
	sessions    *transfer.Sessions
	peerServer  *transfer.Server
	migrations  *migration.Orchestrator
	network     *network.Coordinator
	reporter    *controlplane.Reporter
	updates     *update.Manager
	startedAt   time.Time
	serversMu   sync.Mutex
	servers     []*http.Server
}

const (
	objectStoreCleanupInterval = 6 * time.Hour
	staleMultipartUploadAge    = 24 * time.Hour
	objectStoreCleanupTimeout  = 30 * time.Second
)

func Open(configuration config.Agent, logger *slog.Logger) (*Service, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = observability.NewLogger(configuration.LogLevel)
	}
	if err := os.MkdirAll(configuration.StateDir, 0o700); err != nil {
		return nil, err
	}
	machineIdentity, err := identity.Ensure(filepath.Join(configuration.StateDir, "identity"))
	if err != nil {
		return nil, err
	}
	if configuration.RemoteListen != "" && configuration.InsecureDevelopment && (configuration.TLS.CertificateFile == "" || configuration.TLS.PrivateKeyFile == "") {
		certificate, key, err := machineIdentity.EnsureTLSCertificate(filepath.Join(configuration.StateDir, "identity"), 365*24*time.Hour)
		if err != nil {
			return nil, err
		}
		configuration.TLS.CertificateFile = certificate
		configuration.TLS.PrivateKeyFile = key
	}
	keys, err := securestore.Open(filepath.Join(configuration.StateDir, "keys"))
	if err != nil {
		return nil, err
	}
	runtimeManager, err := shiftruntime.OpenManagerWithKeys(configuration.StateDir, false, logger, keys)
	if err != nil {
		return nil, err
	}
	chunks, err := chunkstore.Open(filepath.Join(configuration.StateDir, "objects"), configuration.ChunkSizeBytes, keys)
	if err != nil {
		return nil, err
	}
	repository, err := checkpoint.OpenRepository(filepath.Join(configuration.StateDir, "checkpoints"), keys)
	if err != nil {
		return nil, err
	}
	criuEngine, criuErr := checkpoint.NewCRIU("")
	var engine checkpoint.Engine
	if criuErr != nil {
		engine = unavailableEngine{reason: criuErr}
	} else {
		engine = criuEngine
	}
	inventory := linuxplatform.NewInventory(machineIdentity.Machine.ID)
	checkpointService := checkpoint.NewService(configuration.StateDir, runtimeManager, machineIdentity, inventory, chunks, repository, engine, logger)
	// The SHIFT-layer network: port reservations, forwarders, draining, and
	// the migration status documents applications read. It backs both sides
	// of a migration — the restorer publishes listeners at the destination
	// and the orchestrator drains them at the source.
	coordinator := network.NewCoordinator(logger.With("component", "network"))
	checkpointService.SetNetwork(coordinator)
	diagnostics := observability.NewDiagnostics()
	checkpointService.SetDiagnostics(diagnostics)
	var remoteObjects objectstore.Store
	if configuration.ObjectStore.Enabled {
		remoteObjects, err = configuration.ObjectStore.Open()
		if err != nil {
			return nil, fmt.Errorf("open object store: %w", err)
		}
		checkpointService.SetMirror(checkpoint.NewMirror(remoteObjects, chunks, repository))
	}
	restorer, err := checkpoint.OpenRestorer(checkpointService)
	if err != nil {
		return nil, err
	}
	forker, err := checkpoint.OpenForker(checkpointService)
	if err != nil {
		return nil, err
	}
	sessions, err := transfer.OpenSessions(configuration.StateDir, keys)
	if err != nil {
		return nil, err
	}
	metrics := observability.NewMetrics()
	metrics.SetDiagnostics(diagnostics)
	tracingServiceName := configuration.Tracing.ServiceName
	if tracingServiceName == "" {
		tracingServiceName = "shift-agent-" + machineIdentity.Machine.ID
	}
	var otlpExporter *observability.OTLPExporter
	if configuration.Tracing.Enabled() {
		otlpExporter = observability.NewOTLPExporter(logger, configuration.Tracing.OTLPEndpoint, tracingServiceName, config.Version)
	}
	var spanSinks []observability.SpanSink
	if otlpExporter != nil {
		spanSinks = append(spanSinks, otlpExporter)
	}
	tracer := observability.NewTracer(logger, spanSinks...)
	service := &Service{
		config: configuration, logger: logger, metrics: metrics, diagnostics: diagnostics,
		tracer: tracer, otlp: otlpExporter,
		identity: machineIdentity,
		keys:     keys, inventory: inventory, runtime: runtimeManager, chunks: chunks,
		objectStore: remoteObjects,
		checkpoints: checkpointService, repository: repository, restorer: restorer, forker: forker, sessions: sessions,
		network:   coordinator,
		startedAt: time.Now().UTC(),
	}
	authenticate := transfer.CertificateMachineID
	if configuration.InsecureDevelopment {
		authenticate = developmentPeerIdentity
	}
	service.peerServer = transfer.NewServer(sessions, keys, chunks, repository, restorer, inventory, authenticate, logger)
	service.peerServer.SetDiagnostics(diagnostics)
	if configuration.ControlPlane.Enabled() {
		machineName := configuration.ControlPlane.MachineName
		if machineName == "" {
			machineName = configuration.ControlPlane.MachineID
		}
		service.reporter = controlplane.NewReporter(
			configuration.ControlPlane.URL,
			configuration.ControlPlane.OrganizationID,
			configuration.ControlPlane.MachineID,
			machineName,
			configuration.ControlPlane.AgentURL,
			configuration.ControlPlane.APIKey,
			configuration.ControlPlane.Interval,
			configuration.ControlPlane.RequestTimeout,
			logger.With("component", "control_reporter"),
		)
		service.reporter.SetCapabilitiesProvider(func(ctx context.Context) (any, error) {
			return inventory.Inspect(ctx)
		})
	}
	factory := func(destination model.Destination) (migration.PeerClient, error) {
		if configuration.InsecureDevelopment {
			return transfer.NewDevelopmentClient(destination.AgentURL, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile)
		}
		caFile := configuration.TLS.PeerCAFile
		if caFile == "" {
			caFile = configuration.TLS.ClientCAFile
		}
		return transfer.NewClient(destination.AgentURL, destination.ServerName, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile, caFile)
	}
	migrations, err := migration.Open(configuration.StateDir, keys, runtimeManager, checkpointService, repository, chunks, machineIdentity, inventory, factory, configuration.MaxConcurrentMigrations, logger)
	if err != nil {
		return nil, err
	}
	migrations.SetSourceNetwork(coordinator)
	migrations.SetDiagnostics(diagnostics)
	migrations.SetTracer(tracer)
	service.migrations = migrations
	if err := service.openUpdates(); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *Service) Run(ctx context.Context) error {
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if s.reporter != nil {
		reporterContext, stopReporter := context.WithCancel(ctx)
		s.reporter.Start(reporterContext)
		defer func() {
			stopReporter()
			s.reporter.Stop()
		}()
	}
	localListener, localTLS, localPath, err := s.listener(s.config.Listen, false)
	if err != nil {
		return err
	}
	localServer := &http.Server{
		Handler:           s.metrics.Middleware(s.logger, observability.TraceMiddleware(s.tracer, s.localHandler())),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		ConnContext: connectionContext,
	}
	s.addServer(localServer)
	errorsChannel := make(chan error, 2)
	go serveHTTP(localServer, localListener, localTLS, errorsChannel)
	if localPath != "" {
		defer os.Remove(localPath)
	}
	if s.config.RemoteListen != "" {
		remoteListener, remoteTLS, _, err := s.listener(s.config.RemoteListen, true)
		if err != nil {
			_ = localServer.Close()
			return err
		}
		remoteServer := &http.Server{
			Handler:           s.metrics.Middleware(s.logger, observability.TraceMiddleware(s.tracer, s.peerServer.Handler())),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		}
		s.addServer(remoteServer)
		go serveHTTP(remoteServer, remoteListener, remoteTLS, errorsChannel)
	}
	go s.objectStoreCleanupLoop(runContext)
	go s.runSampler(runContext, s.diagnostics)
	if s.otlp != nil {
		go s.otlp.Run(runContext)
	}
	if s.updates != nil {
		go s.updates.Run(runContext)
	}
	s.migrations.Recover(ctx)
	s.logger.Info("shift agent started", "machine_id", s.identity.Machine.ID, "listen", s.config.Listen, "remote_listen", s.config.RemoteListen, "version", config.Version)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
		defer cancel()
		return s.shutdown(shutdownCtx)
	case err := <-errorsChannel:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Service) objectStoreCleanupLoop(ctx context.Context) {
	cleanup := func() {
		if s.objectStore == nil || s.checkpoints == nil {
			return
		}
		cleanupContext, cancel := context.WithTimeout(ctx, objectStoreCleanupTimeout)
		defer cancel()
		removed, err := s.checkpoints.CleanupMirrorUploads(cleanupContext, time.Now().UTC().Add(-staleMultipartUploadAge))
		if err != nil {
			if !errors.Is(err, checkpoint.ErrMirrorDisabled) && !errors.Is(err, context.Canceled) {
				s.logger.Warn("multipart cleanup failed", "error", err)
			}
			return
		}
		if removed > 0 {
			s.logger.Info("stale multipart uploads cleaned", "uploads", removed)
		}
	}
	cleanup()
	ticker := time.NewTicker(objectStoreCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

func (s *Service) listener(endpoint string, remote bool) (net.Listener, *tls.Config, string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, "", err
	}
	switch parsed.Scheme {
	case "unix":
		if remote {
			return nil, nil, "", errors.New("peer listener cannot use a Unix socket")
		}
		if err := os.MkdirAll(filepath.Dir(parsed.Path), 0o755); err != nil {
			return nil, nil, "", err
		}
		if info, err := os.Lstat(parsed.Path); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return nil, nil, "", errors.New("refusing to replace a non-socket listener path")
			}
			if err := os.Remove(parsed.Path); err != nil {
				return nil, nil, "", err
			}
		}
		listener, err := net.Listen("unix", parsed.Path)
		if err != nil {
			return nil, nil, "", err
		}
		if err := os.Chmod(parsed.Path, 0o660); err != nil {
			_ = listener.Close()
			return nil, nil, "", err
		}
		// A socket owned by the agent's own group locks out every other user,
		// so the local CLI would need root to talk to the local agent. Handing
		// the socket to the configured group lets its members connect. A
		// missing group or a failed chown only narrows access — warn and keep
		// serving rather than refusing to start.
		if group := s.config.SocketGroup; group != "" {
			if groupID, err := strconv.Atoi(group); err == nil {
				if err := os.Chown(parsed.Path, -1, groupID); err != nil {
					s.logger.Warn("socket group ownership not applied", "group", group, "error", err.Error())
				}
			} else if resolved, err := user.LookupGroup(group); err == nil {
				gid, _ := strconv.Atoi(resolved.Gid)
				if err := os.Chown(parsed.Path, -1, gid); err != nil {
					s.logger.Warn("socket group ownership not applied", "group", group, "error", err.Error())
				}
			} else {
				s.logger.Warn("socket group not found; only the agent's own user can connect", "group", group)
			}
		}
		return credentialListener{Listener: listener}, nil, parsed.Path, nil
	case "tcp":
		listener, err := net.Listen("tcp", parsed.Host)
		if err != nil {
			return nil, nil, "", err
		}
		if !remote && s.config.InsecureDevelopment && s.config.TLS.CertificateFile == "" {
			return listener, nil, "", nil
		}
		tlsConfig, err := s.tlsConfig(remote)
		if err != nil {
			_ = listener.Close()
			return nil, nil, "", err
		}
		return listener, tlsConfig, "", nil
	default:
		return nil, nil, "", fmt.Errorf("unsupported listener scheme %q", parsed.Scheme)
	}
}

func (s *Service) tlsConfig(remote bool) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(s.config.TLS.CertificateFile, s.config.TLS.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	configuration := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	if s.config.InsecureDevelopment {
		configuration.ClientAuth = tls.RequestClientCert
		return configuration, nil
	}
	caPEM, err := os.ReadFile(s.config.TLS.ClientCAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("client CA file contains no certificates")
	}
	configuration.ClientCAs = pool
	configuration.ClientAuth = tls.RequireAndVerifyClientCert
	if !remote {
		configuration.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return configuration, nil
}

func (s *Service) addServer(server *http.Server) {
	s.serversMu.Lock()
	defer s.serversMu.Unlock()
	s.servers = append(s.servers, server)
}

func (s *Service) shutdown(ctx context.Context) error {
	s.serversMu.Lock()
	servers := append([]*http.Server(nil), s.servers...)
	s.serversMu.Unlock()
	var combined error
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	return combined
}

func serveHTTP(server *http.Server, listener net.Listener, tlsConfiguration *tls.Config, errorsChannel chan<- error) {
	if tlsConfiguration != nil {
		listener = tls.NewListener(listener, tlsConfiguration)
	}
	errorsChannel <- server.Serve(listener)
}

func developmentPeerIdentity(request *http.Request) (string, error) {
	if request.TLS == nil || len(request.TLS.PeerCertificates) == 0 {
		return "", errors.New("development peer still requires a client certificate")
	}
	digest := sha256.Sum256(request.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
	return hex.EncodeToString(digest[:]), nil
}

type unavailableEngine struct {
	reason error
}

func (e unavailableEngine) Check(context.Context) error                           { return e.reason }
func (e unavailableEngine) Version(context.Context) (string, error)               { return "", e.reason }
func (e unavailableEngine) PreDump(context.Context, checkpoint.DumpOptions) error { return e.reason }
func (e unavailableEngine) Dump(context.Context, checkpoint.DumpOptions) error    { return e.reason }
func (e unavailableEngine) Restore(context.Context, checkpoint.RestoreOptions) (int, error) {
	return 0, e.reason
}
