package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/objectstore"
	"shift.dev/shift/internal/persistence"
)

const Version = "0.1.4"

// AgentConfigVersion is the on-disk agent configuration schema this build reads
// and writes. It is compared against a release's config schema so an update that
// could not read this machine's configuration is never installed.
const AgentConfigVersion = model.ConfigSchemaVersion

type TLSConfig struct {
	CertificateFile string `json:"certificate_file"`
	PrivateKeyFile  string `json:"private_key_file"`
	ClientCAFile    string `json:"client_ca_file"`
	PeerCAFile      string `json:"peer_ca_file,omitempty"`
	ServerName      string `json:"server_name,omitempty"`
}

type Agent struct {
	Version  int    `json:"version"`
	StateDir string `json:"state_dir"`
	Listen   string `json:"listen"`
	// SocketGroup names the group that owns the Unix socket when Listen is a
	// unix:// path, so unprivileged users in that group can reach the local
	// agent. Empty leaves the socket owned by the agent's own group.
	SocketGroup             string             `json:"socket_group,omitempty"`
	RemoteListen            string             `json:"remote_listen,omitempty"`
	TLS                     TLSConfig          `json:"tls"`
	InsecureDevelopment     bool               `json:"insecure_development"`
	ChunkSizeBytes          int                `json:"chunk_size_bytes"`
	MaxConcurrentMigrations int                `json:"max_concurrent_migrations"`
	ShutdownTimeout         time.Duration      `json:"shutdown_timeout"`
	LogLevel                string             `json:"log_level"`
	AllowedOrigins          []string           `json:"allowed_origins,omitempty"`
	ObjectStore             objectstore.Config `json:"object_store"`
	ControlPlane            ControlReporter    `json:"control_plane"`
	Updates                 Updates            `json:"updates"`
	Tracing                 Tracing            `json:"tracing"`

	// CgroupRoot is the directory the agent places workload cgroups under.
	// Empty means /sys/fs/cgroup/shift. Two agents on one machine must use
	// distinct roots — a workload's cgroup path is derived from its id, so a
	// shared root puts both agents' trees for the same workload in one
	// directory, and the migration the second agent restores lands in the
	// cgroup the first is about to tear down.
	CgroupRoot string `json:"cgroup_root,omitempty"`

	objectStoreLocalRootExplicit bool
	objectStoreStateDirExplicit  bool
}

// DefaultControlPlaneURL is the control plane every SHIFT client talks to
// unless told otherwise: the platform hosts the control plane, so its address
// is part of the product, not per-deployment configuration — the same way a
// hosted service's SDK ships its endpoint. A private deployment overrides it
// with --control-url / SHIFT_CONTROL_URL (CLI, agents) or NEXT_PUBLIC_API_URL
// (dashboard).
const DefaultControlPlaneURL = "https://shiftgate.dev"

type ControlReporter struct {
	URL            string        `json:"url,omitempty"`
	OrganizationID string        `json:"organization_id,omitempty"`
	MachineID      string        `json:"machine_id,omitempty"`
	MachineName    string        `json:"machine_name,omitempty"`
	AgentURL       string        `json:"agent_url,omitempty"`
	APIKey         string        `json:"api_key,omitempty"`
	Interval       time.Duration `json:"interval"`
	RequestTimeout time.Duration `json:"request_timeout"`
}

// Tracing configures distributed tracing. Spans always go to the structured
// log; setting an OTLP endpoint additionally ships them to a collector.
type Tracing struct {
	// OTLPEndpoint is an OTLP/HTTP JSON traces endpoint, for example
	// "https://collector:4318/v1/traces". Empty disables remote export.
	OTLPEndpoint string `json:"otlp_endpoint,omitempty"`
	// ServiceName labels emitted spans; defaults to the agent's machine id.
	ServiceName string `json:"service_name,omitempty"`
}

func (t Tracing) Enabled() bool {
	return strings.TrimSpace(t.OTLPEndpoint) != ""
}

