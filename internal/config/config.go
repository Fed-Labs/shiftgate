package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/objectstore"
	"shift.dev/shift/internal/persistence"
)

const Version = "0.2.0"

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
	ObjectStore             objectstore.Config `json:"object_store"`
	ControlPlane            ControlReporter    `json:"control_plane"`
	Fencing                 FencingConfig      `json:"fencing"`
	Relay                   RelayConfig        `json:"relay"`
	Updates                 Updates            `json:"updates"`
	Tracing                 Tracing            `json:"tracing"`
	// Entitlement is the self-attested entitlement of a detached agent (no
	// control plane). It exists for air-gapped deployments; it is not a
	// license check and cannot be one — see docs/limitations.md. A managed
	// agent's entitlement comes from its control plane and overrides this.
	Entitlement Entitlement `json:"entitlement"`

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

// Entitlement is the explicit, self-attested entitlement of a detached
// agent — one with no control-plane identity to ask. It is a deployment
// assertion, not DRM: it exists so an air-gapped install can perform live
// migrations at all, and it is forgeable by whoever edits the config file.
type Entitlement struct {
	// LiveMigration permits P2P live-mode migrations on this agent. Absent
	// both this assertion and a control-plane entitlement, live mode is
	// refused: the paywall fails closed.
	LiveMigration bool `json:"live_migration"`
}

// DefaultControlPlaneURL is the control plane every SHIFT client talks to
// unless told otherwise: the platform hosts the control plane, so its address
// is part of the product, not per-deployment configuration — the same way a
// hosted service's SDK ships its endpoint. A private deployment overrides it
// with --control-url / SHIFT_CONTROL_URL (CLI, agents) or NEXT_PUBLIC_API_URL
// (dashboard).
const DefaultControlPlaneURL = "https://shiftgate.dev"

type RelayConfig struct {
	Enabled    bool   `json:"enabled"`
	Endpoint   string `json:"endpoint,omitempty"`
	CAFile     string `json:"ca_file,omitempty"`
	ServerName string `json:"server_name,omitempty"`
}

