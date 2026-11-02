package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentStateDirRebasesInheritedObjectStorePaths(t *testing.T) {
	clearAgentEnvironment(t)
	configurationPath := filepath.Join(t.TempDir(), "agent.json")
	writeConfig(t, configurationPath, `{
  "version": 1,
  "state_dir": "/srv/shift",
  "listen": "unix:///run/shift/agent.sock",
  "chunk_size_bytes": 4194304,
  "max_concurrent_migrations": 2,
  "shutdown_timeout": 20000000000,
  "log_level": "info",
  "object_store": {"enabled": false, "backend": "local"}
}`)

	configuration, err := LoadAgent(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ObjectStore.LocalRoot != "/srv/shift/cloud-objects" {
		t.Fatalf("local root was not rebased: %q", configuration.ObjectStore.LocalRoot)
	}
	if configuration.ObjectStore.StateDir != "/srv/shift/objectstore-state" {
		t.Fatalf("state directory was not rebased: %q", configuration.ObjectStore.StateDir)
	}
}

func TestAgentStateDirPreservesExplicitObjectStorePaths(t *testing.T) {
	clearAgentEnvironment(t)
	configurationPath := filepath.Join(t.TempDir(), "agent.json")
	writeConfig(t, configurationPath, `{
  "version": 1,
  "state_dir": "/srv/shift",
  "listen": "unix:///run/shift/agent.sock",
  "chunk_size_bytes": 4194304,
  "max_concurrent_migrations": 2,
  "shutdown_timeout": 20000000000,
  "log_level": "info",
  "object_store": {
    "enabled": false,
    "backend": "local",
    "local_root": "/mnt/checkpoints",
    "state_dir": "/mnt/objectstore-state"
  }
}`)

	configuration, err := LoadAgent(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration.SetStateDir("/opt/shift")
	if configuration.ObjectStore.LocalRoot != "/mnt/checkpoints" || configuration.ObjectStore.StateDir != "/mnt/objectstore-state" {
		t.Fatalf("explicit paths changed: %+v", configuration.ObjectStore)
	}
}

func TestAgentEnvironmentAndFlagStateDirPrecedence(t *testing.T) {
	clearAgentEnvironment(t)
	t.Setenv("SHIFT_STATE_DIR", "/env/shift")
	t.Setenv("SHIFT_OBJECTSTORE_LOCAL_ROOT", "/remote/checkpoints")

	configuration, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ObjectStore.StateDir != "/env/shift/objectstore-state" {
		t.Fatalf("environment state directory was not rebased: %q", configuration.ObjectStore.StateDir)
	}
	configuration.SetStateDir("/flag/shift")
	if configuration.ObjectStore.LocalRoot != "/remote/checkpoints" {
		t.Fatalf("explicit environment path changed: %q", configuration.ObjectStore.LocalRoot)
	}
	if configuration.ObjectStore.StateDir != "/flag/shift/objectstore-state" {
		t.Fatalf("inherited path did not follow flag override: %q", configuration.ObjectStore.StateDir)
	}
}

func TestAgentControlReporterDefaultsToThePlatformURL(t *testing.T) {
	clearAgentEnvironment(t)
	t.Setenv("SHIFT_CONTROL_ORGANIZATION_ID", "org_1234")
	t.Setenv("SHIFT_CONTROL_MACHINE_ID", "mach_1234")
	t.Setenv("SHIFT_CONTROL_API_KEY", "shift_ak_test_0123456789abcdef")

	// LoadAgent validates the reporter, so this also proves a reporter whose
	// URL was defaulted survives validation instead of failing on a missing URL.
	configuration, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.ControlPlane.Enabled() {
		t.Fatal("organization, machine, and key alone must enable the reporter")
	}
	if configuration.ControlPlane.URL != DefaultControlPlaneURL {
		t.Fatalf("reporter URL = %q, want the platform default %q", configuration.ControlPlane.URL, DefaultControlPlaneURL)
	}
}

func TestAgentControlReporterWithoutSettingsStaysDisabled(t *testing.T) {
	clearAgentEnvironment(t)

	configuration, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.ControlPlane.Enabled() {
		t.Fatal("an unconfigured reporter must stay disabled — the platform default never leaks into it")
	}
	if configuration.ControlPlane.URL != "" {
		t.Fatalf("reporter URL = %q, want empty", configuration.ControlPlane.URL)
	}
}

func writeConfig(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clearAgentEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SHIFT_STATE_DIR", "SHIFT_AGENT_LISTEN", "SHIFT_LOG_LEVEL", "SHIFT_OBJECTSTORE_ENABLED",
		"SHIFT_OBJECTSTORE_BACKEND", "SHIFT_OBJECTSTORE_LOCAL_ROOT", "SHIFT_S3_ENDPOINT",
		"SHIFT_S3_REGION", "SHIFT_S3_BUCKET", "SHIFT_S3_ACCESS_KEY_ID", "SHIFT_S3_SECRET_ACCESS_KEY",
		"SHIFT_S3_SESSION_TOKEN", "SHIFT_S3_PREFIX", "SHIFT_OBJECTSTORE_STATE_DIR", "SHIFT_S3_FORCE_PATH_STYLE",
		"SHIFT_UPDATE_ENABLED", "SHIFT_UPDATE_CHANNEL", "SHIFT_UPDATE_POLICY", "SHIFT_UPDATE_FEED_URL",
		"SHIFT_UPDATE_FEED_FILE", "SHIFT_UPDATE_TRUSTED_KEYS_FILE", "SHIFT_UPDATE_SIGNATURE_THRESHOLD",
		"SHIFT_UPDATE_CHECK_INTERVAL", "SHIFT_UPDATE_EXECUTABLE",
		"SHIFT_OTLP_ENDPOINT", "SHIFT_TRACING_SERVICE_NAME", "SHIFT_OTLP_SERVICE_NAME",
		"SHIFT_CONTROL_URL", "SHIFT_CONTROL_ORGANIZATION_ID", "SHIFT_CONTROL_MACHINE_ID",
		"SHIFT_CONTROL_MACHINE_NAME", "SHIFT_CONTROL_AGENT_URL", "SHIFT_CONTROL_API_KEY",
		"SHIFT_CONTROL_REPORT_INTERVAL", "SHIFT_CGROUP_ROOT", "SHIFT_ALLOW_REMOTE_AGENT_DISPATCH",
	} {
		t.Setenv(name, "")
	}
}

func TestAgentCgroupRootEnvironment(t *testing.T) {
	clearAgentEnvironment(t)

	configuration, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.CgroupRoot != "" {
		t.Fatalf("unset cgroup_root must stay empty for the default root: %q", configuration.CgroupRoot)
	}

	t.Setenv("SHIFT_CGROUP_ROOT", "/sys/fs/cgroup/shift-agent-2")
	configuration, err = LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.CgroupRoot != "/sys/fs/cgroup/shift-agent-2" {
		t.Fatalf("cgroup_root from environment = %q", configuration.CgroupRoot)
	}
}

func TestAgentCgroupRootValidation(t *testing.T) {
	clearAgentEnvironment(t)
	for _, root := range []string{"shift/relative", "/sys/fs/cgroup/../shift", "/sys/fs/cgroup/shift/", "/"} {
		configuration := DefaultAgent()
		configuration.CgroupRoot = root
		if err := configuration.Validate(); err == nil {
			t.Fatalf("cgroup_root %q was accepted", root)
		}
	}
	configuration := DefaultAgent()
	configuration.CgroupRoot = "/sys/fs/cgroup/shift-agent-2"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("a clean absolute cgroup_root was rejected: %v", err)
	}
}