func (t *Tracing) Validate() error {
	if !t.Enabled() {
		return nil
	}
	parsed, err := url.Parse(t.OTLPEndpoint)
	if err != nil || parsed.Host == "" {
		return errors.New("otlp_endpoint must be a full http:// or https:// URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("otlp_endpoint must use http:// or https://")
	}
	return nil
}

func DefaultAgent() Agent {
	return Agent{
		Version:                 AgentConfigVersion,
		StateDir:                "/var/lib/shift",
		Listen:                  "unix:///run/shift/agent.sock",
		SocketGroup:             "shift",
		ChunkSizeBytes:          4 << 20,
		MaxConcurrentMigrations: 2,
		ShutdownTimeout:         20 * time.Second,
		LogLevel:                "info",
		ObjectStore:             objectstore.DefaultConfig("/var/lib/shift"),
		Updates:                 DefaultUpdates(),
	}
}

func (c *Agent) Validate() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("shift-agent is implemented for Linux; detected %s", runtime.GOOS)
	}
	if c.Version != AgentConfigVersion {
		return fmt.Errorf("unsupported agent config version %d", c.Version)
	}
	if c.StateDir == "" || !filepath.IsAbs(c.StateDir) {
		return errors.New("state_dir must be an absolute path")
	}
	if c.CgroupRoot != "" {
		if !filepath.IsAbs(c.CgroupRoot) {
			return errors.New("cgroup_root must be an absolute path")
		}
		if cleaned := filepath.Clean(c.CgroupRoot); cleaned != c.CgroupRoot || cleaned == "/" || strings.Contains(cleaned, "..") {
			return errors.New("cgroup_root must be a cleaned path under a cgroup mount, not / or a relative traversal")
		}
	}
	if c.ChunkSizeBytes < 64<<10 || c.ChunkSizeBytes > 64<<20 {
		return errors.New("chunk_size_bytes must be between 64 KiB and 64 MiB")
	}
	if c.MaxConcurrentMigrations < 1 || c.MaxConcurrentMigrations > 32 {
		return errors.New("max_concurrent_migrations must be between 1 and 32")
	}
	if err := c.ControlPlane.Validate(); err != nil {
		return fmt.Errorf("control_plane: %w", err)
	}
	if err := c.ObjectStore.Validate(); err != nil {
		return fmt.Errorf("object_store: %w", err)
	}
	if err := c.Updates.Validate(c.StateDir); err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	if err := c.Tracing.Validate(); err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	parsed, err := url.Parse(c.Listen)
	if err != nil {
		return fmt.Errorf("parse listen URL: %w", err)
	}
	switch parsed.Scheme {
	case "unix":
		if parsed.Path == "" {
			return errors.New("unix listener path is required")
		}
	case "tcp":
		if parsed.Host == "" {
			return errors.New("tcp listener address is required")
		}
		if !c.InsecureDevelopment {
			return errors.New("the local control API must use a Unix socket in production; configure remote_listen for peer traffic")
		}
		if !c.InsecureDevelopment && (c.TLS.CertificateFile == "" || c.TLS.PrivateKeyFile == "" || c.TLS.ClientCAFile == "") {
			return errors.New("remote TCP listeners require a server certificate, private key, and client CA")
		}
	default:
		return fmt.Errorf("unsupported listener scheme %q", parsed.Scheme)
	}
	if c.RemoteListen != "" {
		remote, err := url.Parse(c.RemoteListen)
		if err != nil || remote.Scheme != "tcp" || remote.Host == "" {
			return errors.New("remote_listen must be a tcp:// address")
		}
		if !c.InsecureDevelopment && (c.TLS.CertificateFile == "" || c.TLS.PrivateKeyFile == "" || c.TLS.ClientCAFile == "") {
			return errors.New("remote TCP listeners require mutual TLS certificate, key, and client CA")
		}
	}
	return nil
}

func (c ControlReporter) Enabled() bool {
	return strings.TrimSpace(c.URL) != "" || strings.TrimSpace(c.OrganizationID) != "" || strings.TrimSpace(c.MachineID) != "" || strings.TrimSpace(c.APIKey) != ""
}