type FencingConfig struct {
	// Provider is disabled by default. Automatic failover is admitted only by
	// an external command provider that exits positively.
	Provider      string        `json:"provider,omitempty"`
	Command       string        `json:"command,omitempty"`
	StatusCommand string        `json:"status_command,omitempty"`
	Timeout       time.Duration `json:"timeout"`
}

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
		Fencing:                 FencingConfig{Provider: "disabled", Timeout: 30 * time.Second},
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
	if c.StateDir == "" {
		return errors.New("state_dir is required")
	}
	// A relative state dir (the unprivileged evaluation flow passes
	// --state-dir ./data) resolves against the working directory so the
	// derived paths below are always absolute.
	if !filepath.IsAbs(c.StateDir) {
		absolute, err := filepath.Abs(c.StateDir)
		if err != nil {
			return fmt.Errorf("state_dir: %w", err)
		}
		c.SetStateDir(absolute)
	}
	// The agent derives /updates, /cloud-objects, and /objectstore-state under
	// this root and the collector sweeps it, so a root-like or unclean value
	// (a typo like / or /var/lib/shift/..) is refused exactly like
	// cgroup_root.
	if cleaned := filepath.Clean(c.StateDir); !filepath.IsAbs(c.StateDir) || cleaned != c.StateDir || cleaned == "/" || strings.Contains(cleaned, "..") {
		return errors.New("state_dir must be a cleaned absolute path under a dedicated directory, not / or a relative traversal")
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
	// shutdown_timeout is a nanosecond duration. A zero or negative value
	// silently disabled graceful shutdown, and an unbounded one hung
	// systemctl stop until the service manager's kill, so both ends are
	// pinned here.
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = 20 * time.Second
	}
	if c.ShutdownTimeout < time.Second {
		return errors.New("shutdown_timeout must be at least one second")
	}
	if c.ShutdownTimeout > 10*time.Minute {
		return errors.New("shutdown_timeout must be no longer than ten minutes")
	}
	switch level := strings.ToLower(strings.TrimSpace(c.LogLevel)); level {
	case "":
		c.LogLevel = "info"
	case "debug", "info", "warn", "warning", "error":
		c.LogLevel = level
	default:
		return fmt.Errorf("log_level %q must be debug, info, warn, or error", c.LogLevel)
	}
	if err := c.ControlPlane.Validate(); err != nil {
		return fmt.Errorf("control_plane: %w", err)
	}
	if err := c.ObjectStore.Validate(); err != nil {
		return fmt.Errorf("object_store: %w", err)
	}
	if c.ObjectStore.Enabled {
		for _, path := range []struct{ name, value string }{
			{"object_store.local_root", c.ObjectStore.LocalRoot},
			{"object_store.state_dir", c.ObjectStore.StateDir},
		} {
			if path.value == "" {
				continue
			}
			if cleaned := filepath.Clean(path.value); !filepath.IsAbs(path.value) || cleaned != path.value || cleaned == "/" || strings.Contains(cleaned, "..") {
				return fmt.Errorf("%s must be a cleaned absolute path under a dedicated directory, not / or a relative traversal", path.name)
			}
		}
	}
	if err := c.Updates.Validate(c.StateDir); err != nil {
		return fmt.Errorf("updates: %w", err)
	}
	if err := c.Tracing.Validate(); err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	if c.Relay.Enabled {
		if strings.TrimSpace(c.Relay.Endpoint) == "" || strings.TrimSpace(c.Relay.CAFile) == "" {
			return errors.New("relay enabled requires endpoint and ca_file")
		}
		if _, err := url.Parse(c.Relay.Endpoint); err != nil {
			return fmt.Errorf("relay endpoint: %w", err)
		}
	}
	if c.Fencing.Provider == "" {
		c.Fencing.Provider = "disabled"
	}
	if c.Fencing.Provider != "disabled" && c.Fencing.Provider != "self" && c.Fencing.Provider != "command" {
		return fmt.Errorf("unsupported fencing provider %q", c.Fencing.Provider)
	}
	if c.Fencing.Provider == "command" && (strings.TrimSpace(c.Fencing.Command) == "" || strings.TrimSpace(c.Fencing.StatusCommand) == "") {
		return errors.New("command fencing requires command and status_command")
	}
	if c.Fencing.Timeout <= 0 {
		c.Fencing.Timeout = 30 * time.Second
	}
	if c.Fencing.Timeout > 5*time.Minute {
		return errors.New("fencing timeout must be no longer than five minutes")
	}
	parsed, err := url.Parse(c.Listen)
	if err != nil {
		return fmt.Errorf("parse listen URL: %w", err)
	}
	switch parsed.Scheme {
	case "unix":
		socketPath, err := unixListenPath(parsed)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(socketPath) {
			if socketPath, err = filepath.Abs(socketPath); err != nil {
				return fmt.Errorf("unix listener path: %w", err)
			}
		}
		// The listener binds this canonical form, so a relative path or a
		// typo'd URL can no longer create a socket at an unintended location.
		c.Listen = "unix://" + filepath.Clean(socketPath)
	case "tcp":
		if parsed.Host == "" {
			return errors.New("tcp listener address is required")
		}
		if !c.InsecureDevelopment {
			return errors.New("the local control API must use a Unix socket in production; configure remote_listen for peer traffic")
		}
	default:
		return fmt.Errorf("unsupported listener scheme %q", parsed.Scheme)
	}
	// A partially configured TLS block is a mistake in any mode and with any
	// listener: when any certificate setting is present, the complete server
	// set must be.
	if c.TLS.CertificateFile != "" || c.TLS.PrivateKeyFile != "" || c.TLS.ClientCAFile != "" {
		if c.TLS.CertificateFile == "" || c.TLS.PrivateKeyFile == "" || c.TLS.ClientCAFile == "" {
			return errors.New("a partial TLS block is a mistake in any mode: certificate_file, private_key_file, and client_ca_file must be configured together")
		}
	}
	if c.RemoteListen != "" {
		remote, err := url.Parse(c.RemoteListen)
		if err != nil || remote.Scheme != "tcp" || remote.Host == "" {
			return errors.New("remote_listen must be a tcp:// address")
		}
	}
	// Outbound peer traffic — relay dialing, remote peers, an advertised
	// agent_url, an entitled live migration — needs the machine's client
	// identity in production. Without it a deployment passes startup and
	// health and only fails on the first real migration, so the complete
	// trio is required (and the pair must actually load) up front.
	if !c.InsecureDevelopment && (c.Relay.Enabled || c.RemoteListen != "" || strings.TrimSpace(c.ControlPlane.AgentURL) != "" || c.Entitlement.LiveMigration) {
		if c.TLS.CertificateFile == "" || c.TLS.PrivateKeyFile == "" || c.TLS.ClientCAFile == "" {
			return errors.New("peer traffic (relay, remote_listen, control_plane.agent_url, entitlement.live_migration) requires mutual TLS certificate, key, and client CA")
		}
		certPEM, err := readPEMOrFile(c.TLS.CertificateFile)
		if err != nil {
			return fmt.Errorf("load tls certificate/key pair: %w", err)
		}
		keyPEM, err := readPEMOrFile(c.TLS.PrivateKeyFile)
		if err != nil {
			return fmt.Errorf("load tls certificate/key pair: %w", err)
		}
		if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
			return fmt.Errorf("load tls certificate/key pair: %w", err)
		}
		caPEM, err := readPEMOrFile(c.TLS.ClientCAFile)
		if err != nil {
			return fmt.Errorf("read tls client_ca_file: %w", err)
		}
		if pool := x509.NewCertPool(); !pool.AppendCertsFromPEM(caPEM) {
			return errors.New("tls client_ca_file contains no certificates")
		}
	}
	return nil
}