func TestTracingConfigValidation(t *testing.T) {
	disabled := Tracing{}
	if disabled.Enabled() {
		t.Fatal("empty tracing config claims to be enabled")
	}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled tracing rejected: %v", err)
	}
	valid := Tracing{OTLPEndpoint: "https://collector.internal:4318/v1/traces"}
	if !valid.Enabled() {
		t.Fatal("configured endpoint not detected")
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid endpoint rejected: %v", err)
	}
	for _, endpoint := range []string{
		"collector.internal:4318",
		"grpc://collector:4317",
		"ftp://collector/traces",
		"http://",
	} {
		invalid := Tracing{OTLPEndpoint: endpoint}
		if err := invalid.Validate(); err == nil {
			t.Fatalf("endpoint %q accepted", endpoint)
		}
	}
}

func TestAgentEnvironmentConfiguresTracing(t *testing.T) {
	clearAgentEnvironment(t)
	t.Setenv("SHIFT_OTLP_ENDPOINT", "http://127.0.0.1:4318/v1/traces")
	t.Setenv("SHIFT_TRACING_SERVICE_NAME", "edge-agent-1")
	configuration, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Tracing.OTLPEndpoint != "http://127.0.0.1:4318/v1/traces" {
		t.Fatalf("endpoint = %q", configuration.Tracing.OTLPEndpoint)
	}
	if configuration.Tracing.ServiceName != "edge-agent-1" {
		t.Fatalf("service name = %q", configuration.Tracing.ServiceName)
	}
	if !configuration.Tracing.Enabled() {
		t.Fatal("environment-configured tracing not enabled")
	}
}

func validControlPlaneConfiguration() ControlPlane {
	configuration := DefaultControlPlane()
	configuration.DatabaseURL = "postgres://localhost:5432/shift"
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	return configuration
}

func TestStripePriceConfigValidation(t *testing.T) {
	configuration := validControlPlaneConfiguration()
	configuration.StripePrices = map[string]string{
		"pro":      "price_pro_monthly",
		"business": "price_business_monthly",
	}
	if err := configuration.Validate(); err != nil {
		t.Fatalf("valid price map rejected: %v", err)
	}
	configuration.StripePrices = map[string]string{"nonexistent": "price_1"}
	if err := configuration.Validate(); err == nil {
		t.Fatal("price for an unknown plan accepted")
	}
	configuration.StripePrices = map[string]string{"pro": "not-a-price-id"}
	if err := configuration.Validate(); err == nil {
		t.Fatal("malformed price id accepted")
	}
	configuration.StripePrices = map[string]string{"pro": ""}
	if err := configuration.Validate(); err == nil {
		t.Fatal("empty price id accepted")
	}
}

