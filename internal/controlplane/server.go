package controlplane

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/observability"
)

// server.go holds the Server type, its construction, the listener lifecycle,
// the route table, and the health/readiness endpoints. Handlers live in the
// *_routes.go files beside it; middleware and authentication in middleware.go;
// request/response plumbing in http.go and responses.go.

type Server struct {
	config    config.ControlPlane
	logger    *slog.Logger
	metrics   *observability.Metrics
	database  *database.Store
	limiter   *RateLimiter
	stripe    *billing.StripeClient
	startedAt time.Time
	// storage brokers per-org credentials for the hosted checkpoint store and
	// reconciles usage from the bucket. It is nil when hosting is off, which
	// every storage route treats as STORAGE_NOT_CONFIGURED.
	storage *storageService
	// oidcHTTPClient overrides the outbound client used for OIDC discovery,
	// token exchange, and key fetches. It exists for tests, which point it at
	// a locally issued TLS certificate; production leaves it nil and gets the
	// default client.
	oidcHTTPClient *http.Client
	// trustedProxies is the parsed trusted-proxy list: forwarding headers are
	// honored only when the socket peer matches one of these.
	trustedProxies []net.IPNet
	// metricsLimiter gives scrapes their own budget so a burst of API traffic
	// cannot starve monitoring (and vice versa).
	metricsLimiter *RateLimiter
	// loginLimiter throttles credential attempts per client+account, so one
	// account cannot be brute-forced through a shared NAT address alone.
	loginLimiter *RateLimiter
	// apiBudgets enforces the per-plan API request budgets (daily budget +
	// hourly burst) per organization.
	apiBudgets *apiBudget
	tracer     *observability.Tracer
	// schemaStatus reports the background schema-migration state: "pending",
	// "applied", or "failed". Readiness reports it separately from database
	// availability so a replica waiting out a slow migration says exactly that.
	schemaStatus   atomic.Value
	migrateOnStart bool
	// sweepMu makes the retention/housekeeping pass single-flight in-process;
	// across replicas the database-side advisory lock takes that role.
	sweepMu sync.Mutex
	// backgroundWg tracks the background workers Run starts (schema apply,
	// retention sweep, storage/Stripe reconcilers, migration worker) so
	// shutdown can drain them before the database pool closes underneath.
	backgroundWg sync.WaitGroup
	// commandClaimLease bounds how long an agent may hold a claimed command
	// unsettled before the claim lease requeues it for redelivery.
	commandClaimLease time.Duration
	// unmanagedMigrations tracks non-managed migration jobs the settlement
	// pass must follow to a terminal state (dispatch and relay routes).
	settleMu            sync.Mutex
	unmanagedMigrations map[string]unmanagedMigrationWatch
}

const (
	schemaPending = "pending"
	schemaApplied = "applied"
	schemaFailed  = "failed"
)

func Open(ctx context.Context, configuration config.ControlPlane, logger *slog.Logger) (*Server, error) {
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = observability.NewLogger(configuration.LogLevel)
	}
	store, err := database.Open(ctx, configuration.DatabaseURL)
	if err != nil {
		return nil, err
	}
	server := New(configuration, store, logger)
	if configuration.Storage.Enabled && server.storage == nil {
		store.Close()
		return nil, errors.New("hosted storage was requested but the broker could not be constructed")
	}
	// Schema migrations run in the background once Run starts, not before the
	// listener opens: a rolling deploy must not hold every replica unready
	// behind one long index build on the advisory lock. Readiness reports the
	// migration state separately until the schema is applied.
	server.migrateOnStart = true
	server.schemaStatus.Store(schemaPending)
	return server, nil
}

