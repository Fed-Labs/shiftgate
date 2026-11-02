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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/fencing"
	"shift.dev/shift/internal/gc"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/network"
	"shift.dev/shift/internal/objectstore"
	"shift.dev/shift/internal/observability"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/relay"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/transfer"
	"shift.dev/shift/internal/update"
)

type Service struct {
	stateLock   *gc.StateLock
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
	cloner      *checkpoint.Cloner
	sessions    *transfer.Sessions
	peerServer  *transfer.Server
	migrations  *migration.Orchestrator
	network     *network.Coordinator
	reporter    *controlplane.Reporter
	updates     *update.Manager
	// hostedStorage drives the control-plane object-store credential loop when
	// object_store.backend is "control-plane"; nil otherwise.
	hostedStorage *hostedStorageLoop
	// standby is this agent's warm-standby duty supervisor: the records of
	// the workloads it holds replicated state for, and the loop that watches
	// their sources. Always non-nil; without a control plane it simply never
	// confirms a death automatically.
	standby *standbySupervisor
	// replicator pushes checkpoints to the standbys named by workload
	// failover policies. Always non-nil; it does nothing when no workload
	// carries a policy.
	replicator *failoverReplicator
	startedAt  time.Time
	serversMu  sync.Mutex
	servers    []*http.Server
	// backgroundLoops tracks the goroutines Run starts so shutdown can stop
	// and wait for them before the state lock is released.
	backgroundLoops backgroundLoopTracker
	relayMu         sync.Mutex
	relayContext    context.Context
	relayJoins      map[string]context.CancelFunc
	relayWG         sync.WaitGroup
	// gcGuard is the in-process write guard the online collector (gc.OpenOnline)
	// serializes against: mutating handlers share it (guardWrites), background
	// writers such as the periodic-checkpoint loop take it around their own
	// chunk-resolving work, and a GC sweep takes it exclusively, so a chunk
	// cannot be deleted between a writer resolving it and installing its
	// reference.
	gcGuard gcWriteGuard
	// gcRunMu serializes concurrent POST /v1/gc handlers: two collectors over
	// the same state directory would race the orphan ledger even though each
	// sweep is internally guarded.
	gcRunMu sync.Mutex
	// entitlement caches what the control plane says about this agent's
	// live-migration allowance. When nothing has been learned, the explicit
	// detached-agent configuration assertion is consulted instead; absent
	// both, live migrations are refused.
	entitlementMu     sync.Mutex
	entitlementKnown  bool
	entitlementAllows bool
}

// liveMigrationAllowed is the agent half of the live-migration paywall. Truth
// comes from the control-plane identity's entitlement, learned on the
// heartbeat and cached; a detached agent has no entitlement source and
// consults the explicit configuration assertion (config.Agent.Entitlement).
// Absent both: cold-only. The gate fails closed on purpose — live mode is a
// paid entitlement enforced at every admission point the product controls,
// including peer-to-peer moves.
func (s *Service) liveMigrationAllowed() bool {
	s.entitlementMu.Lock()
	defer s.entitlementMu.Unlock()
	if s.entitlementKnown {
		return s.entitlementAllows
	}
	if s.config.ControlPlane.Enabled() {
		// A managed agent's licensing truth is the control plane's, learned
		// on the heartbeat and never forged locally: until the first
		// heartbeat response reports the entitlement, the gate fails closed,
		// and the configuration assertion is deliberately ignored — an agent
		// with a control-plane identity must not be able to grant itself the
		// paid tier by editing a file. Only a detached agent (no control
		// plane) has no entitlement source and may assert explicitly.
		return false
	}
	return s.config.Entitlement.LiveMigration
}

// noteEntitlement records the live-migration allowance the heartbeat (or any
// control-plane round trip) learned, overriding the local configuration
// assertion: a managed agent's licensing truth is the control plane's.
func (s *Service) noteEntitlement(allowsLiveMigration bool) {
	s.entitlementMu.Lock()
	defer s.entitlementMu.Unlock()
	s.entitlementKnown = true
	s.entitlementAllows = allowsLiveMigration
}

// gcWriteGuard is the agent's in-process checkpoint/transfer write exclusion.
// Writers share it (RLock via guardWrites) and the online garbage collector
// holds it exclusively (gc.WriteGuard's Lock/Unlock).
type gcWriteGuard struct {
	mu sync.RWMutex
}

func (g *gcWriteGuard) Lock()   { g.mu.Lock() }
func (g *gcWriteGuard) Unlock() { g.mu.Unlock() }