func TestControlPlaneEnvironmentConfiguresStripePrices(t *testing.T) {
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	t.Setenv("SHIFT_STRIPE_PRICE_PRO", "price_env_pro")
	t.Setenv("SHIFT_STRIPE_PRICE_BUSINESS", "price_env_business")
	configuration, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.StripePrices["pro"] != "price_env_pro" {
		t.Fatalf("pro price = %q", configuration.StripePrices["pro"])
	}
	if configuration.StripePrices["business"] != "price_env_business" {
		t.Fatalf("business price = %q", configuration.StripePrices["business"])
	}
	if _, present := configuration.StripePrices["enterprise"]; present {
		t.Fatal("unset plan price was invented")
	}
	if err := configuration.Validate(); err != nil {
		t.Fatalf("environment-configured prices rejected: %v", err)
	}
}

func TestControlPlaneEnvironmentConfiguresAllowedOrigins(t *testing.T) {
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	t.Setenv("SHIFT_ALLOWED_ORIGINS", " http://localhost:3001 , https://app.example.com ,")
	configuration, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.AllowedOrigins) != 2 {
		t.Fatalf("allowed origins = %#v, want the two non-empty entries", configuration.AllowedOrigins)
	}
	if configuration.AllowedOrigins[0] != "http://localhost:3001" || configuration.AllowedOrigins[1] != "https://app.example.com" {
		t.Fatalf("allowed origins = %#v, want trimmed values in order", configuration.AllowedOrigins)
	}
	if err := configuration.Validate(); err != nil {
		t.Fatalf("environment-configured origins rejected: %v", err)
	}
}

func TestControlPlaneRejectsMalformedAllowedOrigins(t *testing.T) {
	configuration := DefaultControlPlane()
	configuration.DatabaseURL = "postgres://localhost:5432/shift"
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	// Each entry omits or adds something an origin is not: no scheme, a path,
	// a query, a fragment, a non-web scheme, no host.
	for _, origin := range []string{
		"localhost:3001",
		"http://localhost:3000/app",
		"http://localhost:3000?x=1",
		"http://localhost:3000#f",
		"ftp://localhost:3000",
		"http://",
	} {
		configuration.AllowedOrigins = []string{origin}
		if err := configuration.Validate(); err == nil {
			t.Fatalf("origin %q must be rejected", origin)
		}
	}
	configuration.AllowedOrigins = []string{"http://localhost:3000", "https://app.example.com"}
	if err := configuration.Validate(); err != nil {
		t.Fatalf("exact origins must be accepted: %v", err)
	}
}

// The metrics scrape token is an operator shared secret: a set token too short
// to resist guessing is refused at startup. An unset token stays valid here —
// /metrics then fails closed at request time instead of opening.
func TestControlPlaneValidatesMetricsTokenLength(t *testing.T) {
	configuration := validControlPlaneConfiguration()
	configuration.MetricsToken = strings.Repeat("m", 15)
	if err := configuration.Validate(); err == nil {
		t.Fatal("a metrics token under 16 characters must be refused")
	}
	configuration.MetricsToken = strings.Repeat("m", 16)
	if err := configuration.Validate(); err != nil {
		t.Fatalf("a 16-character metrics token rejected: %v", err)
	}
	configuration.MetricsToken = ""
	if err := configuration.Validate(); err != nil {
		t.Fatalf("an unset metrics token rejected: %v", err)
	}
}

func validStorageConfiguration() StorageConfig {
	return StorageConfig{
		Enabled:         true,
		Endpoint:        "http://minio:9000",
		Region:          "us-east-1",
		Bucket:          "shift-checkpoints",
		AccessKeyID:     "storage-key",
		SecretAccessKey: "storage-secret",
	}
}

func TestStorageConfigValidation(t *testing.T) {
	disabled := StorageConfig{}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled storage rejected: %v", err)
	}
	// A disabled block with leftovers is still fine: hosting is simply off.
	disabled.Endpoint = "not a url"
	if err := disabled.Validate(); err != nil {
		t.Fatalf("disabled storage with leftovers rejected: %v", err)
	}

	configuration := validStorageConfiguration()
	if err := configuration.Validate(); err != nil {
		t.Fatalf("valid storage rejected: %v", err)
	}
	if configuration.CredentialTTL != DefaultStorageCredentialTTL {
		t.Fatalf("credential TTL default not applied: %s", configuration.CredentialTTL)
	}
	if configuration.ReconcileInterval != DefaultStorageReconcileInterval {
		t.Fatalf("reconcile interval default not applied: %s", configuration.ReconcileInterval)
	}
	if configuration.AgentEndpoint() != "http://minio:9000" {
		t.Fatalf("agent endpoint without public override = %q", configuration.AgentEndpoint())
	}
	configuration.PublicEndpoint = "https://storage.shiftgate.dev"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("public endpoint rejected: %v", err)
	}
	if configuration.AgentEndpoint() != "https://storage.shiftgate.dev" {
		t.Fatalf("agent endpoint with public override = %q", configuration.AgentEndpoint())
	}

	cases := []struct {
		name   string
		damage func(*StorageConfig)
	}{
		{"no endpoint", func(c *StorageConfig) { c.Endpoint = "" }},
		{"endpoint without scheme", func(c *StorageConfig) { c.Endpoint = "minio:9000" }},
		{"endpoint with path", func(c *StorageConfig) { c.Endpoint = "http://minio:9000/bucket" }},
		{"endpoint with query", func(c *StorageConfig) { c.Endpoint = "http://minio:9000?x=1" }},
		{"endpoint with credentials", func(c *StorageConfig) { c.Endpoint = "http://key:secret@minio:9000" }},
		{"public endpoint without scheme", func(c *StorageConfig) { c.PublicEndpoint = "storage.example.com" }},
		{"no region", func(c *StorageConfig) { c.Region = "" }},
		{"no bucket", func(c *StorageConfig) { c.Bucket = "" }},
		{"short bucket", func(c *StorageConfig) { c.Bucket = "ab" }},
		{"no access key", func(c *StorageConfig) { c.AccessKeyID = "" }},
		{"no secret", func(c *StorageConfig) { c.SecretAccessKey = "" }},
		{"credential ttl too short", func(c *StorageConfig) { c.CredentialTTL = time.Minute }},
		{"credential ttl too long", func(c *StorageConfig) { c.CredentialTTL = 8 * 24 * time.Hour }},
		{"reconcile too fast", func(c *StorageConfig) { c.ReconcileInterval = time.Second }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			configuration := validStorageConfiguration()
			configuration.CredentialTTL = time.Hour
			configuration.ReconcileInterval = 5 * time.Minute
			testCase.damage(&configuration)
			if err := configuration.Validate(); err == nil {
				t.Fatalf("%s accepted", testCase.name)
			}
		})
	}
}

