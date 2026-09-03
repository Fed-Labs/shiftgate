package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/persistence"
	"shift.dev/shift/internal/update"
)

// Update paths are derived from the state directory when an operator does not
// name them, so a default installation has a staging area and a backup area on
// the same filesystem as the rest of the agent's state.
const (
	updatesDirectory  = "updates"
	stagingDirectory  = "staging"
	backupsDirectory  = "backups"
	updateStateFile   = "state.json"
	maximumKeptBackup = 10
)

// Updates configures secure automatic updates. It stays disabled until an
// operator supplies both a release feed and the keys whose signatures this
// machine accepts: a machine that does not know who may sign a release must
// never install one, so there is no default feed and no default key.
type Updates struct {
	Enabled bool `json:"enabled"`
	// Channel is the release train this machine follows. A machine only ever
	// considers releases from its own channel.
	Channel update.Channel `json:"channel"`
	// Policy is how much the machine may do without an operator: report only,
	// apply mandatory releases, or apply everything applicable.
	Policy update.Policy `json:"policy"`
	// FeedURL is an https feed; FeedFile is a feed copied onto the machine, which
	// is how an air-gapped fleet is updated. Exactly one is configured, and the
	// same signature verification runs on either.
	FeedURL  string `json:"feed_url,omitempty"`
	FeedFile string `json:"feed_file,omitempty"`
	// TrustedKeys are the release-signing keys this machine accepts, either
	// inline or in a separate file that a configuration-management system can
	// distribute on its own. SignatureThreshold above one requires that many
	// distinct trusted keys to have signed a release, so a single compromised
	// signing key cannot ship an update to the fleet.
	TrustedKeys        []update.TrustedKey `json:"trusted_keys,omitempty"`
	TrustedKeysFile    string              `json:"trusted_keys_file,omitempty"`
	SignatureThreshold int                 `json:"signature_threshold"`
	CheckInterval      time.Duration       `json:"check_interval"`
	FeedTimeout        time.Duration       `json:"feed_timeout"`
	DownloadTimeout    time.Duration       `json:"download_timeout"`
	// KeepBackups is how many previous binaries stay on disk for rollback.
	KeepBackups int `json:"keep_backups"`
	// ExecutablePath is the binary an update replaces. It defaults to the running
	// executable, which is what a service manager will start again.
	ExecutablePath string `json:"executable_path,omitempty"`
	StagingDir     string `json:"staging_dir,omitempty"`
	BackupDir      string `json:"backup_dir,omitempty"`
	StateFile      string `json:"state_file,omitempty"`
}

// DefaultUpdates is the configuration of a machine that has not been told where
// its releases come from: updates are off, and the values that do have safe
// defaults are set so enabling updates needs only a feed and a key.
func DefaultUpdates() Updates {
	return Updates{
		Enabled:            false,
		Channel:            update.ChannelStable,
		Policy:             update.PolicyMandatory,
		SignatureThreshold: 1,
		CheckInterval:      6 * time.Hour,
		FeedTimeout:        30 * time.Second,
		DownloadTimeout:    10 * time.Minute,
		KeepBackups:        3,
	}
}

// Validate checks the update configuration and fills in the paths that are
// derived from the state directory. Disabled updates are still normalized, so a
// machine that is switched on later does not pick up stale values.
func (u *Updates) Validate(stateDir string) error {
	u.Channel = update.Channel(strings.ToLower(strings.TrimSpace(string(u.Channel))))
	u.Policy = update.Policy(strings.ToLower(strings.TrimSpace(string(u.Policy))))
	u.FeedURL = strings.TrimSpace(u.FeedURL)
	u.FeedFile = strings.TrimSpace(u.FeedFile)
	u.TrustedKeysFile = strings.TrimSpace(u.TrustedKeysFile)
	u.ExecutablePath = strings.TrimSpace(u.ExecutablePath)
	defaults := DefaultUpdates()
	if u.Channel == "" {
		u.Channel = defaults.Channel
	}
	if u.Policy == "" {
		u.Policy = defaults.Policy
	}
	if u.SignatureThreshold <= 0 {
		u.SignatureThreshold = defaults.SignatureThreshold
	}
	if u.CheckInterval <= 0 {
		u.CheckInterval = defaults.CheckInterval
	}
	if u.FeedTimeout <= 0 {
		u.FeedTimeout = defaults.FeedTimeout
	}
	if u.DownloadTimeout <= 0 {
		u.DownloadTimeout = defaults.DownloadTimeout
	}
	if u.KeepBackups <= 0 {
		u.KeepBackups = defaults.KeepBackups
	}
	u.applyStateDir(stateDir)
	if !u.Channel.Valid() {
		return fmt.Errorf("unsupported release channel %q", u.Channel)
	}
	if !u.Policy.Valid() {
		return fmt.Errorf("unsupported update policy %q", u.Policy)
	}
	if u.CheckInterval < update.MinimumCheckInterval {
		return fmt.Errorf("check_interval must be at least %s", update.MinimumCheckInterval)
	}
	if u.CheckInterval > 7*24*time.Hour {
		return errors.New("check_interval must be no longer than seven days")
	}
	if u.FeedTimeout > 5*time.Minute {
		return errors.New("feed_timeout must be no longer than five minutes")
	}
	if u.DownloadTimeout > 2*time.Hour {
		return errors.New("download_timeout must be no longer than two hours")
	}
	if u.KeepBackups > maximumKeptBackup {
		return fmt.Errorf("keep_backups must be no more than %d", maximumKeptBackup)
	}
	for _, path := range []struct {
		name  string
		value string
	}{
		{"executable_path", u.ExecutablePath},
		{"staging_dir", u.StagingDir},
		{"backup_dir", u.BackupDir},
		{"state_file", u.StateFile},
		{"feed_file", u.FeedFile},
		{"trusted_keys_file", u.TrustedKeysFile},
	} {
		if path.value != "" && !filepath.IsAbs(path.value) {
			return fmt.Errorf("%s must be an absolute path", path.name)
		}
	}
	if !u.Enabled {
		return nil
	}
	if u.FeedURL == "" && u.FeedFile == "" {
		return errors.New("enabling updates requires feed_url or feed_file")
	}
	if u.FeedURL != "" && u.FeedFile != "" {
		return errors.New("feed_url and feed_file are mutually exclusive")
	}
	if u.FeedURL != "" {
		parsed, err := url.Parse(u.FeedURL)
		if err != nil {
			return fmt.Errorf("parse feed_url: %w", err)
		}
		if parsed.Scheme != "https" || parsed.Host == "" {
			return errors.New("feed_url must be an absolute https url")
		}
	}
	if len(u.TrustedKeys) == 0 && u.TrustedKeysFile == "" {
		return errors.New("enabling updates requires trusted_keys or trusted_keys_file")
	}
	if u.StagingDir == u.BackupDir {
		return errors.New("staging_dir and backup_dir must be different directories")
	}
	return nil
}