func (c *ControlReporter) Validate() error {
	if !c.Enabled() {
		c.Interval = 30 * time.Second
		c.RequestTimeout = 5 * time.Second
		return nil
	}
	if strings.TrimSpace(c.URL) == "" || strings.TrimSpace(c.OrganizationID) == "" || strings.TrimSpace(c.APIKey) == "" || strings.TrimSpace(c.MachineID) == "" {
		return errors.New("url, organization_id, machine_id, and api_key are required when the control-plane reporter is enabled")
	}
	if _, err := url.Parse(c.URL); err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
		return errors.New("URL must use http:// or https://")
	}
	if len(c.APIKey) < 20 || len(c.APIKey) > 256 {
		return errors.New("API key length is invalid")
	}
	if c.Interval <= time.Second {
		c.Interval = 30 * time.Second
	}
	if c.Interval > 24*time.Hour {
		return errors.New("interval must be no longer than 24 hours")
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 5 * time.Second
	}
	if c.RequestTimeout > time.Minute {
		return errors.New("request_timeout must be no longer than one minute")
	}
	return nil
}

// SetStateDir applies a state-directory override while keeping inherited
// object-store paths under the same root. Paths explicitly supplied by a
// configuration file or environment variable are preserved.
func (c *Agent) SetStateDir(stateDir string) {
	c.StateDir = stateDir
	defaults := objectstore.DefaultConfig(stateDir)
	if !c.objectStoreLocalRootExplicit {
		c.ObjectStore.LocalRoot = defaults.LocalRoot
	}
	if !c.objectStoreStateDirExplicit {
		c.ObjectStore.StateDir = defaults.StateDir
	}
}

func LoadAgent(path string) (Agent, error) {
	configuration := DefaultAgent()
	if path != "" {
		if err := persistence.ReadJSON(path, &configuration); err != nil {
			return Agent{}, err
		}
		localRootExplicit, stateDirExplicit, err := configuredObjectStorePaths(path)
		if err != nil {
			return Agent{}, err
		}
		configuration.objectStoreLocalRootExplicit = localRootExplicit
		configuration.objectStoreStateDirExplicit = stateDirExplicit
		configuration.SetStateDir(configuration.StateDir)
	}
	applyAgentEnvironment(&configuration)
	err := configuration.Validate()
	return configuration, err
}

func applyAgentEnvironment(configuration *Agent) {
	if value := os.Getenv("SHIFT_STATE_DIR"); value != "" {
		configuration.SetStateDir(value)
	}
	if value := os.Getenv("SHIFT_AGENT_LISTEN"); value != "" {
		configuration.Listen = value
	}
	if value := os.Getenv("SHIFT_SOCKET_GROUP"); value != "" {
		configuration.SocketGroup = value
	}
	if value := os.Getenv("SHIFT_LOG_LEVEL"); value != "" {
		configuration.LogLevel = strings.ToLower(value)
	}
	if value := os.Getenv("SHIFT_CGROUP_ROOT"); value != "" {
		configuration.CgroupRoot = value
	}
	if os.Getenv("SHIFT_OBJECTSTORE_LOCAL_ROOT") != "" {
		configuration.objectStoreLocalRootExplicit = true
	}
	if os.Getenv("SHIFT_OBJECTSTORE_STATE_DIR") != "" {
		configuration.objectStoreStateDirExplicit = true
	}
	objectstore.ApplyEnvironment(&configuration.ObjectStore)
	applyControlPlaneEnvironment(&configuration.ControlPlane)
	applyUpdateEnvironment(&configuration.Updates)
	if value := os.Getenv("SHIFT_OTLP_ENDPOINT"); value != "" {
		configuration.Tracing.OTLPEndpoint = value
	}
	if value := os.Getenv("SHIFT_TRACING_SERVICE_NAME"); value != "" {
		configuration.Tracing.ServiceName = value
	}
}

// applyStorageEnvironment maps SHIFT_STORAGE_* variables onto the hosted
// storage block. Durations accept Go duration strings ("90m"); an unparseable
// duration is ignored here so Validate reports it with its own message — the
// variable was clearly set, so silently keeping the default would hide that.
func applyStorageEnvironment(configuration *StorageConfig) {
	if value := os.Getenv("SHIFT_STORAGE_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err == nil {
			configuration.Enabled = enabled
		}
	}
	if value := os.Getenv("SHIFT_STORAGE_ENDPOINT"); value != "" {
		configuration.Endpoint = value
	}
	if value := os.Getenv("SHIFT_STORAGE_PUBLIC_ENDPOINT"); value != "" {
		configuration.PublicEndpoint = value
	}
	if value := os.Getenv("SHIFT_STORAGE_REGION"); value != "" {
		configuration.Region = value
	}
	if value := os.Getenv("SHIFT_STORAGE_BUCKET"); value != "" {
		configuration.Bucket = value
	}
	if value := os.Getenv("SHIFT_STORAGE_ACCESS_KEY_ID"); value != "" {
		configuration.AccessKeyID = value
	}
	if value := os.Getenv("SHIFT_STORAGE_SECRET_ACCESS_KEY"); value != "" {
		configuration.SecretAccessKey = value
	}
	if value := os.Getenv("SHIFT_STORAGE_CREDENTIAL_TTL"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			configuration.CredentialTTL = parsed
		}
	}
	if value := os.Getenv("SHIFT_STORAGE_RECONCILE_INTERVAL"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			configuration.ReconcileInterval = parsed
		}
	}
}

