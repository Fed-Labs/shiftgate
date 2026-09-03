package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/update"
)

func TestDefaultUpdatesAreOffAndValid(t *testing.T) {
	configuration := DefaultUpdates()
	if configuration.Enabled {
		t.Fatal("updates must be off until a feed and a trusted key are configured")
	}
	if err := configuration.Validate("/srv/shift"); err != nil {
		t.Fatalf("default updates must validate: %v", err)
	}
	if configuration.Channel != update.ChannelStable || configuration.Policy != update.PolicyMandatory {
		t.Fatalf("unexpected defaults: channel %q policy %q", configuration.Channel, configuration.Policy)
	}
	if configuration.SignatureThreshold != 1 {
		t.Fatalf("expected a signature threshold of 1, got %d", configuration.SignatureThreshold)
	}
}

func TestUpdatesDeriveTheirPathsFromTheStateDirectory(t *testing.T) {
	configuration := DefaultUpdates()
	if err := configuration.Validate("/srv/shift"); err != nil {
		t.Fatal(err)
	}
	if configuration.StagingDir != "/srv/shift/updates/staging" {
		t.Fatalf("staging directory was not derived: %q", configuration.StagingDir)
	}
	if configuration.BackupDir != "/srv/shift/updates/backups" {
		t.Fatalf("backup directory was not derived: %q", configuration.BackupDir)
	}
	if configuration.StateFile != "/srv/shift/updates/state.json" {
		t.Fatalf("state file was not derived: %q", configuration.StateFile)
	}
}

func TestUpdatesPreserveExplicitPaths(t *testing.T) {
	configuration := DefaultUpdates()
	configuration.StagingDir = "/mnt/fast/staging"
	configuration.BackupDir = "/mnt/slow/backups"
	configuration.StateFile = "/var/lib/shift/updates.json"
	if err := configuration.Validate("/srv/shift"); err != nil {
		t.Fatal(err)
	}
	if configuration.StagingDir != "/mnt/fast/staging" ||
		configuration.BackupDir != "/mnt/slow/backups" ||
		configuration.StateFile != "/var/lib/shift/updates.json" {
		t.Fatalf("explicit paths were rewritten: %+v", configuration)
	}
}

func TestUpdatesRequireAFeedAndKeysOnceEnabled(t *testing.T) {
	configuration := DefaultUpdates()
	configuration.Enabled = true
	err := configuration.Validate("/srv/shift")
	if err == nil || !strings.Contains(err.Error(), "feed_url or feed_file") {
		t.Fatalf("enabling updates without a feed must fail, got %v", err)
	}
	configuration.FeedURL = "https://releases.example/feed.json"
	err = configuration.Validate("/srv/shift")
	if err == nil || !strings.Contains(err.Error(), "trusted_keys") {
		t.Fatalf("enabling updates without a key must fail, got %v", err)
	}
	configuration.TrustedKeys = []update.TrustedKey{{PublicKeyPEM: "unchecked here"}}
	if err := configuration.Validate("/srv/shift"); err != nil {
		t.Fatalf("a feed and a key are enough: %v", err)
	}
}

