package objectstore

import (
	"path/filepath"
	"testing"
)

func TestDefaultConfigUsesAgentStateDirectory(t *testing.T) {
	configuration := DefaultConfig("/srv/shift")
	if configuration.LocalRoot != filepath.Join("/srv/shift", "cloud-objects") {
		t.Fatalf("unexpected local root %q", configuration.LocalRoot)
	}
	if configuration.StateDir != filepath.Join("/srv/shift", "objectstore-state") {
		t.Fatalf("unexpected object-store state directory %q", configuration.StateDir)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name          string
		configuration Config
		wantError     bool
	}{
		{name: "disabled", configuration: Config{}},
		{name: "local", configuration: Config{Enabled: true, Backend: "local", LocalRoot: "/srv/objects"}},
		{name: "relative local", configuration: Config{Enabled: true, Backend: "local", LocalRoot: "objects"}, wantError: true},
		{name: "s3", configuration: Config{Enabled: true, Backend: "s3", Endpoint: "https://s3.example", Region: "eu-1", Bucket: "shift", StateDir: "/srv/state"}},
		{name: "incomplete s3", configuration: Config{Enabled: true, Backend: "s3", Region: "eu-1", Bucket: "shift", StateDir: "/srv/state"}, wantError: true},
		{name: "unknown", configuration: Config{Enabled: true, Backend: "other"}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.configuration.Validate()
			if (err != nil) != test.wantError {
				t.Fatalf("Validate() error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}

func TestApplyEnvironment(t *testing.T) {
	t.Setenv("SHIFT_OBJECTSTORE_ENABLED", "true")
	t.Setenv("SHIFT_OBJECTSTORE_BACKEND", "s3")
	t.Setenv("SHIFT_S3_ENDPOINT", "https://objects.example")
	t.Setenv("SHIFT_S3_REGION", "eu-test-1")
	t.Setenv("SHIFT_S3_BUCKET", "shift-checkpoints")
	t.Setenv("SHIFT_S3_ACCESS_KEY_ID", "access")
	t.Setenv("SHIFT_S3_SECRET_ACCESS_KEY", "secret")
	t.Setenv("SHIFT_S3_SESSION_TOKEN", "token")
	t.Setenv("SHIFT_S3_PREFIX", "tenant/one")
	t.Setenv("SHIFT_OBJECTSTORE_STATE_DIR", "/srv/shift/s3-state")
	t.Setenv("SHIFT_S3_FORCE_PATH_STYLE", "true")

	configuration := DefaultConfig("/srv/shift")
	ApplyEnvironment(&configuration)
	if !configuration.Enabled || configuration.Backend != "s3" || configuration.Endpoint != "https://objects.example" {
		t.Fatalf("environment was not applied: %+v", configuration)
	}
	if configuration.AccessKeyID != "access" || configuration.SecretAccessKey != "secret" || configuration.SessionToken != "token" {
		t.Fatalf("credentials were not applied: %+v", configuration)
	}
	if configuration.Prefix != "tenant/one" || configuration.StateDir != "/srv/shift/s3-state" || !configuration.ForcePathStyle {
		t.Fatalf("S3 options were not applied: %+v", configuration)
	}
}