func New(configuration config.ControlPlane, store *database.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	tracer := observability.NewTracer(logger)
	if configuration.Tracing.Enabled() {
		exporter := observability.NewOTLPExporter(logger, configuration.Tracing.OTLPEndpoint, tracingServiceName(configuration), config.Version)
		tracer = observability.NewTracer(logger, exporter)
	}
	server := &Server{
		config: configuration, database: store, logger: logger,
		metrics: observability.NewMetrics(), limiter: NewRateLimiter(configuration.RateLimitPerMin),
		metricsLimiter: NewRateLimiter(120), loginLimiter: NewRateLimiter(10), apiBudgets: newAPIBudget(),
		stripe: billing.NewStripeClient(configuration.StripeSecretKey), startedAt: time.Now().UTC(),
		tracer: tracer, commandClaimLease: defaultCommandClaimLease,
		unmanagedMigrations: map[string]unmanagedMigrationWatch{},
	}
	// New hands out a server over an already-prepared store; only Open defers
	// schema migration to the background (see runSchemaMigration), so New
	// starts "applied" and Open downgrades it to "pending".
	server.schemaStatus.Store(schemaApplied)
	server.trustedProxies = parseTrustedProxies(configuration.TrustedProxies)
	storage, err := newStorageService(configuration.Storage)
	if err != nil {
		// New cannot fail, so a broker that could not be built leaves hosting
		// off for this in-process server with an error logged; Open — the
		// production constructor — refuses to start instead.
		logger.Error("hosted storage broker construction failed; hosting is off", "error", err)
	}
	server.storage = storage
	server.registerRuntimeGauges()
	return server
}

func tracingServiceName(configuration config.ControlPlane) string {
	if strings.TrimSpace(configuration.Tracing.ServiceName) != "" {
		return configuration.Tracing.ServiceName
	}
	return "shift-control"
}

// Handler returns the fully instrumented HTTP handler. It is useful for
// embedding the control plane behind an existing listener and for integration
// tests; Run remains the default production listener lifecycle.
func (server *Server) Handler() http.Handler {
	return server.instrument(server.handler())
}

// instrument stacks the middleware every serving path shares: request
// metrics, tracing, security headers, and CORS. Security headers sit outside
// CORS so the short-circuited responses (preflight answers, origin refusals)
// carry them too.
func (server *Server) instrument(next http.Handler) http.Handler {
	return server.metrics.Middleware(server.logger, observability.TraceMiddleware(server.tracer, server.securityHeaders(server.cors(next))))
}

func (server *Server) Close() {
	if server.database != nil {
		server.database.Close()
	}
}

func (server *Server) Run(ctx context.Context) error {
	// WriteTimeout is deliberately more generous than RequestTimeout: a slow
	// client downloading a large list response must not have the body cut off
	// mid-stream by the same budget that bounds request processing.
	httpServer := &http.Server{
		Addr:              server.config.Listen,
		Handler:           server.instrument(server.handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       server.config.RequestTimeout,
		WriteTimeout:      2 * server.config.RequestTimeout,
		IdleTimeout:       90 * time.Second,
	}
	errorsChannel := make(chan error, 1)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.config.Listen)
	if err != nil {
		return fmt.Errorf("listen control plane on %s: %w", server.config.Listen, err)
	}
	if server.migrateOnStart {
		// The schema apply is detached from the signal context: a SIGTERM
		// mid-migration must not abort an apply the shutdown drain below is
		// about to wait for. It is one-shot and transactional, and the drain
		// is bounded, so exit stays prompt either way.
		server.backgroundWg.Add(1)
		go func() {
			defer server.backgroundWg.Done()
			server.runSchemaMigration(context.WithoutCancel(ctx))
		}()
	}
	// Retention enforcement runs alongside request serving: a policy that only
	// an operator could trigger would let expired records outlive their window
	// for as long as nobody remembered to sweep.
	server.startRetentionSweeper(ctx)
	// Hosted-storage usage is recounted on the same principle: the quota the
	// credential broker enforces must track the bucket without an operator
	// remembering to ask.
	server.startStorageReconciler(ctx)
	// Stripe↔entitlement drift converges daily (plan 4 BR-12): webhook
	// backlog re-drive plus per-org subscription truth, audited.
	server.startStripeReconciler(ctx)
	server.backgroundWg.Add(1)
	go func() {
		defer server.backgroundWg.Done()
		server.runMigrationWorker(ctx)
	}()
	go func() { errorsChannel <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
		// In-flight requests may legitimately run up to RequestTimeout; the
		// shutdown deadline has to cover them or a rollout cuts off slow but
		// healthy work with 5xx.
		deadline := server.config.RequestTimeout
		if deadline < 30*time.Second {
			deadline = 30 * time.Second
		}
		shutdownContext, cancel := context.WithTimeout(context.Background(), deadline)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownContext)
		// Drain the background workers before returning: the caller closes
		// the database pool the moment Run returns, and a worker mid-query
		// against a closed pool — an in-flight migration job's writes, a
		// schema apply mid-file — is graceful shutdown in name only. The wait
		// is bounded so a wedged sweep cannot block process exit forever.
		if !awaitWorkers(&server.backgroundWg, backgroundDrainTimeout) {
			server.logger.Warn("background workers did not drain before the shutdown deadline")
		}
		return shutdownErr
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// backgroundDrainTimeout bounds how long shutdown waits for the background
// workers to finish in-flight work after the HTTP listener has stopped.
const backgroundDrainTimeout = 30 * time.Second

// awaitWorkers waits for the background WaitGroup with a bound and reports
// whether every worker drained in time.
func awaitWorkers(workers *sync.WaitGroup, timeout time.Duration) bool {
	drained := make(chan struct{})
	go func() {
		workers.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		return true
	case <-time.After(timeout):
		return false
	}
}

// runSchemaMigration applies pending migrations on a background goroutine and
// records the outcome for readiness. Failures leave the process serving: the
// next restart (or an operator-triggered retry) attempts again.
func (server *Server) runSchemaMigration(ctx context.Context) {
	if err := server.database.Migrate(ctx); err != nil {
		server.logger.Warn("schema migration failed; readiness reports the schema as not applied", schemaMigrationFailureAttrs(err)...)
		server.schemaStatus.Store(schemaFailed)
		return
	}
	server.schemaStatus.Store(schemaApplied)
}

// schemaMigrationFailureAttrs classifies the error as before and carries the
// full cause — the failing migration file name and the underlying database
// error. Unlike the probe logs errorClass protects, this is a one-shot
// startup event whose whole value is the cause: an operator must be able to
// tell whether 015_billing_v2 failed on a lock, a syntax edge, or a
// constraint without attaching a debugger to a replica that is still serving.
func schemaMigrationFailureAttrs(err error) []any {
	return []any{"error_class", errorClass(err), "cause", err.Error()}
}

// errorClass strips a database error down to a safe classification for probe
// logs: the DSN fragments and SQL a pgx error can carry never reach the log at
// probe frequency.
func errorClass(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "context_done"
	}
	var netError net.Error
	if errors.As(err, &netError) {
		return "network"
	}
	return "database"
}