func TestUpdatesRejectMalformedSettings(t *testing.T) {
	for name, mutate := range map[string]func(*Updates){
		"plaintext feed":        func(u *Updates) { u.FeedURL = "http://releases.example/feed.json" },
		"relative feed file":    func(u *Updates) { u.FeedURL = ""; u.FeedFile = "feed.json" },
		"two feeds":             func(u *Updates) { u.FeedFile = "/etc/shift/feed.json" },
		"unknown channel":       func(u *Updates) { u.Channel = "experimental" },
		"unknown policy":        func(u *Updates) { u.Policy = "whenever" },
		"impatient interval":    func(u *Updates) { u.CheckInterval = time.Minute },
		"abandoned interval":    func(u *Updates) { u.CheckInterval = 30 * 24 * time.Hour },
		"relative executable":   func(u *Updates) { u.ExecutablePath = "bin/shift-agent" },
		"relative keys file":    func(u *Updates) { u.TrustedKeysFile = "keys.json" },
		"too many backups":      func(u *Updates) { u.KeepBackups = 99 },
		"colliding directories": func(u *Updates) { u.StagingDir = "/srv/shift/both"; u.BackupDir = "/srv/shift/both" },
	} {
		configuration := DefaultUpdates()
		configuration.Enabled = true
		configuration.FeedURL = "https://releases.example/feed.json"
		configuration.TrustedKeys = []update.TrustedKey{{PublicKeyPEM: "unchecked here"}}
		mutate(&configuration)
		if err := configuration.Validate("/srv/shift"); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestUpdatesLoadTrustedKeysFromAFile(t *testing.T) {
	directory := t.TempDir()
	keysPath := filepath.Join(directory, "keys.json")
	writeConfig(t, keysPath, `[{"public_key_pem": "from the file", "comment": "publisher"}]`)
	configuration := DefaultUpdates()
	configuration.TrustedKeys = []update.TrustedKey{{PublicKeyPEM: "inline"}}
	configuration.TrustedKeysFile = keysPath
	keys, err := configuration.LoadTrustedKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].PublicKeyPEM != "inline" || keys[1].PublicKeyPEM != "from the file" {
		t.Fatalf("inline and file keys must both be honoured: %+v", keys)
	}
}

func TestUpdatesRefuseAnEmptyOrMissingKeyFile(t *testing.T) {
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.json")
	writeConfig(t, empty, `[]`)
	configuration := DefaultUpdates()
	configuration.TrustedKeysFile = empty
	if _, err := configuration.LoadTrustedKeys(); err == nil {
		t.Fatal("a key file listing no keys must be refused")
	}
	configuration.TrustedKeysFile = filepath.Join(directory, "absent.json")
	if _, err := configuration.LoadTrustedKeys(); err == nil {
		t.Fatal("a missing key file must be refused")
	}
}

func TestUpdatesBuildTheConfiguredFeedSource(t *testing.T) {
	configuration := DefaultUpdates()
	configuration.FeedURL = "https://releases.example/feed.json"
	source, err := configuration.FeedSource()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source.(*update.HTTPFeedSource); !ok {
		t.Fatalf("expected an https feed source, got %T", source)
	}
	configuration.FeedURL = ""
	configuration.FeedFile = "/etc/shift/feed.json"
	source, err = configuration.FeedSource()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source.(*update.FileFeedSource); !ok {
		t.Fatalf("expected a file feed source, got %T", source)
	}
}

func TestAgentConfigurationCarriesUpdates(t *testing.T) {
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
  "object_store": {"enabled": false, "backend": "local"},
  "updates": {"enabled": true, "channel": "beta", "policy": "automatic",
    "feed_url": "https://releases.example/feed.json",
    "trusted_keys": [{"public_key_pem": "unchecked here"}],
    "check_interval": 3600000000000}
}`)

	configuration, err := LoadAgent(configurationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.Updates.Enabled || configuration.Updates.Channel != update.ChannelBeta {
		t.Fatalf("update block was not loaded: %+v", configuration.Updates)
	}
	if configuration.Updates.StagingDir != "/srv/shift/updates/staging" {
		t.Fatalf("update paths were not derived from the state directory: %q", configuration.Updates.StagingDir)
	}
}

func TestAgentUpdateEnvironmentOverrides(t *testing.T) {
	clearAgentEnvironment(t)
	t.Setenv("SHIFT_UPDATE_ENABLED", "true")
	t.Setenv("SHIFT_UPDATE_CHANNEL", "nightly")
	t.Setenv("SHIFT_UPDATE_POLICY", "automatic")
	t.Setenv("SHIFT_UPDATE_FEED_URL", "https://nightly.example/feed.json")
	t.Setenv("SHIFT_UPDATE_TRUSTED_KEYS_FILE", "/etc/shift/keys.json")
	t.Setenv("SHIFT_UPDATE_SIGNATURE_THRESHOLD", "2")
	t.Setenv("SHIFT_UPDATE_CHECK_INTERVAL", "45m")
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
	updates := configuration.Updates
	if !updates.Enabled || updates.Channel != update.ChannelNightly || updates.Policy != update.PolicyAutomatic {
		t.Fatalf("environment did not override the update block: %+v", updates)
	}
	if updates.FeedURL != "https://nightly.example/feed.json" || updates.TrustedKeysFile != "/etc/shift/keys.json" {
		t.Fatalf("environment did not override the feed or key file: %+v", updates)
	}
	if updates.SignatureThreshold != 2 || updates.CheckInterval != 45*time.Minute {
		t.Fatalf("environment did not override the threshold or interval: %+v", updates)
	}
}