// readPEMOrFile returns the material a TLS setting names: inline PEM (any
// value with a PEM BEGIN line — deployment environments that carry TLS
// material as variables rather than files) or the contents of the file the
// value names. The relay and the agent-TLS loaders share this contract.
func readPEMOrFile(value string) ([]byte, error) {
	if strings.Contains(value, "-----BEGIN") {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}

// unixListenPath extracts the filesystem path from a unix:// listen URL. The
// authority is never a host: `unix://run/shift/agent.sock` (one slash short of
// the documented `unix:///run/shift/agent.sock`) used to silently bind
// /shift/agent.sock, so a non-empty authority is honored only as an explicit
// relative-path marker (`unix://./data/agent.sock`) and anything else fails
// with the canonical form in the message.
func unixListenPath(parsed *url.URL) (string, error) {
	switch {
	case parsed.Host == "":
		if parsed.Path == "" {
			return "", errors.New("unix listener path is required")
		}
		return parsed.Path, nil
	case parsed.Host == "." || parsed.Host == "..":
		return parsed.Host + parsed.Path, nil
	default:
		return "", fmt.Errorf("unix listener URL must not carry a host; write unix:///run/shift/agent.sock (three slashes), got %q", parsed.String())
	}
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
	parsedURL, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("parse URL: %w", err)
	}
	if parsedURL.Scheme != "https" && parsedURL.Scheme != "http" {
		return errors.New("URL must use http:// or https://")
	}
	if parsedURL.Scheme == "http" && parsedURL.Hostname() != "127.0.0.1" && parsedURL.Hostname() != "localhost" && parsedURL.Hostname() != "::1" {
		return errors.New("control-plane URL must use https outside local development")
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
// configuration file or environment variable are preserved. Relative values
// (the unprivileged evaluation flow passes --state-dir ./data) are resolved
// against the working directory so the rest of the loader always sees an
// absolute root.
func (c *Agent) SetStateDir(stateDir string) {
	if absolute, err := filepath.Abs(stateDir); err == nil {
		stateDir = absolute
	}
	c.StateDir = stateDir
	defaults := objectstore.DefaultConfig(stateDir)
	if !c.objectStoreLocalRootExplicit {
		c.ObjectStore.LocalRoot = defaults.LocalRoot
	}
	if !c.objectStoreStateDirExplicit {
		c.ObjectStore.StateDir = defaults.StateDir
	}
}

// checkConfigFilePermissions is the loader's gate on who may read a
// configuration file. Config files carry peppers and API keys, so a
// world-readable one is refused outright. Owner-only modes load as before.
//
// Group-readable modes (0640 as written by install.sh and systemd units) are
// accepted only on a tight ownership chain, because that is what a root-run
// package install produces and refusing it crash-loops every install path:
//
//   - the file must be owned by root or the agent's own user;
//   - group members may read but never write or execute it;
//   - the owning group must be root, the agent's own group, or the group of
//     the file's directory;
//   - the containing directory (of the file and of any symlink target) must
//     be owned by root or the agent's own user and must not be group- or
//     world-writable, so the file cannot be swapped out from under us.
//
// Anything looser fails closed with a chmod 600 hint.
func checkConfigFilePermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config file %s must be a regular file", path)
	}
	mode := info.Mode().Perm()
	if mode&0o077 == 0 {
		return nil
	}
	if mode&0o007 != 0 {
		return fmt.Errorf("config file %s has mode %04o; it must not be readable by group or other (chmod 600)", path, mode)
	}
	if mode&0o030 != 0 {
		return fmt.Errorf("config file %s has mode %04o; group members must not write or execute it (chmod 600)", path, mode)
	}
	return checkGroupReadableConfigOwnership(path, info)
}

func checkGroupReadableConfigOwnership(path string, info os.FileInfo) error {
	fileOwner, fileGroup, err := fileOwnership(info)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}
	euid, egid := os.Geteuid(), os.Getegid()
	if fileOwner != 0 && fileOwner != uint32(euid) {
		return fmt.Errorf("config file %s is group-readable and owned by uid %d; it must be owned by root or the agent user (chmod 600)", path, fileOwner)
	}
	parentPath := filepath.Dir(path)
	parent, err := os.Stat(parentPath)
	if err != nil {
		return fmt.Errorf("config file %s: %w", parentPath, err)
	}
	if !parent.IsDir() {
		return fmt.Errorf("config file %s: parent %s is not a directory", path, parentPath)
	}
	parentOwner, parentGroup, err := fileOwnership(parent)
	if err != nil {
		return fmt.Errorf("config file %s: %w", parentPath, err)
	}
	if parentOwner != 0 && parentOwner != uint32(euid) {
		return fmt.Errorf("config file %s is group-readable; its parent %s must be owned by root or the agent user (chmod 600)", path, parentPath)
	}
	if parent.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("config file %s is group-readable; its parent %s has mode %04o and must not be writable by group or other (chmod 600)", path, parentPath, parent.Mode().Perm())
	}
	if fileGroup != 0 && fileGroup != uint32(egid) && fileGroup != parentGroup {
		return fmt.Errorf("config file %s is group-readable and owned by gid %d; the group must be root, the agent group, or the parent directory's group (chmod 600)", path, fileGroup)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		resolvedParent := filepath.Dir(resolved)
		resolvedInfo, err := os.Stat(resolvedParent)
		if err != nil {
			return fmt.Errorf("config file %s: %w", resolvedParent, err)
		}
		resolvedOwner, _, err := fileOwnership(resolvedInfo)
		if err != nil {
			return fmt.Errorf("config file %s: %w", resolvedParent, err)
		}
		if resolvedOwner != 0 && resolvedOwner != uint32(euid) {
			return fmt.Errorf("config file %s is group-readable; its target's parent %s must be owned by root or the agent user (chmod 600)", path, resolvedParent)
		}
		if resolvedInfo.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("config file %s is group-readable; its target's parent %s has mode %04o and must not be writable by group or other (chmod 600)", path, resolvedParent, resolvedInfo.Mode().Perm())
		}
	}
	return nil
}