func TestControlPlaneRejectsIncompleteStorage(t *testing.T) {
	configuration := validControlPlaneConfiguration()
	configuration.Storage = StorageConfig{Enabled: true}
	if err := configuration.Validate(); err == nil {
		t.Fatal("enabled storage with nothing configured accepted")
	}
	configuration.Storage = validStorageConfiguration()
	if err := configuration.Validate(); err != nil {
		t.Fatalf("complete storage block rejected: %v", err)
	}
}

func TestControlPlaneEnvironmentConfiguresStorage(t *testing.T) {
	clearStorageEnvironment(t)
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	t.Setenv("SHIFT_STORAGE_ENABLED", "true")
	t.Setenv("SHIFT_STORAGE_ENDPOINT", "http://127.0.0.1:9100")
	t.Setenv("SHIFT_STORAGE_PUBLIC_ENDPOINT", "https://storage.example.com")
	t.Setenv("SHIFT_STORAGE_REGION", "us-east-1")
	t.Setenv("SHIFT_STORAGE_BUCKET", "shift-checkpoints")
	t.Setenv("SHIFT_STORAGE_ACCESS_KEY_ID", "storage-key")
	t.Setenv("SHIFT_STORAGE_SECRET_ACCESS_KEY", "storage-secret")
	t.Setenv("SHIFT_STORAGE_CREDENTIAL_TTL", "90m")
	t.Setenv("SHIFT_STORAGE_RECONCILE_INTERVAL", "2m")

	configuration, err := LoadControlPlane("")
	if err != nil {
		t.Fatal(err)
	}
	storage := configuration.Storage
	if !storage.Enabled || storage.Endpoint != "http://127.0.0.1:9100" || storage.PublicEndpoint != "https://storage.example.com" {
		t.Fatalf("storage = %+v", storage)
	}
	if storage.Region != "us-east-1" || storage.Bucket != "shift-checkpoints" {
		t.Fatalf("storage = %+v", storage)
	}
	if storage.AccessKeyID != "storage-key" || storage.SecretAccessKey != "storage-secret" {
		t.Fatalf("storage = %+v", storage)
	}
	if storage.CredentialTTL != 90*time.Minute {
		t.Fatalf("credential TTL = %s, want 90m", storage.CredentialTTL)
	}
	if storage.ReconcileInterval != 2*time.Minute {
		t.Fatalf("reconcile interval = %s, want 2m", storage.ReconcileInterval)
	}
	if storage.AgentEndpoint() != "https://storage.example.com" {
		t.Fatalf("agent endpoint = %q", storage.AgentEndpoint())
	}
}

func TestControlPlaneStorageDisabledByDefault(t *testing.T) {
	clearStorageEnvironment(t)
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	configuration, err := LoadControlPlane("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Storage.Enabled {
		t.Fatal("storage must be disabled without SHIFT_STORAGE_ENABLED")
	}
}

func clearStorageEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"SHIFT_STORAGE_ENABLED", "SHIFT_STORAGE_ENDPOINT", "SHIFT_STORAGE_PUBLIC_ENDPOINT",
		"SHIFT_STORAGE_REGION", "SHIFT_STORAGE_BUCKET", "SHIFT_STORAGE_ACCESS_KEY_ID",
		"SHIFT_STORAGE_SECRET_ACCESS_KEY", "SHIFT_STORAGE_CREDENTIAL_TTL", "SHIFT_STORAGE_RECONCILE_INTERVAL",
	} {
		t.Setenv(name, "")
	}
}

