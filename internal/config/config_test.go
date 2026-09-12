package config

import (
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
		"SHIFT_OTLP_ENDPOINT", "SHIFT_TRACING_SERVICE_NAME",
		"SHIFT_CONTROL_URL", "SHIFT_CONTROL_ORGANIZATION_ID", "SHIFT_CONTROL_MACHINE_ID",
		"SHIFT_CONTROL_MACHINE_NAME", "SHIFT_CONTROL_AGENT_URL", "SHIFT_CONTROL_API_KEY",
		"SHIFT_CONTROL_REPORT_INTERVAL", "SHIFT_CGROUP_ROOT",
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