// fileOwnership reports the uid and gid recorded on a stat result. When the
// platform does not expose them the caller treats group-readable modes as
// untrusted and refuses them.
func fileOwnership(info os.FileInfo) (uid, gid uint32, err error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("file ownership is unavailable on this platform")
	}
	return stat.Uid, stat.Gid, nil
}

func LoadAgent(path string) (Agent, error) {
	configuration := DefaultAgent()
	if path != "" {
		if err := checkConfigFilePermissions(path); err != nil {
			return Agent{}, err
		}
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
	if err := applyAgentEnvironment(&configuration); err != nil {
		return Agent{}, err
	}
	err := configuration.Validate()
	return configuration, err
}

func applyAgentEnvironment(configuration *Agent) error {
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
	if err := applyControlPlaneEnvironment(&configuration.ControlPlane); err != nil {
		return err
	}
	if err := applyUpdateEnvironment(&configuration.Updates); err != nil {
		return err
	}
	if value := os.Getenv("SHIFT_OTLP_ENDPOINT"); value != "" {
		configuration.Tracing.OTLPEndpoint = value
	}
	// Both binaries accept both service-name spellings: the agent's historic
	// SHIFT_TRACING_SERVICE_NAME and the control plane's SHIFT_OTLP_SERVICE_NAME,
	// so one environment block can be copied between the two services without
	// silently dropping the name.
	if value := os.Getenv("SHIFT_TRACING_SERVICE_NAME"); value != "" {
		configuration.Tracing.ServiceName = value
	} else if value := os.Getenv("SHIFT_OTLP_SERVICE_NAME"); value != "" {
		configuration.Tracing.ServiceName = value
	}
	return nil
}

// applyStorageEnvironment maps SHIFT_STORAGE_* variables onto the hosted
// storage block. Durations accept Go duration strings ("90m"). A variable
// that was clearly set but cannot be parsed fails the load instead of
// silently keeping the default — the operator must never believe a setting
// took effect when it did not.
func applyStorageEnvironment(configuration *StorageConfig) error {
	if value := os.Getenv("SHIFT_STORAGE_ENABLED"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("SHIFT_STORAGE_ENABLED must be a boolean: %w", err)
		}
		configuration.Enabled = enabled
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
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("SHIFT_STORAGE_CREDENTIAL_TTL must be a duration: %w", err)
		}
		configuration.CredentialTTL = parsed
	}
	if value := os.Getenv("SHIFT_STORAGE_RECONCILE_INTERVAL"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("SHIFT_STORAGE_RECONCILE_INTERVAL must be a duration: %w", err)
		}
		configuration.ReconcileInterval = parsed
	}
	return nil
}

func applyControlPlaneEnvironment(configuration *ControlReporter) error {
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
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("SHIFT_CONTROL_REPORT_INTERVAL must be a duration: %w", err)
		}
		configuration.Interval = parsed
	}
	// The control plane's address is part of the platform: when the reporter
	// is being configured at all, an omitted URL means "the platform's", not
	// a validation error. A reporter configured with nothing at all stays
	// disabled, so the default never leaks into unconfigured agents.
	if strings.TrimSpace(configuration.URL) == "" &&
		(strings.TrimSpace(configuration.OrganizationID) != "" || strings.TrimSpace(configuration.MachineID) != "" || strings.TrimSpace(configuration.APIKey) != "") {
		configuration.URL = DefaultControlPlaneURL
	}
	return nil
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

type AgentClientTLSConfig struct {
	CertificateFile string `json:"certificate_file"`
	PrivateKeyFile  string `json:"private_key_file"`
	PeerCAFile      string `json:"peer_ca_file"`
	ServerName      string `json:"server_name,omitempty"`
}