// TestAgentTCPListenRequiresCompleteTLS proves the development-mode TCP
// listener enforces the TLS trio whenever a TLS block is partially present —
// a check that used to sit dead behind the production rejection — and that
// production still refuses a TCP control-API listener outright.
func TestAgentTCPListenRequiresCompleteTLS(t *testing.T) {
	clearAgentEnvironment(t)
	configuration := DefaultAgent()
	configuration.Listen = "tcp://127.0.0.1:18443"
	configuration.InsecureDevelopment = true
	if err := configuration.Validate(); err != nil {
		t.Fatalf("plain dev-mode TCP listen was rejected: %v", err)
	}
	configuration.TLS.CertificateFile = "/etc/shift/tls/agent-cert.pem"
	if err := configuration.Validate(); err == nil {
		t.Fatal("a partial TLS block on a TCP listener was accepted")
	}
	configuration.TLS.PrivateKeyFile = "/etc/shift/tls/agent-key.pem"
	configuration.TLS.ClientCAFile = "/etc/shift/tls/agents-ca.pem"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("a complete TLS block on a dev-mode TCP listener was rejected: %v", err)
	}
	production := DefaultAgent()
	production.Listen = "tcp://127.0.0.1:18443"
	if err := production.Validate(); err == nil {
		t.Fatal("a production TCP control-API listener was accepted")
	}
}

func writeConfigWithMode(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

const minimalAgentConfig = `{
  "version": 1,
  "state_dir": "/srv/shift",
  "listen": "unix:///run/shift/agent.sock",
  "chunk_size_bytes": 4194304,
  "max_concurrent_migrations": 2,
  "shutdown_timeout": 20000000000,
  "log_level": "info",
  "object_store": {"enabled": false, "backend": "local"}
}`

// TestAgentConfigFilePermissions covers the crash-loop regression: install.sh
// and the systemd units write agent.json 0640, which the loader used to
// refuse, so every install path died on start. Group-readable configs load on
// a tight ownership chain; anything readable by other keeps failing closed.
func TestAgentConfigFilePermissions(t *testing.T) {
	clearAgentEnvironment(t)
	directory := t.TempDir()
	// The strict chain requires a parent that group members cannot write;
	// pin it so the test does not depend on the environment's umask.
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name    string
		mode    os.FileMode
		parent  os.FileMode
		wantErr string
	}{
		{name: "owner only loads", mode: 0o600},
		{name: "installer group-readable loads", mode: 0o640},
		{name: "read-only owner loads", mode: 0o400},
		{name: "world-readable refuses", mode: 0o644, wantErr: "readable by group or other"},
		{name: "group-writable refuses", mode: 0o620, wantErr: "chmod 600"},
		{name: "group-executable refuses", mode: 0o610, wantErr: "chmod 600"},
		{name: "group-readable under a writable parent refuses", mode: 0o640, parent: 0o777, wantErr: "writable by group or other"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			parent := directory
			if testCase.parent != 0 {
				parent = filepath.Join(directory, "shared")
				if err := os.Mkdir(parent, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(parent, testCase.parent); err != nil {
					t.Fatal(err)
				}
			}
			configurationPath := filepath.Join(parent, "agent.json")
			writeConfigWithMode(t, configurationPath, minimalAgentConfig, testCase.mode)
			_, err := LoadAgent(configurationPath)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("mode %04o must load: %v", testCase.mode, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("mode %04o error = %v, want it to mention %q", testCase.mode, err, testCase.wantErr)
			}
		})
	}
}

func TestControlConfigFilePermissionsAcceptInstallerMode(t *testing.T) {
	clearStorageEnvironment(t)
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	configurationPath := filepath.Join(t.TempDir(), "control.json")
	if err := os.Chmod(filepath.Dir(configurationPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeConfigWithMode(t, configurationPath, `{"version": 1}`, 0o640)
	if _, err := LoadControlPlaneUnvalidated(configurationPath); err != nil {
		t.Fatalf("an installer-mode control.json must load: %v", err)
	}
	writeConfigWithMode(t, configurationPath, `{"version": 1}`, 0o644)
	if _, err := LoadControlPlaneUnvalidated(configurationPath); err == nil {
		t.Fatal("a world-readable control.json must be refused")
	}
}

// TestAgentUnprivilegedEvaluationFlow is the README's documented unprivileged
// flow: `shift-agent --state-dir ./data --listen unix://./data/agent.sock`.
// Both a relative state dir and an explicitly relative unix:// URL must start
// the agent instead of failing validation or binding an unintended path.
func TestAgentUnprivilegedEvaluationFlow(t *testing.T) {
	clearAgentEnvironment(t)
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	configuration := DefaultAgent()
	configuration.SetStateDir("./data")
	configuration.Listen = "unix://./data/agent.sock"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("the unprivileged evaluation flow must validate: %v", err)
	}
	wantStateDir := filepath.Join(workingDirectory, "data")
	if configuration.StateDir != wantStateDir {
		t.Fatalf("state dir = %q, want %q", configuration.StateDir, wantStateDir)
	}
	wantListen := "unix://" + filepath.Join(wantStateDir, "agent.sock")
	if configuration.Listen != wantListen {
		t.Fatalf("listen = %q, want %q", configuration.Listen, wantListen)
	}
	if configuration.ObjectStore.LocalRoot != filepath.Join(wantStateDir, "cloud-objects") {
		t.Fatalf("object-store root did not follow the state dir: %q", configuration.ObjectStore.LocalRoot)
	}
}

func TestAgentListenURLNormalizesPaths(t *testing.T) {
	clearAgentEnvironment(t)
	configuration := DefaultAgent()
	configuration.Listen = "unix:///run/shift/agent.sock"
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	if configuration.Listen != "unix:///run/shift/agent.sock" {
		t.Fatalf("the canonical listen URL was rewritten: %q", configuration.Listen)
	}
	configuration.Listen = "unix:///run//shift/../shift/agent.sock"
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	if configuration.Listen != "unix:///run/shift/agent.sock" {
		t.Fatalf("unclean listen URL was not cleaned: %q", configuration.Listen)
	}
}

// TestAgentListenURLRejectsAuthorityTypo covers the silent-wrong-socket
// regression: `unix://run/shift/agent.sock` (one slash short) used to bind
// /shift/agent.sock. A non-empty URL authority is only honored as an explicit
// relative-path marker; anything else fails with the canonical form.
func TestAgentListenURLRejectsAuthorityTypo(t *testing.T) {
	clearAgentEnvironment(t)
	for _, listen := range []string{
		"unix://run/shift/agent.sock",
		"unix://localhost/run/shift/agent.sock",
		"unix://data/agent.sock",
		"unix:",
	} {
		configuration := DefaultAgent()
		configuration.Listen = listen
		err := configuration.Validate()
		if err == nil {
			t.Fatalf("listen %q must be refused", listen)
		}
	}
	configuration := DefaultAgent()
	configuration.Listen = "unix://run/shift/agent.sock"
	if err := configuration.Validate(); err == nil || !strings.Contains(err.Error(), "unix:///run/shift/agent.sock") {
		t.Fatalf("the typo error must show the canonical form, got %v", err)
	}
	configuration = DefaultAgent()
	configuration.Listen = "unix://../run/shift/agent.sock"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("an explicit relative path must be accepted: %v", err)
	}
}

