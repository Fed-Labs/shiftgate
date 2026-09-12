package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
}

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
	if err := store.Migrate(ctx); err != nil {
		store.Close()
		return nil, err
	}
	server := New(configuration, store, logger)
	if configuration.Storage.Enabled && server.storage == nil {
		store.Close()
		return nil, errors.New("hosted storage was requested but the broker could not be constructed")
	}
	return server, nil
}

func New(configuration config.ControlPlane, store *database.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{config: configuration, database: store, logger: logger, metrics: observability.NewMetrics(), limiter: NewRateLimiter(configuration.RateLimitPerMin), stripe: billing.NewStripeClient(configuration.StripeSecretKey), startedAt: time.Now().UTC()}
	storage, err := newStorageService(configuration.Storage)
	if err != nil {
		// New cannot fail, so a broker that could not be built leaves hosting
		// off for this in-process server with an error logged; Open — the
		// production constructor — refuses to start instead.
		logger.Error("hosted storage broker construction failed; hosting is off", "error", err)
	}
	server.storage = storage
	return server
}

// Handler returns the fully instrumented HTTP handler. It is useful for
// embedding the control plane behind an existing listener and for integration
// tests; Run remains the default production listener lifecycle.
func (server *Server) Handler() http.Handler {
	return server.metrics.Middleware(server.logger, server.cors(server.handler()))
}

func (server *Server) Close() {
	if server.database != nil {
		server.database.Close()
	}
}

func (server *Server) Run(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              server.config.Listen,
		Handler:           server.metrics.Middleware(server.logger, server.cors(server.handler())),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       server.config.RequestTimeout,
		WriteTimeout:      server.config.RequestTimeout,
		IdleTimeout:       90 * time.Second,
	}
	errorsChannel := make(chan error, 1)
	listener, err := net.Listen("tcp", server.config.Listen)
	if err != nil {
		return fmt.Errorf("listen control plane on %s: %w", server.config.Listen, err)
	}
	// Retention enforcement runs alongside request serving: a policy that only
	// an operator could trigger would let expired records outlive their window
	// for as long as nobody remembered to sweep.
	server.startRetentionSweeper(ctx)
	// Hosted-storage usage is recounted on the same principle: the quota the
	// credential broker enforces must track the bucket without an operator
	// remembering to ask.
	server.startStorageReconciler(ctx)
	go func() { errorsChannel <- httpServer.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownContext)
	case err := <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
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
	mux.HandleFunc("POST /v1/auth/logout", server.requireAuth(server.handleLogout))
	mux.HandleFunc("GET /v1/me", server.requireAuth(server.handleMe))
	mux.HandleFunc("GET /v1/organizations", server.requireAuth(server.handleOrganizations))
	mux.HandleFunc("POST /v1/organizations/{organizationID}/members", server.requireOrganization(RoleAdmin, "administration", server.handleMemberAdd))
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
	mux.HandleFunc("POST /v1/organizations/{organizationID}/reconcile", server.requireOrganization(RoleOperator, "workloads", server.handleReconcile))
	mux.HandleFunc("GET /v1/organizations/{organizationID}/usage", server.requireOrganization(RoleViewer, "billing", server.handleUsage))
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
		if !server.limiter.Allow(rateLimitKey(request)) {
			writeError(writer, http.StatusTooManyRequests, "RATE_LIMITED", "request rate limit exceeded")
			return
		}
		requestContext, cancel := context.WithTimeout(request.Context(), server.config.RequestTimeout)
		defer cancel()
		mux.ServeHTTP(writer, request.WithContext(requestContext))
	})
}

func (server *Server) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "version": config.Version, "started_at": server.startedAt, "uptime": time.Since(server.startedAt).Round(time.Second).String()})
}

func (server *Server) handleReady(writer http.ResponseWriter, request *http.Request) {
	if err := server.database.Ping(request.Context()); err != nil {
		server.logger.Error("control-plane readiness failed", "error", err)
		writeError(writer, http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database is unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ready"})
}