type ControlPlane struct {
	Version int    `json:"version"`
	Listen  string `json:"listen"`
	// AgentTLS is the control plane's client identity for remote HTTPS agent
	// dispatch. It is never generated by SHIFT; operators provision the
	// certificate, private key, and peer CA from their fleet PKI.
	AgentTLS AgentClientTLSConfig `json:"agent_tls"`

	DatabaseURL      string `json:"database_url"`
	TokenPepper      string `json:"token_pepper"`
	PasswordPepper   string `json:"password_pepper"`
	StripeSecretKey  string `json:"stripe_secret_key,omitempty"`
	StripeWebhookKey string `json:"stripe_webhook_key,omitempty"`
	// StripePrices maps a plan key (free, pro, business, enterprise) to the
	// Stripe price id customers of that plan check out with. Checkout routes
	// exist only for plans present here. Derived keys are also honored:
	// "<plan>_annual" for annual prepay and "addon_<key>" for checkout
	// add-ons.
	StripePrices map[string]string `json:"stripe_prices,omitempty"`
	// CheckoutCoupons is the allowlist of Stripe promotion codes this control
	// plane forwards at checkout (e.g. the founding-partner code). The codes
	// themselves are operator secrets supplied via config or
	// SHIFT_CHECKOUT_COUPONS — never committed to the repository.
	CheckoutCoupons []string      `json:"checkout_coupons,omitempty"`
	SessionTTL      time.Duration `json:"session_ttl"`
	RequestTimeout  time.Duration `json:"request_timeout"`
	LogLevel        string        `json:"log_level"`
	AccessTokenTTL  time.Duration `json:"access_token_ttl"`
	RefreshTokenTTL time.Duration `json:"refresh_token_ttl"`
	// ShortRefreshTokenTTL is the refresh lifetime granted to logins that did
	// not ask to be remembered, so a browser-session login ends with its
	// refresh token instead of living on for the full remembered lifetime.
	ShortRefreshTokenTTL time.Duration `json:"short_refresh_token_ttl"`
	RateLimitPerMin      int           `json:"rate_limit_per_minute"`
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
	// GitHub sign-in ("Sign in / Sign up with GitHub"). The client id and
	// secret must be configured together or not at all; without them every
	// GitHub auth endpoint fails closed with GITHUB_NOT_CONFIGURED. The
	// optional organization allowlist restricts authentication to members of
	// those GitHub organizations (empty = any GitHub user).
	GitHub GitHub `json:"github"`

	// AllowedOrigins lists the exact browser origins that may call the API, for
	// the web dashboard ("http://localhost:3000"). A request carrying any other
	// Origin is served without CORS headers, so a browser blocks it; the CLI and
	// agents send no Origin and are unaffected. Empty allows no browser origin.
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	// LocalAgentSocketRoot limits the Unix sockets a control-plane worker may
	// dispatch through. It prevents a tenant-supplied path from turning the
	// worker into an arbitrary local-socket client.
	LocalAgentSocketRoot string `json:"local_agent_socket_root"`

	// Storage is the platform-hosted checkpoint store. When its Enabled flag is
	// off (the default) the control plane serves metadata only, exactly as
	// before, and agents that want a cloud mirror bring their own S3 bucket.
	Storage StorageConfig `json:"storage"`
	// RelaySigningKey authorizes short-lived opaque relay tickets. It never
	// leaves the control plane; relay instances receive the same secret through
	// their own secret manager configuration.
	RelaySigningKey string `json:"relay_signing_key,omitempty"`
	RelayEndpoint   string `json:"relay_endpoint,omitempty"`

	// TrustedProxies lists the reverse-proxy addresses (CIDRs or single IPs)
	// whose X-Forwarded-For / X-Real-IP headers are believed. Only when the
	// socket peer is one of these does the control plane derive the client IP
	// from forwarding headers; everyone else's headers are ignored, so a
	// client cannot forge its rate-limit bucket or its audit address. Empty
	// (the default) trusts no one: rate limiting and audit rows key on the
	// socket peer until the operator lists their TLS proxy.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
	// MetricsToken is required to scrape /metrics. Requests may carry it as a
	// bearer token or X-Metrics-Token header; an empty value fails closed.
	MetricsToken string `json:"metrics_token,omitempty"`
	// MigrationWorkerConcurrency bounds how many managed migrations one
	// control-plane replica executes at once. Claims stay lease-based; only
	// execution is parallel, so a long cold migration no longer blocks the
	// queue behind it.
	MigrationWorkerConcurrency int `json:"migration_worker_concurrency"`
	// AllowRemoteAgentDispatch lets the managed-migration worker reach a
	// source agent over its remote agent URL when no co-located Unix socket is
	// configured. It defaults to false: the documented contract is that
	// control-plane dispatch goes only through a configured
	// local_agent_socket_root and the public peer agent_url is never a
	// command backdoor, so the switch must be flipped explicitly to leave
	// that boundary.
	AllowRemoteAgentDispatch bool `json:"allow_remote_agent_dispatch"`
	// Tracing mirrors the agent's tracing block: spans always go to the
	// structured log; an OTLP endpoint additionally exports them.
	Tracing Tracing `json:"tracing"`
}

// OIDC names the identity provider the control plane federates to.
type OIDC struct {
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// GitHub names the OAuth application behind "Sign in / Sign up with GitHub".
type GitHub struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	// AllowedOrganizations restricts authentication to members of these GitHub
	// organizations. Empty (the default) admits any GitHub user. Membership is
	// read from the GitHub API during the OAuth callback; the check fails
	// closed (github_org_denied) when no allowed organization is matched.
	AllowedOrganizations []string `json:"allowed_organizations,omitempty"`
}