// applyStateDir fills in the update paths an operator left unset. Explicit paths
// are never moved, so a deployment that stages updates on a separate filesystem
// keeps doing so.
func (u *Updates) applyStateDir(stateDir string) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return
	}
	root := filepath.Join(stateDir, updatesDirectory)
	if u.StagingDir == "" {
		u.StagingDir = filepath.Join(root, stagingDirectory)
	}
	if u.BackupDir == "" {
		u.BackupDir = filepath.Join(root, backupsDirectory)
	}
	if u.StateFile == "" {
		u.StateFile = filepath.Join(root, updateStateFile)
	}
}

// LoadTrustedKeys returns the keys this machine accepts, reading the key file
// when one is configured. Keys from the file are appended to the inline keys;
// duplicate identifiers are refused by the key ring rather than silently
// deduplicated here, so a confused configuration fails loudly.
func (u Updates) LoadTrustedKeys() ([]update.TrustedKey, error) {
	keys := make([]update.TrustedKey, 0, len(u.TrustedKeys)+1)
	keys = append(keys, u.TrustedKeys...)
	if u.TrustedKeysFile == "" {
		return keys, nil
	}
	var fromFile []update.TrustedKey
	if err := persistence.ReadJSON(u.TrustedKeysFile, &fromFile); err != nil {
		return nil, fmt.Errorf("read trusted_keys_file %s: %w", u.TrustedKeysFile, err)
	}
	if len(fromFile) == 0 {
		return nil, fmt.Errorf("trusted_keys_file %s lists no keys", u.TrustedKeysFile)
	}
	return append(keys, fromFile...), nil
}

// FeedSource builds the configured feed source.
func (u Updates) FeedSource() (update.FeedSource, error) {
	if u.FeedFile != "" {
		return update.NewFileFeedSource(u.FeedFile)
	}
	return update.NewHTTPFeedSource(u.FeedURL, u.FeedTimeout, nil)
}

// Fetcher builds the artifact fetcher matching the configured feed. A file feed
// reads artifacts from disk alongside it, which is how an air-gapped fleet is
// updated; an https feed downloads them over https. Digest verification is the
// installer's either way and is not affected by this choice.
func (u Updates) Fetcher() update.Fetcher {
	if u.FeedFile != "" {
		return update.NewFileFetcher()
	}
	return update.NewHTTPFetcher(u.DownloadTimeout, nil)
}

func applyUpdateEnvironment(configuration *Updates) {
	if value := os.Getenv("SHIFT_UPDATE_ENABLED"); value != "" {
		if enabled, err := strconv.ParseBool(value); err == nil {
			configuration.Enabled = enabled
		}
	}
	if value := os.Getenv("SHIFT_UPDATE_CHANNEL"); value != "" {
		configuration.Channel = update.Channel(value)
	}
	if value := os.Getenv("SHIFT_UPDATE_POLICY"); value != "" {
		configuration.Policy = update.Policy(value)
	}
	if value := os.Getenv("SHIFT_UPDATE_FEED_URL"); value != "" {
		configuration.FeedURL = value
	}
	if value := os.Getenv("SHIFT_UPDATE_FEED_FILE"); value != "" {
		configuration.FeedFile = value
	}
	if value := os.Getenv("SHIFT_UPDATE_TRUSTED_KEYS_FILE"); value != "" {
		configuration.TrustedKeysFile = value
	}
	if value := os.Getenv("SHIFT_UPDATE_SIGNATURE_THRESHOLD"); value != "" {
		if threshold, err := strconv.Atoi(value); err == nil {
			configuration.SignatureThreshold = threshold
		}
	}
	if value := os.Getenv("SHIFT_UPDATE_CHECK_INTERVAL"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			configuration.CheckInterval = parsed
		}
	}
	if value := os.Getenv("SHIFT_UPDATE_EXECUTABLE"); value != "" {
		configuration.ExecutablePath = value
	}
}