func TestAgentShutdownTimeoutValidation(t *testing.T) {
	clearAgentEnvironment(t)
	configuration := DefaultAgent()
	configuration.ShutdownTimeout = 0
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	if configuration.ShutdownTimeout != 20*time.Second {
		t.Fatalf("zero shutdown_timeout must default to 20s, got %s", configuration.ShutdownTimeout)
	}
	configuration = DefaultAgent()
	configuration.ShutdownTimeout = -5
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	if configuration.ShutdownTimeout != 20*time.Second {
		t.Fatalf("negative shutdown_timeout must default to 20s, got %s", configuration.ShutdownTimeout)
	}
	configuration = DefaultAgent()
	configuration.ShutdownTimeout = 500 * time.Millisecond
	if err := configuration.Validate(); err == nil {
		t.Fatal("a sub-second shutdown_timeout must be refused")
	}
	configuration = DefaultAgent()
	configuration.ShutdownTimeout = 3600000000000
	if err := configuration.Validate(); err == nil {
		t.Fatal("a runaway shutdown_timeout must be refused")
	}
	configuration = DefaultAgent()
	configuration.ShutdownTimeout = 20 * time.Second
	if err := configuration.Validate(); err != nil {
		t.Fatalf("the default shutdown_timeout must be accepted: %v", err)
	}
}

func TestAgentLogLevelValidation(t *testing.T) {
	clearAgentEnvironment(t)
	configuration := DefaultAgent()
	configuration.LogLevel = ""
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	if configuration.LogLevel != "info" {
		t.Fatalf("empty log_level must default to info, got %q", configuration.LogLevel)
	}
	configuration = DefaultAgent()
	configuration.LogLevel = "WARN"
	if err := configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	if configuration.LogLevel != "warn" {
		t.Fatalf("log_level normalization = %q", configuration.LogLevel)
	}
	for _, level := range []string{"warnning", "verbose", "trace"} {
		configuration = DefaultAgent()
		configuration.LogLevel = level
		if err := configuration.Validate(); err == nil {
			t.Fatalf("log_level %q must be rejected, not coerced to info", level)
		}
	}
}

// TestAgentRejectsDeadConfigurationKeys proves the schema no longer accepts
// keys nothing reads: an operator must not believe allowed_origins restricts
// anything on a local socket API that has no browser surface.
func TestAgentRejectsDeadConfigurationKeys(t *testing.T) {
	clearAgentEnvironment(t)
	configurationPath := filepath.Join(t.TempDir(), "agent.json")
	writeConfig(t, configurationPath, strings.Replace(minimalAgentConfig, `"object_store"`, `"allowed_origins": ["http://localhost:3000"], "object_store"`, 1))
	if _, err := LoadAgent(configurationPath); err == nil {
		t.Fatal("agent.allowed_origins is dead configuration and must be refused")
	}
	writeConfigWithMode(t, configurationPath, `{"version": 1, "public_url": "https://shift.example"}`, 0o600)
	if _, err := LoadControlPlaneUnvalidated(configurationPath); err == nil {
		t.Fatal("control.public_url is dead configuration and must be refused")
	}
}

func TestAgentStateDirRefusesRootLikePaths(t *testing.T) {
	clearAgentEnvironment(t)
	for _, stateDir := range []string{"/", "/var/lib/shift/", "/var/lib/../shift", ""} {
		configuration := DefaultAgent()
		configuration.StateDir = stateDir
		if err := configuration.Validate(); err == nil {
			t.Fatalf("state_dir %q must be refused", stateDir)
		}
	}
	configuration := DefaultAgent()
	configuration.StateDir = "/srv/shift"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("a clean absolute state dir was rejected: %v", err)
	}
}

