package objectstore

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ControlPlaneBackend names the object-store backend whose credentials arrive
// from the control plane at runtime instead of static configuration.
const ControlPlaneBackend = "control-plane"

type Config struct {
	Enabled         bool   `json:"enabled"`
	Backend         string `json:"backend"`
	LocalRoot       string `json:"local_root,omitempty"`
	Endpoint        string `json:"endpoint,omitempty"`
	Region          string `json:"region,omitempty"`
	Bucket          string `json:"bucket,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	StateDir        string `json:"state_dir,omitempty"`
	ForcePathStyle  bool   `json:"force_path_style,omitempty"`
}

func DefaultConfig(stateDir string) Config {
	return Config{Enabled: false, Backend: "local", LocalRoot: filepath.Join(stateDir, "cloud-objects"), StateDir: filepath.Join(stateDir, "objectstore-state")}
}

func (configuration Config) Validate() error {
	if !configuration.Enabled {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(configuration.Backend)) {
	case "local":
		if configuration.LocalRoot == "" || !filepath.IsAbs(configuration.LocalRoot) {
			return errors.New("local object-store root must be an absolute path")
		}
	case "s3":
		if configuration.Endpoint == "" || configuration.Region == "" || configuration.Bucket == "" {
			return errors.New("S3 endpoint, region, and bucket are required")
		}
		if configuration.StateDir == "" || !filepath.IsAbs(configuration.StateDir) {
			return errors.New("S3 state directory must be an absolute path")
		}
	case "control-plane":
		// The broker client's credentials, endpoint, and bucket all arrive from
		// the control plane at runtime; only the multipart state directory is
		// the agent's own. The control-plane reporter's presence is validated
		// by the agent config, which owns that block.
		if configuration.StateDir == "" || !filepath.IsAbs(configuration.StateDir) {
			return errors.New("control-plane object store requires an absolute state directory for multipart state")
		}
	default:
		return errors.New("object-store backend must be local, s3, or control-plane")
	}
	return nil
}

func (configuration Config) Open() (Store, error) {
	if !configuration.Enabled {
		return nil, nil
	}
	if err := configuration.Validate(); err != nil {
		return nil, err
	}
	switch strings.ToLower(strings.TrimSpace(configuration.Backend)) {
	case "local":
		return OpenLocal(configuration.LocalRoot)
	case "s3":
		return OpenS3(S3Config{
			Endpoint: configuration.Endpoint, Region: configuration.Region, Bucket: configuration.Bucket,
			AccessKeyID: configuration.AccessKeyID, SecretAccessKey: configuration.SecretAccessKey,
			SessionToken: configuration.SessionToken, Prefix: configuration.Prefix,
			StateDir: configuration.StateDir, ForcePathStyle: configuration.ForcePathStyle,
		})
	case "control-plane":
		// The broker store is constructed by the agent's hosted-storage loop
		// once the control plane issues credentials, so opening one directly
		// from static configuration is a configuration mistake.
		return nil, errors.New("the control-plane object store is opened by the agent from issued credentials, not from static configuration")
	default:
		return nil, ErrUnsupported
	}
}

func ApplyEnvironment(configuration *Config) {
	if value := os.Getenv("SHIFT_OBJECTSTORE_ENABLED"); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			configuration.Enabled = parsed
		}
	}
	if value := os.Getenv("SHIFT_OBJECTSTORE_BACKEND"); value != "" {
		configuration.Backend = value
	}
	if value := os.Getenv("SHIFT_OBJECTSTORE_LOCAL_ROOT"); value != "" {
		configuration.LocalRoot = value
	}
	if value := os.Getenv("SHIFT_S3_ENDPOINT"); value != "" {
		configuration.Endpoint = value
	}
	if value := os.Getenv("SHIFT_S3_REGION"); value != "" {
		configuration.Region = value
	}
	if value := os.Getenv("SHIFT_S3_BUCKET"); value != "" {
		configuration.Bucket = value
	}
	if value := os.Getenv("SHIFT_S3_ACCESS_KEY_ID"); value != "" {
		configuration.AccessKeyID = value
	}
	if value := os.Getenv("SHIFT_S3_SECRET_ACCESS_KEY"); value != "" {
		configuration.SecretAccessKey = value
	}
	if value := os.Getenv("SHIFT_S3_SESSION_TOKEN"); value != "" {
		configuration.SessionToken = value
	}
	if value := os.Getenv("SHIFT_S3_PREFIX"); value != "" {
		configuration.Prefix = value
	}
	if value := os.Getenv("SHIFT_OBJECTSTORE_STATE_DIR"); value != "" {
		configuration.StateDir = value
	}
	if value := os.Getenv("SHIFT_S3_FORCE_PATH_STYLE"); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			configuration.ForcePathStyle = parsed
		}
	}
}