func applyControlPlaneEnvironment(configuration *ControlReporter) {
	if value := os.Getenv("SHIFT_CONTROL_URL"); value != "" {
		configuration.URL = value
	}
	if value := os.Getenv("SHIFT_CONTROL_ORGANIZATION_ID"); value != "" {
		configuration.OrganizationID = value
	}
	if value := os.Getenv("SHIFT_CONTROL_MACHINE_ID"); value != "" {
		configuration.MachineID = value
	}
	if value := os.Getenv("SHIFT_CONTROL_MACHINE_NAME"); value != "" {
		configuration.MachineName = value
	}
	if value := os.Getenv("SHIFT_CONTROL_AGENT_URL"); value != "" {
		configuration.AgentURL = value
	}
	if value := os.Getenv("SHIFT_CONTROL_API_KEY"); value != "" {
		configuration.APIKey = value
	}
	if value := os.Getenv("SHIFT_CONTROL_REPORT_INTERVAL"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			configuration.Interval = parsed
		}
	}
	// The control plane's address is part of the platform: when the reporter
	// is being configured at all, an omitted URL means "the platform's", not
	// a validation error. A reporter configured with nothing at all stays
	// disabled, so the default never leaks into unconfigured agents.
	if strings.TrimSpace(configuration.URL) == "" &&
		(strings.TrimSpace(configuration.OrganizationID) != "" || strings.TrimSpace(configuration.MachineID) != "" || strings.TrimSpace(configuration.APIKey) != "") {
		configuration.URL = DefaultControlPlaneURL
	}
}

func configuredObjectStorePaths(path string) (localRoot, stateDir bool, err error) {
	file, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = file.Close() }()
	var document struct {
		ObjectStore map[string]json.RawMessage `json:"object_store"`
	}
	if err := json.NewDecoder(io.LimitReader(file, 64<<20)).Decode(&document); err != nil {
		return false, false, fmt.Errorf("inspect %s object-store paths: %w", path, err)
	}
	_, localRoot = document.ObjectStore["local_root"]
	_, stateDir = document.ObjectStore["state_dir"]
	return localRoot, stateDir, nil
}

