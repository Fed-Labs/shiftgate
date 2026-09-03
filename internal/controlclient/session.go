package controlclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Session wraps a client and a token store with one behavior: before every
// authenticated call it checks the access token's expiry and refreshes (and
// re-persists) the pair when needed. A refresh failure clears the stored
// session, because the rotation may already have consumed the old refresh
// token — a stale file is worse than no file.
type Session struct {
	client  *Client
	store   *TokenStore
	current StoredSession
}

// OpenSession loads the stored session for the default token store, resolving
// SHIFT_CONTROL_URL when no URL is given. Callers that choose their own store
// path — tests, embedders — use NewSession instead.
func OpenSession(controlPlaneURL string, timeout time.Duration) (*Session, error) {
	if strings.TrimSpace(controlPlaneURL) == "" {
		controlPlaneURL = os.Getenv("SHIFT_CONTROL_URL")
	}
	if strings.TrimSpace(controlPlaneURL) == "" {
		return nil, errors.New("no control plane configured: set SHIFT_CONTROL_URL or pass --control-url")
	}
	return NewSession(controlPlaneURL, DefaultTokenStorePath(), timeout)
}

// NewSession builds a session over a control plane and an explicit token-store
// path. It does not require a stored session to exist — login uses it before
// there is one — so a missing file is not an error here, only a session with
// no tokens yet.
func NewSession(controlPlaneURL, tokenPath string, timeout time.Duration) (*Session, error) {
	client, err := New(controlPlaneURL, timeout)
	if err != nil {
		return nil, err
	}
	store, err := OpenTokenStore(tokenPath)
	if err != nil {
		return nil, err
	}
	current, err := store.Load(client.URL())
	if err != nil && !errors.Is(err, ErrNoSession) {
		return nil, err
	}
	return &Session{client: client, store: store, current: current}, nil
}

// Client is the transport underneath.
func (session *Session) Client() *Client { return session.client }

// TokenStorePath is where this session persists. Shown after login so the
// operator knows what to protect and what to delete.
func (session *Session) TokenStorePath() string { return session.store.Path() }

// UserEmail is the identity the session was opened with.
func (session *Session) UserEmail() string { return session.current.Email }

// Authenticated reports whether this session holds tokens. NewSession succeeds
// without a stored session so login has somewhere to save; this distinguishes
// a fresh session from a usable one.
func (session *Session) Authenticated() bool { return session.current.AccessToken != "" }

// token returns a valid access token, refreshing first when the stored one is
// expired or about to be.
func (session *Session) token(ctx context.Context) (string, error) {
	if time.Until(session.current.ExpiresAt) > 30*time.Second {
		return session.current.AccessToken, nil
	}
	refreshed, err := session.client.Refresh(ctx, session.current.RefreshToken)
	if err != nil {
		_ = session.store.Clear()
		return "", fmt.Errorf("session expired and refresh failed; run shift login: %w", err)
	}
	session.current.AccessToken = refreshed.AccessToken
	session.current.RefreshToken = refreshed.RefreshToken
	session.current.ExpiresAt = refreshed.ExpiresAt
	if err := session.store.Save(session.current); err != nil {
		return "", err
	}
	return session.current.AccessToken, nil
}

// Logout revokes the session server-side and clears the token file.
func (session *Session) Logout(ctx context.Context) error {
	accessToken, err := session.token(ctx)
	if err != nil {
		// Already unusable locally; clearing is the whole job.
		return session.store.Clear()
	}
	if err := session.client.Logout(ctx, accessToken); err != nil {
		var api *APIError
		if errors.As(err, &api) && api.Status == 401 {
			return session.store.Clear()
		}
		return err
	}
	return session.store.Clear()
}

// call is the shape every authenticated method funnels through.
func (session *Session) call(ctx context.Context, method, requestPath string, input, output any) error {
	accessToken, err := session.token(ctx)
	if err != nil {
		return err
	}
	err = session.client.do(ctx, method, requestPath, accessToken, input, output)
	if err == nil {
		return nil
	}
	var api *APIError
	if errors.As(err, &api) && api.Status == 401 {
		// One forced refresh, then a single replay — the token may have been
		// rotated by another client of the same session.
		refreshed, refreshErr := session.client.Refresh(ctx, session.current.RefreshToken)
		if refreshErr != nil {
			_ = session.store.Clear()
			return fmt.Errorf("session rejected; run shift login: %w", err)
		}
		session.current.AccessToken = refreshed.AccessToken
		session.current.RefreshToken = refreshed.RefreshToken
		session.current.ExpiresAt = refreshed.ExpiresAt
		if saveErr := session.store.Save(session.current); saveErr != nil {
			return saveErr
		}
		return session.client.do(ctx, method, requestPath, refreshed.AccessToken, input, output)
	}
	return err
}

// Save persists a fresh login result into the store this session reads.
func (session *Session) Save(user User, tokens SessionTokens) error {
	session.current = StoredSession{
		ControlPlaneURL: session.client.URL(),
		UserID:          user.ID,
		Email:           user.Email,
		AccessToken:     tokens.AccessToken,
		RefreshToken:    tokens.RefreshToken,
		ExpiresAt:       tokens.ExpiresAt,
		SavedAt:         time.Now().UTC(),
	}
	return session.store.Save(session.current)
}