// parseTrustedProxies turns configured CIDRs and single addresses into match
// rules. A malformed entry is dropped rather than trusted.
func parseTrustedProxies(entries []string) []net.IPNet {
	var networks []net.IPNet
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "/") {
			if _, network, err := net.ParseCIDR(trimmed); err == nil {
				networks = append(networks, *network)
			}
			continue
		}
		if ip := net.ParseIP(trimmed); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			networks = append(networks, net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return networks
}

func (server *Server) isTrustedProxy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for index := range server.trustedProxies {
		if server.trustedProxies[index].Contains(ip) {
			return true
		}
	}
	return false
}

// isProbePath reports whether a request is a kubelet-style health probe or a
// metrics scrape. Probes must never share the API's rate budget: a throttled
// liveness probe restarts the pod exactly when the system is busiest.
func isProbePath(path string) bool {
	return path == "/health" || path == "/ready"
}

func (server *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.handleHealth)
	mux.HandleFunc("GET /ready", server.handleReady)
	mux.Handle("GET /metrics", server.metrics.Handler())
	mux.HandleFunc("POST /v1/auth/register", server.handleRegister)
	mux.HandleFunc("POST /v1/auth/login", server.handleLogin)
	mux.HandleFunc("POST /v1/auth/refresh", server.handleRefresh)
	mux.HandleFunc("POST /v1/auth/sso/authorize", server.handleSSOAuthorize)
	mux.HandleFunc("POST /v1/auth/sso/token", server.handleSSOToken)
	mux.HandleFunc("GET /v1/auth/sso/required", server.handleSSORequired)
	mux.HandleFunc("POST /v1/auth/github/authorize", server.handleGitHubAuthorize)
	mux.HandleFunc("GET /v1/auth/github/callback", server.handleGitHubCallback)
	mux.HandleFunc("POST /v1/auth/github/token", server.handleGitHubToken)
	// Logout is reachable without a bearer token so a browser whose access
	// token has expired can still revoke its session via the httpOnly shift_rt
	// cookie; the handler authenticates from the bearer or that cookie.
	mux.HandleFunc("POST /v1/auth/logout", server.handleLogout)
	mux.HandleFunc("GET /v1/me", server.requireAuth(server.handleMe))
	mux.HandleFunc("DELETE /v1/users/me", server.requireAuth(server.handleUserErase))
	mux.HandleFunc("GET /v1/organizations", server.requireAuth(server.handleOrganizations))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/members", server.requireOrganization(RoleAdmin, "administration", server.handleMemberAdd))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/relay/tickets", server.requireOrganization(RoleOperator, "migrations", server.handleRelayTicket))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/ownership/acquire", server.requireOrganization(RoleOperator, "machines", server.handleOwnershipAcquire))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/ownership/fence", server.requireOrganization(RoleOperator, "machines", server.handleOwnershipFence))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/machines", server.requireOrganization(RoleViewer, "machines", server.handleMachineList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/machines", server.requireOrganization(RoleOperator, "machines", server.handleMachineCreate))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/machines/capability", server.requireOrganization(RoleViewer, "machines", server.handleMachineCapability))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/machines/{machineID}/heartbeat", server.requireOrganization(RoleOperator, "machines", server.handleMachineHeartbeat))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/workloads", server.requireOrganization(RoleViewer, "workloads", server.handleWorkloadList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/workloads", server.requireOrganization(RoleOperator, "workloads", server.handleWorkloadCreate))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/workloads/{workloadID}/status", server.requireOrganization(RoleOperator, "workloads", server.handleWorkloadStatus))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/migrations", server.requireOrganization(RoleViewer, "migrations", server.handleMigrationList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/migrations", server.requireOrganization(RoleOperator, "migrations", server.handleMigrationCreate))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/audit", server.requireOrganization(RoleAdmin, "administration", server.handleAuditList))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/retention", server.requireOrganization(RoleAdmin, "administration", server.handleRetentionGet))
	mux.HandleFunc("PUT /v1/organizations/{organizationID}/retention", server.requireOrganization(RoleAdmin, "administration", server.handleRetentionSet))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/sso", server.requireOrganization(RoleAdmin, "administration", server.handleOrganizationSSOGet))
	mux.HandleFunc("PUT /v1/organizations/{organizationID}/sso", server.requireOrganization(RoleAdmin, "administration", server.handleOrganizationSSOSet))
	// SCIM v2 provisioning. The tenant is the API key's own organization, so
	// these paths carry no organization id.
	mux.HandleFunc("GET /v1/scim/v2/ServiceProviderConfig", server.requireSCIM(server.handleSCIMServiceProviderConfig))
	mux.HandleFunc("GET /v1/scim/v2/ResourceTypes", server.requireSCIM(server.handleSCIMResourceTypes))
	mux.HandleFunc("GET /v1/scim/v2/Schemas", server.requireSCIM(server.handleSCIMSchemas))
	mux.HandleFunc("GET /v1/scim/v2/Users", server.requireSCIM(server.handleSCIMUsersList))
	mux.HandleFunc("POST /v1/scim/v2/Users", server.requireSCIM(server.handleSCIMUserCreate))
	mux.HandleFunc("GET /v1/scim/v2/Users/{userID}", server.requireSCIM(server.handleSCIMUserGet))
	mux.HandleFunc("PUT /v1/scim/v2/Users/{userID}", server.requireSCIM(server.handleSCIMUserReplace))
	mux.HandleFunc("PATCH /v1/scim/v2/Users/{userID}", server.requireSCIM(server.handleSCIMUserPatch))
	mux.HandleFunc("DELETE /v1/scim/v2/Users/{userID}", server.requireSCIM(server.handleSCIMUserDelete))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/entitlement", server.requireOrganization(RoleViewer, "billing", server.handleEntitlement))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/api-keys", server.requireOrganization(RoleAdmin, "administration", server.handleAPIKeyList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/api-keys", server.requireOrganization(RoleAdmin, "administration", server.handleAPIKeyCreate))
	mux.HandleFunc("DELETE /v1/organizations/{organizationID}/api-keys/{keyID}", server.requireOrganization(RoleAdmin, "administration", server.handleAPIKeyRevoke))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/checkpoints", server.requireOrganization(RoleViewer, "checkpoints", server.handleCheckpointList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/checkpoints", server.requireOrganization(RoleOperator, "checkpoints", server.handleCheckpointCreate))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/migrations/{migrationID}/events", server.requireOrganization(RoleViewer, "migrations", server.handleMigrationEvents))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/migrations/{migrationID}/cancel", server.requireOrganization(RoleOperator, "migrations", server.handleMigrationCancel))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/machines/{machineID}/commands", server.requireOrganization(RoleOperator, "workloads", server.handleAgentCommand))
	// Agent command delivery: the machine's own reporter polls for queued
	// commands and reports completion. Both handlers additionally require a
	// machine-bound API key (its name or scope pins it to one machine), so
	// neither a user session nor another machine's key can drain this
	// machine's queue or finalize its commands.
	mux.HandleFunc("GET /v1/organizations/{organizationID}/machines/{machineID}/commands/poll", server.requireOrganization(RoleOperator, "machines", server.handleAgentCommandPoll))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/machines/{machineID}/commands/{commandID}/complete", server.requireOrganization(RoleOperator, "machines", server.handleAgentCommandComplete))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/reconcile", server.requireOrganization(RoleOperator, "workloads", server.handleReconcile))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/usage", server.requireOrganization(RoleViewer, "billing", server.handleUsage))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/usage/summary", server.requireOrganization(RoleAdmin, "billing", server.handleUsageSummary))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/trial", server.requireOrganization(RoleAdmin, "billing", server.handleTrialStart))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/trial", server.requireOrganization(RoleViewer, "billing", server.handleTrialStatus))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/referral", server.requireOrganization(RoleViewer, "billing", server.handleReferralStats))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/referral/claim", server.requireOrganization(RoleAdmin, "billing", server.handleReferralClaim))
	mux.HandleFunc("GET /v1/public/stats", server.handlePublicStats)
	mux.HandleFunc("GET /v1/admin/scoreboard", server.requireAuth(server.handleScoreboard))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/storage", server.requireOrganization(RoleViewer, "billing", server.handleStorageStatus))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/storage/credentials", server.requireOrganization(RoleOperator, "machines", server.handleStorageCredentials))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/storage/reconcile", server.requireOrganization(RoleAdmin, "billing", server.handleStorageReconcile))
	mux.HandleFunc("GET /v1/plans", server.handlePlans)
	mux.HandleFunc("POST /v1/organizations/{organizationID}/billing/checkout", server.requireOrganization(RoleAdmin, "billing", server.handleBillingCheckout))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/billing/portal", server.requireOrganization(RoleAdmin, "billing", server.handleBillingPortal))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/compute/offers", server.requireOrganization(RoleViewer, "compute", server.handleComputeOfferList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/compute/offers", server.requireOrganization(RoleOperator, "compute", server.handleComputeOfferPublish))
	mux.HandleFunc("DELETE /v1/organizations/{organizationID}/compute/offers/{offerID}", server.requireOrganization(RoleOperator, "compute", server.handleComputeOfferWithdraw))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/compute/inventory", server.requireOrganization(RoleViewer, "compute", server.handleComputeInventory))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/compute/placements", server.requireOrganization(RoleOperator, "compute", server.handleComputePlacement))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/compute/reservations", server.requireOrganization(RoleViewer, "compute", server.handleComputeReservationList))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/compute/reservations", server.requireOrganization(RoleOperator, "compute", server.handleComputeReservationCreate))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/compute/reservations/{reservationID}/state", server.requireOrganization(RoleOperator, "compute", server.handleComputeReservationState))
	mux.HandleFunc("POST /v1/webhooks/stripe", server.handleStripeWebhook)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path := request.URL.Path
		switch {
		case request.Method == http.MethodGet && isProbePath(path):
			// Health probes and readiness checks bypass rate limiting
			// entirely: they must answer even when real traffic is throttled.
		case request.Method == http.MethodGet && path == "/metrics":
			if !server.authorizeMetrics(writer, request) {
				return
			}
			if !server.metricsLimiter.Allow(server.rateLimitKey(request)) {
				writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "request rate limit exceeded")
				return
			}
		default:
			if !server.limiter.Allow(server.rateLimitKey(request)) {
				writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "request rate limit exceeded")
				return
			}
		}
		requestContext, cancel := context.WithTimeout(request.Context(), server.config.RequestTimeout)
		defer cancel()
		mux.ServeHTTP(writer, request.WithContext(requestContext))
	})
}