type ControlPlane struct {
	Version          int    `json:"version"`
	Listen           string `json:"listen"`
	DatabaseURL      string `json:"database_url"`
	PublicURL        string `json:"public_url"`
	TokenPepper      string `json:"token_pepper"`
	PasswordPepper   string `json:"password_pepper"`
	StripeSecretKey  string `json:"stripe_secret_key,omitempty"`
	StripeWebhookKey string `json:"stripe_webhook_key,omitempty"`
	// StripePrices maps a plan key (free, pro, business, enterprise) to the
	// Stripe price id customers of that plan check out with. Checkout routes
	// exist only for plans present here.
	StripePrices    map[string]string `json:"stripe_prices,omitempty"`
	SessionTTL      time.Duration     `json:"session_ttl"`
	RequestTimeout  time.Duration     `json:"request_timeout"`
	LogLevel        string            `json:"log_level"`
	AccessTokenTTL  time.Duration     `json:"access_token_ttl"`
	RefreshTokenTTL time.Duration     `json:"refresh_token_ttl"`
	RateLimitPerMin int               `json:"rate_limit_per_minute"`
	// ComputeTradingEnabled opens SHIFT Compute to other organizations. It is
	// false everywhere by default: scheduling, offers, and reservations work
	// fully inside one organization with the gate closed, and no public offer is
	// accepted or considered until an operator turns it on.
	ComputeTradingEnabled bool `json:"compute_trading_enabled"`
	// ComputeReservationTTL is how long a reservation holds capacity when the
	// caller does not ask for a specific expiry, so an abandoned migration
	// releases the destination on its own.
	ComputeReservationTTL time.Duration `json:"compute_reservation_ttl"`
	// RetentionSweepInterval is how often the control plane enforces
	// per-organization retention policies. Zero disables the background sweep —
	// retention is then enforced only by an operator invoking the sweep — and a
	// policy never deletes anything before its window has actually elapsed.
	RetentionSweepInterval time.Duration `json:"retention_sweep_interval"`
	// OIDC single sign-on. Setting an issuer lets organizations federate their
	// email domain; while an organization enforces SSO, its domain's accounts
	// authenticate through the issuer and local passwords stop working for
	// them. All three values must be present together or none.
	OIDC OIDC `json:"oidc"`

	// AllowedOrigins lists the exact browser origins that may call the API, for
	// the web dashboard ("http://localhost:3000"). A request carrying any other
	// Origin is served without CORS headers, so a browser blocks it; the CLI and
	// agents send no Origin and are unaffected. Empty allows no browser origin.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`

	// Storage is the platform-hosted checkpoint store. When its Enabled flag is
	// off (the default) the control plane serves metadata only, exactly as
	// before, and agents that want a cloud mirror bring their own S3 bucket.
	Storage StorageConfig `json:"storage"`
}