// Validate checks the GitHub pair: configured together or not at all. The
// organization allowlist is only meaningful with credentials, so a list
// without them is refused as a configuration mistake rather than silently
// ignored.
func (g GitHub) Validate() error {
	clientID := strings.TrimSpace(g.ClientID)
	clientSecret := strings.TrimSpace(g.ClientSecret)
	if clientID == "" && clientSecret == "" {
		if len(g.AllowedOrganizations) > 0 {
			return errors.New("github allowed organizations require a github client id and client secret")
		}
		return nil
	}
	if clientID == "" || clientSecret == "" {
		return errors.New("github client id and client secret must be configured together")
	}
	for _, organization := range g.AllowedOrganizations {
		if strings.TrimSpace(organization) == "" {
			return errors.New("github allowed organizations must not contain empty entries")
		}
	}
	return nil
}

// Configured reports whether GitHub sign-in is available at all.
func (g GitHub) Configured() bool {
	return strings.TrimSpace(g.ClientID) != "" && strings.TrimSpace(g.ClientSecret) != ""
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
		Version:                    1,
		Listen:                     "127.0.0.1:8090",
		SessionTTL:                 30 * 24 * time.Hour,
		RequestTimeout:             30 * time.Second,
		LogLevel:                   "info",
		AccessTokenTTL:             15 * time.Minute,
		RefreshTokenTTL:            30 * 24 * time.Hour,
		ShortRefreshTokenTTL:       24 * time.Hour,
		RateLimitPerMin:            120,
		ComputeTradingEnabled:      false,
		ComputeReservationTTL:      15 * time.Minute,
		RetentionSweepInterval:     time.Hour,
		LocalAgentSocketRoot:       "/run/shift",
		MigrationWorkerConcurrency: 4,
		AllowRemoteAgentDispatch:   false,
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
	// log_level is validated like every other enum-ish field: an unrecognized
	// value must fail at startup instead of silently degrading to info while
	// the operator believes debug logging is on. The accepted set is the
	// logger's ("warning" and casing included); trailing whitespace is a
	// typo, not a value — it is refused like any other misspelling.
	switch level := strings.ToLower(c.LogLevel); level {
	case "":
		c.LogLevel = "info"
	case "debug", "info", "warn", "warning", "error":
		c.LogLevel = level
	default:
		return fmt.Errorf("log_level %q must be debug, info, warn, or error", c.LogLevel)
	}
	if c.AccessTokenTTL < time.Minute || c.AccessTokenTTL > 24*time.Hour {
		return errors.New("access token TTL must be between one minute and 24 hours")
	}
	if c.RefreshTokenTTL < time.Hour || c.RefreshTokenTTL > 365*24*time.Hour {
		return errors.New("refresh token TTL must be between one hour and one year")
	}
	if c.ShortRefreshTokenTTL <= 0 {
		c.ShortRefreshTokenTTL = DefaultControlPlane().ShortRefreshTokenTTL
	}
	if c.ShortRefreshTokenTTL < time.Minute || c.ShortRefreshTokenTTL > c.RefreshTokenTTL {
		return fmt.Errorf("short refresh token TTL must be between one minute and the refresh token TTL (%s), got %s", c.RefreshTokenTTL, c.ShortRefreshTokenTTL)
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
	if strings.TrimSpace(c.LocalAgentSocketRoot) == "" {
		c.LocalAgentSocketRoot = DefaultControlPlane().LocalAgentSocketRoot
	}
	if !filepath.IsAbs(c.LocalAgentSocketRoot) || filepath.Clean(c.LocalAgentSocketRoot) == string(filepath.Separator) {
		return errors.New("local agent socket root must be an absolute non-root path")
	}
	if err := c.OIDC.Validate(); err != nil {
		return err
	}
	if err := c.GitHub.Validate(); err != nil {
		return err
	}
	if err := c.Storage.Validate(); err != nil {
		return err
	}
	if c.RelaySigningKey != "" && len(c.RelaySigningKey) < 32 {
		return errors.New("relay signing key must contain at least 32 characters when relay tickets are enabled")
	}
	for _, entry := range c.TrustedProxies {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			return errors.New("trusted proxy entries must be CIDRs or IP addresses")
		}
		if strings.Contains(trimmed, "/") {
			if _, _, err := net.ParseCIDR(trimmed); err != nil {
				return fmt.Errorf("trusted proxy %q must be a CIDR or IP address: %w", entry, err)
			}
		} else if net.ParseIP(trimmed) == nil {
			return fmt.Errorf("trusted proxy %q must be a CIDR or IP address", entry)
		}
	}
	if len(c.MetricsToken) > 0 && len(c.MetricsToken) < 16 {
		return errors.New("metrics token must contain at least 16 characters when set")
	}
	if c.AllowRemoteAgentDispatch {
		if c.AgentTLS.CertificateFile == "" || c.AgentTLS.PrivateKeyFile == "" || c.AgentTLS.PeerCAFile == "" {
			return errors.New("remote agent dispatch requires agent_tls certificate_file, private_key_file, and peer_ca_file")
		}
		certPEM, err := readPEMOrFile(c.AgentTLS.CertificateFile)
		if err != nil {
			return fmt.Errorf("load agent dispatch tls certificate/key pair: %w", err)
		}
		keyPEM, err := readPEMOrFile(c.AgentTLS.PrivateKeyFile)
		if err != nil {
			return fmt.Errorf("load agent dispatch tls certificate/key pair: %w", err)
		}
		if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
			return fmt.Errorf("load agent dispatch tls certificate/key pair: %w", err)
		}
		caPEM, err := readPEMOrFile(c.AgentTLS.PeerCAFile)
		if err != nil {
			return fmt.Errorf("read agent dispatch peer_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return errors.New("agent dispatch peer_ca_file contains no certificates")
		}
	}
	if c.MigrationWorkerConcurrency == 0 {
		c.MigrationWorkerConcurrency = DefaultControlPlane().MigrationWorkerConcurrency
	}
	if c.MigrationWorkerConcurrency < 1 || c.MigrationWorkerConcurrency > 64 {
		return errors.New("migration worker concurrency must be between 1 and 64")
	}
	if err := c.Tracing.Validate(); err != nil {
		return err
	}
	for plan, priceID := range c.StripePrices {
		if !validStripePriceKey(plan) {
			return fmt.Errorf("stripe price configured for unknown plan %q", plan)
		}
		if strings.TrimSpace(priceID) == "" || !strings.HasPrefix(priceID, "price_") {
			return fmt.Errorf("stripe price for plan %q must be a Stripe price id", plan)
		}
	}
	return nil
}