// authorizeMetrics gates the scrape endpoint. An explicit token is required
// in every deployment; an empty token fails closed so a configuration omission
// cannot expose operational and business metrics.
func (server *Server) authorizeMetrics(writer http.ResponseWriter, request *http.Request) bool {
	expected := strings.TrimSpace(server.config.MetricsToken)
	if expected == "" {
		writeError(writer, http.StatusUnauthorized, "METRICS_UNAUTHORIZED", "metrics authentication is not configured")
		return false
	}
	presented := strings.TrimSpace(request.Header.Get("X-Metrics-Token"))
	if token, ok := parseBearer(request.Header.Get("Authorization")); ok {
		presented = token
	}
	if len(presented) == len(expected) && subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1 {
		return true
	}
	writeError(writer, http.StatusUnauthorized, "METRICS_UNAUTHORIZED", "a valid metrics scrape token is required")
	return false
}

func (server *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": config.Version, "started_at": server.startedAt, "uptime": time.Since(server.startedAt).Round(time.Second).String()})
}

// handleReady reports database availability and schema-migration state
// separately: "migrations" is its own field (and its own failure code) so an
// operator can tell a database outage from a replica still catching up on
// schema changes. Probe failures log a classified error at warn — never the
// raw database error, which can carry DSN fragments and would repeat on every
// probe interval.
func (server *Server) handleReady(writer http.ResponseWriter, request *http.Request) {
	if err := server.database.Ping(request.Context()); err != nil {
		server.logger.Warn("control-plane readiness probe failed", "error_class", errorClass(err))
		writeError(writer, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database is unavailable")
		return
	}
	status, _ := server.schemaStatus.Load().(string)
	switch status {
	case schemaApplied:
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ready", "migrations": schemaApplied})
	case schemaFailed:
		server.logger.Warn("control-plane readiness reports failed schema migration")
		writeError(writer, http.StatusServiceUnavailable, "SCHEMA_MIGRATION_FAILED", "schema migration failed; see operator runbook")
	default:
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "migrations": schemaPending})
	}
}