func TestAgentObjectStorePathValidation(t *testing.T) {
	clearAgentEnvironment(t)
	configuration := DefaultAgent()
	configuration.ObjectStore.Enabled = true
	configuration.ObjectStore.Backend = "local"
	configuration.ObjectStore.LocalRoot = "/"
	if err := configuration.Validate(); err == nil {
		t.Fatal("object_store.local_root=/ must be refused")
	}
	configuration.ObjectStore.LocalRoot = "/srv/shift/cloud-objects"
	if err := configuration.Validate(); err != nil {
		t.Fatalf("a clean local root was rejected: %v", err)
	}
}

func writeTestKeyPair(t *testing.T, directory string) (certificate, key, ca string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "shift-config-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate = filepath.Join(directory, "agent-cert.pem")
	key = filepath.Join(directory, "agent-key.pem")
	ca = filepath.Join(directory, "agents-ca.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	for _, entry := range []struct{ path, value string }{
		{certificate, string(certPEM)},
		{key, string(keyPEM)},
		{ca, string(certPEM)},
	} {
		writeConfigWithMode(t, entry.path, entry.value, 0o600)
	}
	return certificate, key, ca
}

// TestAgentRequiresTLSForPeerTraffic covers the deployment that passed
// startup and health and failed on the first real migration: production
// agents that dial peers (relay, remote peers, advertised agent_url, entitled
// live migration) must carry a complete, loadable TLS identity up front.
func TestAgentRequiresTLSForPeerTraffic(t *testing.T) {
	clearAgentEnvironment(t)
	for name, configure := range map[string]func(*Agent){
		"relay": func(c *Agent) {
			c.Relay = RelayConfig{Enabled: true, Endpoint: "https://relay.example:9443", CAFile: "/etc/shift/tls/relay-ca.pem"}
		},
		"remote listen": func(c *Agent) {
			c.RemoteListen = "tcp://0.0.0.0:8443"
		},
		"advertised agent url": func(c *Agent) {
			c.ControlPlane.AgentURL = "https://peer.example:8443"
		},
		"entitled live migration": func(c *Agent) {
			c.Entitlement = Entitlement{LiveMigration: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			configuration := DefaultAgent()
			configure(&configuration)
			err := configuration.Validate()
			if err == nil {
				t.Fatal("production peer traffic without TLS material must be refused")
			}
			if !strings.Contains(err.Error(), "requires mutual TLS certificate, key, and client CA") {
				t.Fatalf("error = %v, want the TLS requirement", err)
			}
			configuration.TLS.CertificateFile = "/etc/shift/tls/agent-cert.pem"
			if err := configuration.Validate(); err == nil {
				t.Fatal("a partial TLS block must be refused")
			}
			configuration.TLS.PrivateKeyFile = "/etc/shift/tls/agent-key.pem"
			configuration.TLS.ClientCAFile = "/etc/shift/tls/agents-ca.pem"
			err = configuration.Validate()
			if err == nil || !strings.Contains(err.Error(), "load tls certificate/key pair") {
				t.Fatalf("missing key-pair files must fail validation, got %v", err)
			}
		})
	}

	directory := t.TempDir()
	certificate, key, ca := writeTestKeyPair(t, directory)
	configuration := DefaultAgent()
	configuration.Relay = RelayConfig{Enabled: true, Endpoint: "https://relay.example:9443", CAFile: ca}
	configuration.TLS.CertificateFile = certificate
	configuration.TLS.PrivateKeyFile = key
	configuration.TLS.ClientCAFile = ca
	if err := configuration.Validate(); err != nil {
		t.Fatalf("complete loadable TLS material must validate: %v", err)
	}
	// Insecure development generates its identity at startup, so the
	// requirement does not apply there.
	development := DefaultAgent()
	development.InsecureDevelopment = true
	development.Relay = RelayConfig{Enabled: true, Endpoint: "https://relay.example:9443", CAFile: ca}
	if err := development.Validate(); err != nil {
		t.Fatalf("development mode must not demand TLS material up front: %v", err)
	}
}

func TestAgentRejectsMalformedEnvironmentValues(t *testing.T) {
	clearAgentEnvironment(t)
	for name, testCase := range map[string]struct {
		variable string
		value    string
	}{
		"update toggle":    {"SHIFT_UPDATE_ENABLED", "ture"},
		"update threshold": {"SHIFT_UPDATE_SIGNATURE_THRESHOLD", "tuo"},
		"update interval":  {"SHIFT_UPDATE_CHECK_INTERVAL", "6 hours"},
		"report interval":  {"SHIFT_CONTROL_REPORT_INTERVAL", "30 seconds"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(testCase.variable, testCase.value)
			_, err := LoadAgent("")
			if err == nil || !strings.Contains(err.Error(), testCase.variable) {
				t.Fatalf("an unparseable %s must fail the load, got %v", testCase.variable, err)
			}
		})
	}
	clearStorageEnvironment(t)
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	t.Setenv("SHIFT_STORAGE_CREDENTIAL_TTL", "90 minutes")
	if _, err := LoadControlPlaneUnvalidated(""); err == nil || !strings.Contains(err.Error(), "SHIFT_STORAGE_CREDENTIAL_TTL") {
		t.Fatalf("an unparseable storage TTL must fail the load, got %v", err)
	}
	t.Setenv("SHIFT_STORAGE_ENABLED", "yes-please")
	if _, err := LoadControlPlaneUnvalidated(""); err == nil || !strings.Contains(err.Error(), "SHIFT_STORAGE_ENABLED") {
		t.Fatalf("an unparseable storage toggle must fail the load, got %v", err)
	}
}