// validStripePriceKey accepts catalog plan keys plus the derived billing
// shapes: "<plan>_annual" (annual prepay) and "addon_<key>" (checkout add-ons
// like HA Pack and SourceGuard Assurance).
func validStripePriceKey(key string) bool {
	if billing.KnownPlan(key) {
		return true
	}
	if base, ok := strings.CutSuffix(key, "_annual"); ok {
		return billing.KnownPlan(base)
	}
	if addOn, ok := strings.CutPrefix(key, "addon_"); ok {
		_, found := billing.FindAddOn(addOn)
		return found
	}
	return false
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
		if err := checkConfigFilePermissions(path); err != nil {
			return ControlPlane{}, err
		}
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
	// The two Stripe secrets have environment counterparts so live billing
	// credentials never have to travel through argv, where every local user
	// can read them from the process table or shell history. The flags remain
	// for development use only.
	if value := os.Getenv("SHIFT_STRIPE_SECRET_KEY"); value != "" {
		configuration.StripeSecretKey = value
	}
	if value := os.Getenv("SHIFT_STRIPE_WEBHOOK_KEY"); value != "" {
		configuration.StripeWebhookKey = value
	}
	if value := os.Getenv("SHIFT_RELAY_SIGNING_KEY"); value != "" {
		configuration.RelaySigningKey = value
	}
	if value := os.Getenv("SHIFT_RELAY_ENDPOINT"); value != "" {
		configuration.RelayEndpoint = value
	}
	if value := os.Getenv("SHIFT_CONTROL_LISTEN"); value != "" {
		configuration.Listen = value
	} else if port := os.Getenv("PORT"); port != "" && configuration.Listen == DefaultControlPlane().Listen {
	// Platform-as-a-service convention (Heroku-style platforms): when the
	// platform supplies PORT and neither the config file nor
		// SHIFT_CONTROL_LISTEN chose a listener, bind the platform port on all
		// interfaces — the platform's edge proxy is the TLS terminator. The
		// loopback default is unchanged everywhere else.
		configuration.Listen = net.JoinHostPort("0.0.0.0", port)
	}
	// SHIFT_SHORT_REFRESH_TOKEN_TTL bounds the refresh lifetime of logins that
	// did not ask to be remembered, e.g. SHIFT_SHORT_REFRESH_TOKEN_TTL=24h.
	// Must stay between one minute and the (remembered) refresh token TTL.
	if value := os.Getenv("SHIFT_SHORT_REFRESH_TOKEN_TTL"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return ControlPlane{}, fmt.Errorf("SHIFT_SHORT_REFRESH_TOKEN_TTL must be a duration: %w", err)
		}
		configuration.ShortRefreshTokenTTL = parsed
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
	if value := os.Getenv("SHIFT_LOCAL_AGENT_SOCKET_ROOT"); value != "" {
		configuration.LocalAgentSocketRoot = value
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
	if value := os.Getenv("SHIFT_GITHUB_CLIENT_ID"); value != "" {
		configuration.GitHub.ClientID = value
	}
	if value := os.Getenv("SHIFT_GITHUB_CLIENT_SECRET"); value != "" {
		configuration.GitHub.ClientSecret = value
	}
	// SHIFT_GITHUB_ALLOWED_ORGANIZATIONS is a comma-separated allowlist of
	// GitHub organization logins; when set, only members of those
	// organizations may authenticate with GitHub.
	if value := os.Getenv("SHIFT_GITHUB_ALLOWED_ORGANIZATIONS"); value != "" {
		configuration.GitHub.AllowedOrganizations = nil
		for _, entry := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(entry); trimmed != "" {
				configuration.GitHub.AllowedOrganizations = append(configuration.GitHub.AllowedOrganizations, trimmed)
			}
		}
	}
	// SHIFT_STRIPE_PRICE_<PLAN> overrides one plan's Stripe price id, e.g.
	// SHIFT_STRIPE_PRICE_PRO=price_1234. The key is uppercased with dashes and
	// dots turned into underscores. Derived shapes accept
	// SHIFT_STRIPE_PRICE_PRO_ANNUAL and SHIFT_STRIPE_PRICE_ADDON_HA.
	priceKeys := append([]string{}, billing.PlanKeys()...)
	for _, plan := range billing.PlanKeys() {
		priceKeys = append(priceKeys, plan+"_annual")
	}
	for _, addOn := range billing.AddOnCatalog() {
		priceKeys = append(priceKeys, "addon_"+addOn.Key)
	}
	for _, plan := range priceKeys {
		if value := os.Getenv("SHIFT_STRIPE_PRICE_" + strings.ToUpper(strings.ReplaceAll(plan, "-", "_"))); value != "" {
			if configuration.StripePrices == nil {
				configuration.StripePrices = map[string]string{}
			}
			configuration.StripePrices[plan] = value
		}
	}
	// SHIFT_CHECKOUT_COUPONS is a comma-separated allowlist of Stripe
	// promotion codes (e.g. the founding-partner code). The codes live in the
	// deployment environment, never in the repository.
	if value := os.Getenv("SHIFT_CHECKOUT_COUPONS"); value != "" {
		configuration.CheckoutCoupons = nil
		for _, entry := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(entry); trimmed != "" {
				configuration.CheckoutCoupons = append(configuration.CheckoutCoupons, trimmed)
			}
		}
	}
	if value := os.Getenv("SHIFT_TRUSTED_PROXIES"); value != "" {
		configuration.TrustedProxies = nil
		for _, entry := range strings.Split(value, ",") {
			if trimmed := strings.TrimSpace(entry); trimmed != "" {
				configuration.TrustedProxies = append(configuration.TrustedProxies, trimmed)
			}
		}
	}
	if value := os.Getenv("SHIFT_METRICS_TOKEN"); value != "" {
		configuration.MetricsToken = value
	}
	if value := os.Getenv("SHIFT_MIGRATION_WORKER_CONCURRENCY"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return ControlPlane{}, fmt.Errorf("SHIFT_MIGRATION_WORKER_CONCURRENCY must be an integer: %w", err)
		}
		configuration.MigrationWorkerConcurrency = parsed
	}
	if value := os.Getenv("SHIFT_AGENT_TLS_CERT"); value != "" {
		configuration.AgentTLS.CertificateFile = value
	}
	if value := os.Getenv("SHIFT_AGENT_TLS_KEY"); value != "" {
		configuration.AgentTLS.PrivateKeyFile = value
	}
	if value := os.Getenv("SHIFT_AGENT_TLS_PEER_CA"); value != "" {
		configuration.AgentTLS.PeerCAFile = value
	}
	if value := os.Getenv("SHIFT_AGENT_TLS_SERVER_NAME"); value != "" {
		configuration.AgentTLS.ServerName = value
	}
	if value := os.Getenv("SHIFT_ALLOW_REMOTE_AGENT_DISPATCH"); value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return ControlPlane{}, fmt.Errorf("SHIFT_ALLOW_REMOTE_AGENT_DISPATCH must be a boolean: %w", err)
		}
		configuration.AllowRemoteAgentDispatch = enabled
	}
	if value := os.Getenv("SHIFT_OTLP_ENDPOINT"); value != "" {
		configuration.Tracing.OTLPEndpoint = value
	}
	// See applyAgentEnvironment: both service-name spellings are accepted by
	// both binaries so one environment block works everywhere.
	if value := os.Getenv("SHIFT_OTLP_SERVICE_NAME"); value != "" {
		configuration.Tracing.ServiceName = value
	} else if value := os.Getenv("SHIFT_TRACING_SERVICE_NAME"); value != "" {
		configuration.Tracing.ServiceName = value
	}
	if err := applyStorageEnvironment(&configuration.Storage); err != nil {
		return ControlPlane{}, err
	}
	if configuration.ComputeReservationTTL <= 0 {
		configuration.ComputeReservationTTL = DefaultControlPlane().ComputeReservationTTL
	}
	// The database URL is validated in Validate, not here: command-line
	// overrides (shift-control -database-url) land between load and
	// validation, and rejecting an empty URL at load time makes the flag
	// unusable for operators who wire the database via the CLI.
	return configuration, nil
}