// guardWrites shares the write guard across mutating requests (reads run
// untouched), so the online collector can exclude every writer at once
// without serializing writers against each other. The GC route itself is
// excluded: its handler takes the exclusive side of the same lock.
func (s *Service) guardWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet || request.Method == http.MethodHead || request.URL.Path == "/v1/gc" {
			next.ServeHTTP(writer, request)
			return
		}
		s.gcGuard.mu.RLock()
		defer s.gcGuard.mu.RUnlock()
		next.ServeHTTP(writer, request)
	})
}

const (
	objectStoreCleanupInterval = 6 * time.Hour
	staleMultipartUploadAge    = 24 * time.Hour
	objectStoreCleanupTimeout  = 30 * time.Second
)

// backgroundLoopTracker tracks the daemon's background goroutines so shutdown
// can cancel them, wait for them to finish, and report any that outlive the
// shutdown deadline. The state lock must never be released while one of them
// is still mid-write in the state directory, or a restart races the dying
// process.
type backgroundLoopTracker struct {
	mu      sync.Mutex
	running map[string]struct{}
	wg      sync.WaitGroup
}

func (t *backgroundLoopTracker) launch(ctx context.Context, name string, loop func(context.Context)) {
	t.mu.Lock()
	if t.running == nil {
		t.running = make(map[string]struct{})
	}
	t.running[name] = struct{}{}
	t.mu.Unlock()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() {
			t.mu.Lock()
			delete(t.running, name)
			t.mu.Unlock()
		}()
		loop(ctx)
	}()
}

func (t *backgroundLoopTracker) pending() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	names := make([]string, 0, len(t.running))
	for name := range t.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// wait blocks until every launched loop has returned or the context expires.