func TestTracingServiceNameEnvironmentIsSymmetric(t *testing.T) {
	clearAgentEnvironment(t)
	t.Setenv("SHIFT_OTLP_SERVICE_NAME", "shared-name")
	configuration, err := LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Tracing.ServiceName != "shared-name" {
		t.Fatalf("the agent must honor SHIFT_OTLP_SERVICE_NAME, got %q", configuration.Tracing.ServiceName)
	}
	t.Setenv("SHIFT_TRACING_SERVICE_NAME", "agent-name")
	configuration, err = LoadAgent("")
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Tracing.ServiceName != "agent-name" {
		t.Fatalf("the agent-specific name must win, got %q", configuration.Tracing.ServiceName)
	}

	clearStorageEnvironment(t)
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	t.Setenv("SHIFT_OTLP_SERVICE_NAME", "")
	t.Setenv("SHIFT_TRACING_SERVICE_NAME", "control-name")
	control, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if control.Tracing.ServiceName != "control-name" {
		t.Fatalf("the control plane must honor SHIFT_TRACING_SERVICE_NAME, got %q", control.Tracing.ServiceName)
	}
	t.Setenv("SHIFT_OTLP_SERVICE_NAME", "control-otlp-name")
	control, err = LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if control.Tracing.ServiceName != "control-otlp-name" {
		t.Fatalf("the control-plane-specific name must win, got %q", control.Tracing.ServiceName)
	}
}

func TestAllowRemoteAgentDispatchFailsClosed(t *testing.T) {
	configuration := DefaultControlPlane()
	if configuration.AllowRemoteAgentDispatch {
		t.Fatal("remote agent dispatch must default to false: dispatch goes through local_agent_socket_root only")
	}
	clearStorageEnvironment(t)
	t.Setenv("SHIFT_DATABASE_URL", "postgres://localhost:5432/shift")
	t.Setenv("SHIFT_TOKEN_PEPPER", strings.Repeat("token", 12))
	t.Setenv("SHIFT_PASSWORD_PEPPER", strings.Repeat("password", 8))
	t.Setenv("SHIFT_ALLOW_REMOTE_AGENT_DISPATCH", "true")
	loaded, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.AllowRemoteAgentDispatch {
		t.Fatal("the explicit opt-in must be honored")
	}
	if err := loaded.Validate(); err == nil || !strings.Contains(err.Error(), "agent_tls") {
		t.Fatalf("remote dispatch without operator-provided mTLS must fail closed, got %v", err)
	}
}

func TestControlPlaneListenFallsBackToPlatformPort(t *testing.T) {
	clearStorageEnvironment(t)
	t.Setenv("PORT", "8080")
	t.Setenv("SHIFT_CONTROL_LISTEN", "")
	loaded, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Listen != "0.0.0.0:8080" {
		t.Fatalf("listen = %q, want the platform port bound on all interfaces", loaded.Listen)
	}
}

func TestControlPlaneExplicitListenWinsOverPlatformPort(t *testing.T) {
	clearStorageEnvironment(t)
	t.Setenv("PORT", "8080")
	t.Setenv("SHIFT_CONTROL_LISTEN", "127.0.0.1:9090")
	loaded, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Listen != "127.0.0.1:9090" {
		t.Fatalf("listen = %q, want the explicit SHIFT_CONTROL_LISTEN value", loaded.Listen)
	}
}

func TestControlPlaneListenStaysLoopbackWithoutPlatformPort(t *testing.T) {
	clearStorageEnvironment(t)
	t.Setenv("PORT", "")
	t.Setenv("SHIFT_CONTROL_LISTEN", "")
	loaded, err := LoadControlPlaneUnvalidated("")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Listen != "127.0.0.1:8090" {
		t.Fatalf("listen = %q, want the loopback default", loaded.Listen)
	}
}

func TestControlPlaneAcceptsInlineAgentDispatchTLS(t *testing.T) {
	certPath, keyPath, caPath := writeTestKeyPair(t, t.TempDir())
	read := func(path string) string {
		material, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(material)
	}
	configuration := DefaultControlPlane()
	configuration.DatabaseURL = "postgres://localhost:5432/shift"
	configuration.TokenPepper = strings.Repeat("token", 12)
	configuration.PasswordPepper = strings.Repeat("password", 8)
	configuration.AllowRemoteAgentDispatch = true
	// Deployment environments carry TLS material as variables, not files: the
	// loaders accept inline PEM as well as file paths (same contract as the
	// relay).
	configuration.AgentTLS.CertificateFile = read(certPath)
	configuration.AgentTLS.PrivateKeyFile = read(keyPath)
	configuration.AgentTLS.PeerCAFile = read(caPath)
	if err := configuration.Validate(); err != nil {
		t.Fatalf("inline agent dispatch TLS material rejected: %v", err)
	}
}