// OIDC names the identity provider the control plane federates to.
type OIDC struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// StorageConfig configures the control plane's hosted checkpoint storage. When
// Enabled, the control plane brokers short-lived, org-scoped credentials for
// the platform's S3-compatible store (MinIO in compose) instead of each user
// supplying their own bucket. Disabled — the default — leaves every existing
// deployment untouched.
type StorageConfig struct {
	Enabled bool `json:"enabled"`
	// Endpoint is the S3 address the control plane signs against (typically
	// the internal MinIO service). PublicEndpoint is what agents receive; it
	// defaults to Endpoint when empty, and differs when the store is reachable
	// from agents under another host or port.
	Endpoint       string `json:"endpoint"`
	PublicEndpoint string `json:"public_endpoint,omitempty"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	// AccessKeyID and SecretAccessKey are the parent credential the control
	// plane assumes org roles from; it never leaves the control plane.
	AccessKeyID     string        `json:"access_key_id"`
	SecretAccessKey string        `json:"secret_access_key"`
	CredentialTTL   time.Duration `json:"credential_ttl"`
	// ReconcileInterval is how often usage is recounted from the bucket.
	ReconcileInterval time.Duration `json:"reconcile_interval"`
}

// AgentEndpoint is the S3 address handed to agents: PublicEndpoint when set,
// otherwise Endpoint.
func (s StorageConfig) AgentEndpoint() string {
	if strings.TrimSpace(s.PublicEndpoint) != "" {
		return s.PublicEndpoint
	}
	return s.Endpoint
}

func DefaultControlPlane() ControlPlane {
	return ControlPlane{
		Version:                1,
		Listen:                 "127.0.0.1:8090",
		SessionTTL:             30 * 24 * time.Hour,
		RequestTimeout:         30 * time.Second,
		LogLevel:               "info",
		AccessTokenTTL:         15 * time.Minute,
		RefreshTokenTTL:        30 * 24 * time.Hour,
		RateLimitPerMin:        120,
		ComputeTradingEnabled:  false,
		ComputeReservationTTL:  15 * time.Minute,
		RetentionSweepInterval: time.Hour,
	}
}

// Validate checks the OIDC triple: configured together or not at all, and the
// issuer must be an https URL — the token validation refuses plain http, so
// configuration that would never work is refused at load time.
func (o OIDC) Validate() error {
	issuer := strings.TrimSpace(o.Issuer)
	clientID := strings.TrimSpace(o.ClientID)
	secret := strings.TrimSpace(o.ClientSecret)
	if issuer == "" && clientID == "" && secret == "" {
		return nil
	}
	if issuer == "" || clientID == "" || secret == "" {
		return errors.New("oidc issuer, client id, and client secret must be configured together")
	}
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("oidc issuer must be an https URL without query or fragment: %q", issuer)
	}
	return nil
}

// Configured reports whether single sign-on is available at all.
func (o OIDC) Configured() bool {
	return strings.TrimSpace(o.Issuer) != "" && strings.TrimSpace(o.ClientID) != "" && strings.TrimSpace(o.ClientSecret) != ""
}

// Validate applies hosted-storage rules when the block is enabled; a disabled
// block is always valid so partially-filled leftovers never break startup.
// Defaults for the two intervals are applied in place (matching the reporter's
// style) rather than rejected, because both have safe values.
func (s *StorageConfig) Validate() error {
	if !s.Enabled {
		return nil
	}
	endpoint := strings.TrimRight(strings.TrimSpace(s.Endpoint), "/")
	if endpoint == "" {
		return errors.New("storage endpoint is required when hosted storage is enabled")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("storage endpoint must be an http(s) URL: %q", s.Endpoint)
	}
	if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("storage endpoint must carry no path, query, fragment, or credentials: %q", s.Endpoint)
	}
	if strings.TrimSpace(s.PublicEndpoint) != "" {
		public, err := url.Parse(strings.TrimSpace(s.PublicEndpoint))
		if err != nil || (public.Scheme != "https" && public.Scheme != "http") || public.Host == "" {
			return fmt.Errorf("storage public endpoint must be an http(s) URL: %q", s.PublicEndpoint)
		}
	}
	if strings.TrimSpace(s.Region) == "" {
		return errors.New("storage region is required when hosted storage is enabled")
	}
	if err := objectstore.ValidateBucket(strings.TrimSpace(s.Bucket)); err != nil {
		return fmt.Errorf("storage bucket: %w", err)
	}
	if s.AccessKeyID == "" || s.SecretAccessKey == "" {
		return errors.New("storage access key id and secret access key are required when hosted storage is enabled")
	}
	if s.CredentialTTL <= 0 {
		s.CredentialTTL = DefaultStorageCredentialTTL
	}
	if s.CredentialTTL < 15*time.Minute || s.CredentialTTL > 7*24*time.Hour {
		return fmt.Errorf("storage credential TTL must be between 15 minutes and 7 days, got %s", s.CredentialTTL)
	}
	if s.ReconcileInterval <= 0 {
		s.ReconcileInterval = DefaultStorageReconcileInterval
	}
	if s.ReconcileInterval < 30*time.Second {
		return fmt.Errorf("storage reconcile interval must be at least 30 seconds, got %s", s.ReconcileInterval)
	}
	return nil
}

// Default intervals applied by StorageConfig.Validate when the block is
// enabled but leaves them unset.
const (
	DefaultStorageCredentialTTL     = time.Hour
	DefaultStorageReconcileInterval = 5 * time.Minute
)

func (c *ControlPlane) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported control-plane config version %d", c.Version)
	}
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("control-plane listen address is required")
	}
	if strings.TrimSpace(c.DatabaseURL) == "" {
		return errors.New("database URL is required")
	}
	if len(c.TokenPepper) < 32 || len(c.PasswordPepper) < 32 {
		return errors.New("token and password peppers must each contain at least 32 characters")
	}
	if c.AccessTokenTTL < time.Minute || c.AccessTokenTTL > 24*time.Hour {
		return errors.New("access token TTL must be between one minute and 24 hours")
	}
	if c.RefreshTokenTTL < time.Hour || c.RefreshTokenTTL > 365*24*time.Hour {
		return errors.New("refresh token TTL must be between one hour and one year")
	}
	if c.SessionTTL < c.RefreshTokenTTL {
		return errors.New("session TTL cannot be shorter than refresh token TTL")
	}
	if c.RequestTimeout <= 0 || c.RequestTimeout > 5*time.Minute {
		return errors.New("request timeout must be positive and no longer than five minutes")
	}
	if c.RateLimitPerMin < 10 || c.RateLimitPerMin > 10000 {
		return errors.New("rate limit must be between 10 and 10000 requests per minute")
	}
	for _, origin := range c.AllowedOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("allowed origin %q must be an http:// or https:// origin with a host and no path, query, or fragment", origin)
		}
	}
	if c.ComputeReservationTTL < time.Minute || c.ComputeReservationTTL > 24*time.Hour {
		return errors.New("compute reservation TTL must be between one minute and 24 hours")
	}
	if c.RetentionSweepInterval < 0 || c.RetentionSweepInterval > 7*24*time.Hour {
		return errors.New("retention sweep interval must be zero (disabled) or at most seven days")
	}
	if err := c.OIDC.Validate(); err != nil {
		return err
	}
	if err := c.Storage.Validate(); err != nil {
		return err
	}
	for plan, priceID := range c.StripePrices {
		if !billing.KnownPlan(plan) {
			return fmt.Errorf("stripe price configured for unknown plan %q", plan)
		}
		if strings.TrimSpace(priceID) == "" || !strings.HasPrefix(priceID, "price_") {
			return fmt.Errorf("stripe price for plan %q must be a Stripe price id", plan)
		}
	}
	return nil
}

func LoadControlPlane(path string) (ControlPlane, error) {
	configuration, err := LoadControlPlaneUnvalidated(path)
	if err != nil {
		return ControlPlane{}, err
	}
	if err := configuration.Validate(); err != nil {
		return ControlPlane{}, err
	}
	return configuration, nil
}

// LoadControlPlaneUnvalidated loads file and environment values so callers that
// provide command-line overrides can apply them before validation.
func LoadControlPlaneUnvalidated(path string) (ControlPlane, error) {
	configuration := DefaultControlPlane()
	if path != "" {
		if err := persistence.ReadJSON(path, &configuration); err != nil {
			return ControlPlane{}, err
		}
	}
	if value := os.Getenv("SHIFT_DATABASE_URL"); value != "" {
		configuration.DatabaseURL = value
	}
	if value := os.Getenv("SHIFT_TOKEN_PEPPER"); value != "" {
		configuration.TokenPepper = value
	}
	if value := os.Getenv("SHIFT_PASSWORD_PEPPER"); value != "" {
		configuration.PasswordPepper = value
	}
	if value := os.Getenv("SHIFT_CONTROL_LISTEN"); value != "" {
		configuration.Listen = value
	}
	// SHIFT_ALLOWED_ORIGINS is a comma-separated list that replaces the file's
	// allowed_origins, e.g. SHIFT_ALLOWED_ORIGINS=http://localhost:3000,https://app.example.com.
	if value := os.Getenv("SHIFT_ALLOWED_ORIGINS"); value != "" {
		configuration.AllowedOrigins = nil
		for _, origin := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(origin); trimmed != "" {
				configuration.AllowedOrigins = append(configuration.AllowedOrigins, trimmed)
			}
		}
	}
	if value := os.Getenv("SHIFT_COMPUTE_TRADING_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return ControlPlane{}, fmt.Errorf("SHIFT_COMPUTE_TRADING_ENABLED must be a boolean: %w", err)
		}
		configuration.ComputeTradingEnabled = enabled
	}
	if value := os.Getenv("SHIFT_OIDC_ISSUER"); value != "" {
		configuration.OIDC.Issuer = value
	}
	if value := os.Getenv("SHIFT_OIDC_CLIENT_ID"); value != "" {
		configuration.OIDC.ClientID = value
	}
	if value := os.Getenv("SHIFT_OIDC_CLIENT_SECRET"); value != "" {
		configuration.OIDC.ClientSecret = value
	}
	// SHIFT_STRIPE_PRICE_<PLAN> overrides one plan's Stripe price id, e.g.
	// SHIFT_STRIPE_PRICE_PRO=price_1234. The key is uppercased with dashes and
	// dots turned into underscores.
	for _, plan := range billing.PlanKeys() {
		if value := os.Getenv("SHIFT_STRIPE_PRICE_" + strings.ToUpper(strings.ReplaceAll(plan, "-", "_"))); value != "" {
			if configuration.StripePrices == nil {
				configuration.StripePrices = map[string]string{}
			}
			configuration.StripePrices[plan] = value
		}
	}
	applyStorageEnvironment(&configuration.Storage)
	if configuration.ComputeReservationTTL <= 0 {
		configuration.ComputeReservationTTL = DefaultControlPlane().ComputeReservationTTL
	}
	if configuration.DatabaseURL == "" {
		return ControlPlane{}, errors.New("database URL is required")
	}
	return configuration, nil
}