// registerRuntimeGauges publishes pool saturation and the managed-migration
// queue on /metrics so saturation and queue backup are visible before they
// become failures.
func (server *Server) registerRuntimeGauges() {
	server.metrics.RegisterGauge("db_pool_max_conns", "maximum database pool connections", func() float64 {
		return float64(server.database.PoolStats().MaxConns)
	})
	server.metrics.RegisterGauge("db_pool_total_conns", "total database pool connections", func() float64 {
		return float64(server.database.PoolStats().TotalConns)
	})
	server.metrics.RegisterGauge("db_pool_idle_conns", "idle database pool connections", func() float64 {
		return float64(server.database.PoolStats().IdleConns)
	})
	server.metrics.RegisterGauge("db_pool_acquire_count", "cumulative database pool acquires", func() float64 {
		return float64(server.database.PoolStats().AcquireCount)
	})
	server.metrics.RegisterGauge("db_pool_acquire_seconds_total", "cumulative seconds spent waiting for a database connection", func() float64 {
		return server.database.PoolStats().AcquireDuration.Seconds()
	})
	server.metrics.RegisterGauge("migration_queue_depth", "managed migration jobs queued or running", func() float64 {
		depth, _, err := server.database.ManagedMigrationQueueStats(context.Background())
		if err != nil {
			return 0
		}
		return float64(depth)
	})
	server.metrics.RegisterGauge("migration_oldest_queued_seconds", "age of the oldest queued managed migration job", func() float64 {
		_, oldest, err := server.database.ManagedMigrationQueueStats(context.Background())
		if err != nil {
			return 0
		}
		return oldest
	})
	server.metrics.RegisterGauge("schema_migration_failed", "1 when the background schema migration failed on this replica", func() float64 {
		if status, _ := server.schemaStatus.Load().(string); status == schemaFailed {
			return 1
		}
		return 0
	})
	server.metrics.RegisterGauge("machine_presence_lag_seconds", "longest heartbeat silence among non-disabled machines", func() float64 {
		lag, err := server.database.MachinePresenceLag(context.Background())
		if err != nil {
			return 0
		}
		return lag
	})
	// Entitlement drift and webhook backlog (plan 4 BR-12c/KPI-10/KPI-12):
	// both must read 0; the daily reconciliation job drives them there.
	server.metrics.RegisterGauge("shift_entitlement_drift_rows", "subscribed orgs whose plan/status no longer matches Stripe truth", func() float64 {
		rows, err := server.database.EntitlementDriftRows(context.Background())
		if err != nil {
			return 0
		}
		return float64(rows)
	})
	server.metrics.RegisterGauge("shift_webhook_stale_events", "unprocessed Stripe events older than 24 hours", func() float64 {
		count, err := server.database.StaleStripeEventBacklog(context.Background(), time.Now().UTC().Add(-24*time.Hour))
		if err != nil {
			return 0
		}
		return float64(count)
	})
}
