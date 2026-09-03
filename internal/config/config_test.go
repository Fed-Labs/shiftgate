package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		t.Setenv(name, "")
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
