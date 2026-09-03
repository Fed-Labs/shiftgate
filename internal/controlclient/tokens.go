package controlclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TokenStore persists the control-plane session on the operator's machine.
// The file is 0600 in a 0700 directory under the user's config home; tokens
// never go through the environment or command-line arguments, where they
// would leak into shell history and process listings.

type TokenStore struct {
	path string
}

// StoredSession is what the token file holds: the control plane it belongs
// to, so a stale token from another server is never replayed, plus the token
// pair and its user for display.
type StoredSession struct {
	ControlPlaneURL string    `json:"control_plane_url"`
	UserID          string    `json:"user_id"`
	Email           string    `json:"email"`
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	ExpiresAt       time.Time `json:"expires_at"`
	SavedAt         time.Time `json:"saved_at"`
}

// ErrNoSession is returned when no stored session exists.
var ErrNoSession = errors.New("no control-plane session; run shift login")

// DefaultTokenStorePath resolves ~/.config/shift/cli-session.json, honoring
// XDG_CONFIG_HOME.
func DefaultTokenStorePath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "shift", "cli-session.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "shift", "cli-session.json")
	}
	return filepath.Join(home, ".config", "shift", "cli-session.json")
}

// OpenTokenStore opens (or creates lazily) the store at a path.
func OpenTokenStore(path string) (*TokenStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("token store path is required")
	}
	return &TokenStore{path: path}, nil
}

// Load returns the stored session. A file written for a different control
// plane is treated as absent — the URL is part of the session's identity.
func (store *TokenStore) Load(controlPlaneURL string) (StoredSession, error) {
	content, err := os.ReadFile(store.path)
	if err != nil {
		if os.IsNotExist(err) {
			return StoredSession{}, ErrNoSession
		}
		return StoredSession{}, fmt.Errorf("read token store: %w", err)
	}
	var session StoredSession
	if err := json.Unmarshal(content, &session); err != nil {
		return StoredSession{}, fmt.Errorf("parse token store %s: %w", store.path, err)
	}
	if session.ControlPlaneURL != strings.TrimRight(strings.TrimSpace(controlPlaneURL), "/") {
		return StoredSession{}, ErrNoSession
	}
	return session, nil
}

// Save writes the session with restrictive permissions, creating the parent
// directory 0700 first.
func (store *TokenStore) Save(session StoredSession) error {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return fmt.Errorf("create token store directory: %w", err)
	}
	encoded, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(store.path, encoded, 0o600); err != nil {
		return fmt.Errorf("write token store: %w", err)
	}
	return nil
}

// Clear removes the stored session. A missing file is not an error — logging
// out twice is fine.
func (store *TokenStore) Clear() error {
	if err := os.Remove(store.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove token store: %w", err)
	}
	return nil
}

// Path is where the session lives.
func (store *TokenStore) Path() string { return store.path }