func (t *backgroundLoopTracker) wait(ctx context.Context) error {
	finished := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(finished)
	}()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Open(configuration config.Agent, logger *slog.Logger) (*Service, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	stateLock, err := gc.AcquireStateLock(configuration.StateDir)
	if err != nil {
		return nil, err
	}
	keepStateLock := false
	defer func() {
		if !keepStateLock {
			_ = stateLock.Close()
		}
	}()
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
	if (configuration.RemoteListen != "" || configuration.Relay.Enabled) && configuration.InsecureDevelopment && (configuration.TLS.CertificateFile == "" || configuration.TLS.PrivateKeyFile == "") {
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
	runtimeManager, err := shiftruntime.OpenManagerWithCgroups(configuration.StateDir, configuration.CgroupRoot, false, logger, keys)
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
	var hosted *hostedStorageLoop
	deferred := objectstore.NewDeferred()
	backend := strings.ToLower(strings.TrimSpace(configuration.ObjectStore.Backend))
	if configuration.ObjectStore.Enabled && backend == objectstore.ControlPlaneBackend {
		// Hosted mirroring: the store is empty until the control plane issues
		// credentials. The reporter's control-plane block is the credential
		// source, so it is required here — an agent that cannot authenticate
		// to a control plane cannot fetch credentials either.
		if !configuration.ControlPlane.Enabled() {
			return nil, errors.New("object_store.backend control-plane requires a configured control_plane block (url, organization_id, machine_id, api_key)")
		}
		if strings.TrimSpace(configuration.ObjectStore.StateDir) == "" {
			return nil, errors.New("object_store.backend control-plane requires object_store.state_dir for multipart state")
		}
		hosted = newHostedStorageLoop(hostedStorageConfig{
			controlURL:     configuration.ControlPlane.URL,
			organization:   configuration.ControlPlane.OrganizationID,
			apiKey:         configuration.ControlPlane.APIKey,
			stateDir:       configuration.ObjectStore.StateDir,
			requestTimeout: configuration.ControlPlane.RequestTimeout,
		}, deferred, logger.With("component", "hosted_storage"))
		remoteObjects = deferred
		checkpointService.SetMirror(checkpoint.NewMirror(remoteObjects, chunks, repository))
	} else if configuration.ObjectStore.Enabled {
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
	cloner, err := checkpoint.OpenCloner(checkpointService)
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
		stateLock: stateLock,
		config:    configuration, logger: logger, metrics: metrics, diagnostics: diagnostics,
		tracer: tracer, otlp: otlpExporter,
		identity: machineIdentity,
		keys:     keys, inventory: inventory, runtime: runtimeManager, chunks: chunks,
		objectStore: remoteObjects,
		checkpoints: checkpointService, repository: repository, restorer: restorer, forker: forker, cloner: cloner, sessions: sessions,
		network:       coordinator,
		hostedStorage: hosted,
		startedAt:     time.Now().UTC(),
	}
	keepStateLock = true
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
		if configuration.Relay.Enabled {
			service.reporter.SetCommandHandler(service.handleRelayCommand)
		}
		service.reporter.SetCapabilitiesProvider(func(ctx context.Context) (any, error) {
			return inventory.Inspect(ctx)
		})
		// The live-migration entitlement rides the heartbeat: the control
		// plane answers every one with the organization's allowance, and the
		// gate caches it. This is the only production path that sets the
		// entitlement — a managed agent that never learns it stays closed.
		service.reporter.SetEntitlementListener(service.noteEntitlement)
		// Bandwidth metering: completed transfer sessions record their
		// delivered bytes against the organization, and the reporter carries
		// the deltas to the control plane on its heartbeats (usage_records).
		service.peerServer.SetTransferMeter(configuration.ControlPlane.OrganizationID, service.reporter)
	}
	factory := func(destination model.Destination) (migration.PeerClient, error) {
		if destination.Route == "relay" {
			if !configuration.Relay.Enabled || configuration.Relay.Endpoint == "" || destination.RelayEndpoint != configuration.Relay.Endpoint || destination.RelayTicket == "" {
				return nil, errors.New("relay route requested but relay configuration or ticket is missing")
			}
			caPEM, err := os.ReadFile(configuration.Relay.CAFile)
			if err != nil {
				return nil, fmt.Errorf("load relay CA: %w", err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(caPEM) {
				return nil, errors.New("relay CA contains no certificates")
			}
			certificate, err := tls.LoadX509KeyPair(configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load relay client identity: %w", err)
			}
			outerTLS := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{certificate}, ServerName: configuration.Relay.ServerName}
			innerCA := configuration.TLS.PeerCAFile
			if innerCA == "" {
				innerCA = configuration.TLS.ClientCAFile
			}
			if configuration.InsecureDevelopment {
				return transfer.NewDevelopmentClientWithDialer(destination.AgentURL, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile, singleRelayDial(func(ctx context.Context) (net.Conn, error) {
					return relay.DialTLS(ctx, destination.RelayEndpoint, configuration.Relay.ServerName, outerTLS, destination.RelayTicket, machineIdentity.Machine.ID)
				}))
			}
			return transfer.NewClientWithDialer(destination.AgentURL, destination.ServerName, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile, innerCA, singleRelayDial(func(ctx context.Context) (net.Conn, error) {
				return relay.DialTLS(ctx, destination.RelayEndpoint, configuration.Relay.ServerName, outerTLS, destination.RelayTicket, machineIdentity.Machine.ID)
			}))
		}
		if configuration.InsecureDevelopment {
			return transfer.NewDevelopmentClient(destination.AgentURL, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile)
		}
		caFile := configuration.TLS.PeerCAFile
		if caFile == "" {
			caFile = configuration.TLS.ClientCAFile
		}
		return transfer.NewClient(destination.AgentURL, destination.ServerName, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile, caFile)
	}
	// peerClientFor builds a client for any peer listener — a migration
	// destination, a failover standby, or a source being probed — from the
	// same TLS material, so every peer channel in the agent answers to one
	// configuration. An empty server name verifies the certificate against
	// the names it carries; identity is pinned by machine id where it
	// matters, not by hostname.
	peerClientFor := func(endpoint string) (*transfer.Client, error) {
		if configuration.InsecureDevelopment {
			return transfer.NewDevelopmentClient(endpoint, configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile)
		}
		caFile := configuration.TLS.PeerCAFile
		if caFile == "" {
			caFile = configuration.TLS.ClientCAFile
		}
		return transfer.NewClient(endpoint, "", configuration.TLS.CertificateFile, configuration.TLS.PrivateKeyFile, caFile)
	}
	standbyDuties, err := securestore.OpenEncryptedCollection[model.StandbyDuty](filepath.Join(configuration.StateDir, "metadata", "standby-duties.enc.json"), "standby-duties-v1", keys)
	if err != nil {
		return nil, err
	}
	replicationLedger, err := securestore.OpenEncryptedCollection[model.ReplicationEntry](filepath.Join(configuration.StateDir, "metadata", "failover-replication.enc.json"), "failover-replication-v1", keys)
	if err != nil {
		return nil, err
	}
	// The standby's presence query uses the control-plane reporter's own
	// settings: without them this agent cannot confirm a source's death and
	// its supervisor only ever acts on an explicit operator trigger.
	presenceURL, presenceOrg, presenceKey := "", "", ""
	if configuration.ControlPlane.Enabled() {
		presenceURL = configuration.ControlPlane.URL
		presenceOrg = configuration.ControlPlane.OrganizationID
		presenceKey = configuration.ControlPlane.APIKey
	}
	var fenceProvider fencing.Provider = fencing.Disabled{}
	switch configuration.Fencing.Provider {
	case "self":
		fenceProvider = fencing.Self{}
	case "command":
		fenceProvider = fencing.Command{Path: configuration.Fencing.Command, StatusPath: configuration.Fencing.StatusCommand, Timeout: configuration.Fencing.Timeout}
	}
	service.standby = openStandbySupervisor(standbyDuties, sessions, runtimeManager, checkpointService, restorer,
		presenceURL, presenceOrg, presenceKey, machineIdentity.Machine.ID, peerClientFor, fenceProvider, logger.With("component", "standby"))
	service.replicator = &failoverReplicator{
		ledger: replicationLedger, runtime: runtimeManager, checkpoints: checkpointService,
		keys: keys, chunks: chunks, identity: machineIdentity, peerClient: peerClientFor,
		advertisedURL:     configuration.ControlPlane.AgentURL,
		migrationInFlight: func(workloadID string) bool { return service.migrationInFlight(workloadID) },
		logger:            logger.With("component", "failover_replicator"),
		guard:             &service.gcGuard,
	}
	service.peerServer.SetReplicationWatcher(service.standby)
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
	if s.config.Relay.Enabled {
		s.startRelayLifecycle(runContext)
		defer s.stopRelayLifecycle()
	}
	if s.reporter != nil {
		reporterContext, stopReporter := context.WithCancel(ctx)
		s.reporter.Start(reporterContext)
		defer func() {
			stopReporter()
			s.reporter.Stop()
		}()
	}
	localListener, localTLS, localPath, err := s.listener(ctx, s.config.Listen, false)
	if err != nil {
		return err
	}
	localServer := &http.Server{
		Handler:           s.metrics.Middleware(s.logger, observability.TraceMiddleware(s.tracer, s.guardWrites(s.localHandler()))),
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		ConnContext: connectionContext,
	}
	s.addServer(localServer)
	errorsChannel := make(chan error, 2)
	go serveHTTP(localServer, localListener, localTLS, errorsChannel)
	if localPath != "" {
		defer func() { _ = os.Remove(localPath) }()
	}
	if s.config.RemoteListen != "" {
		remoteListener, remoteTLS, _, err := s.listener(ctx, s.config.RemoteListen, true)
		if err != nil {
			_ = localServer.Close()
			return err
		}
		remoteServer := &http.Server{
			Handler:           s.metrics.Middleware(s.logger, observability.TraceMiddleware(s.tracer, s.guardWrites(s.peerServer.Handler()))),
			ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		}
		s.addServer(remoteServer)
		go serveHTTP(remoteServer, remoteListener, remoteTLS, errorsChannel)
	}
	s.backgroundLoops.launch(runContext, "object_store_cleanup", s.objectStoreCleanupLoop)
	s.backgroundLoops.launch(runContext, "sampler", func(ctx context.Context) { s.runSampler(ctx, s.diagnostics) })
	s.backgroundLoops.launch(runContext, "checkpoint_policies", s.runCheckpointPolicies)
	s.backgroundLoops.launch(runContext, "standby", s.standby.run)
	if s.hostedStorage != nil {
		s.backgroundLoops.launch(runContext, "hosted_storage", s.hostedStorage.run)
	}
	if s.otlp != nil {
		s.backgroundLoops.launch(runContext, "otlp", s.otlp.Run)
	}
	if s.updates != nil {
		s.backgroundLoops.launch(runContext, "updates", s.updates.Run)
	}
	s.migrations.Recover(ctx)
	s.logger.Info("shift agent started", "machine_id", s.identity.Machine.ID, "listen", s.config.Listen, "remote_listen", s.config.RemoteListen, "version", config.Version)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
		defer cancel()
		return s.shutdown(shutdownCtx, cancelRun)
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

func (s *Service) listener(ctx context.Context, endpoint string, remote bool) (net.Listener, *tls.Config, string, error) {
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
		listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", parsed.Path)
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
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", parsed.Host)
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

func (s *Service) shutdown(ctx context.Context, cancelBackground context.CancelFunc) error {
	defer func() { _ = s.stateLock.Close() }()
	// Background loops write the state directory (update apply, policy sweeps,
	// multipart cleanup), so they are canceled first and must return before
	// the deferred state lock release above: a restart racing the dying
	// process must never observe the lock free while a loop is mid-write.
	if cancelBackground != nil {
		cancelBackground()
	}
	s.serversMu.Lock()
	servers := append([]*http.Server(nil), s.servers...)
	s.serversMu.Unlock()
	var combined error
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil {
			combined = errors.Join(combined, err)
		}
	}
	if err := s.backgroundLoops.wait(ctx); err != nil {
		s.logger.Warn("shutdown deadline reached with background loops still running", "loops", s.backgroundLoops.pending())
		combined = errors.Join(combined, err)
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
