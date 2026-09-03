package controlclient

import (
	"context"
	"net/url"
	"time"

	"shift.dev/shift/internal/billing"
)

// User is the authenticated account as the control plane reports it.
type User struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	DisplayName string     `json:"display_name"`
	DisabledAt  *time.Time `json:"disabled_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Organization is one organization the user belongs to.
type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// SessionTokens is the login or refresh result. The access token is
// short-lived; the refresh token rotates on every refresh.
type SessionTokens struct {
	SessionID    string    `json:"session_id"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Login exchanges credentials for a session. The tokens are returned to the
// caller and are not stored by the client; persistence is the token store's
// business.
func (c *Client) Login(ctx context.Context, email, password string) (User, SessionTokens, error) {
	var result struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	err := c.do(ctx, "POST", "/v1/auth/login", "", map[string]string{"email": email, "password": password}, &result)
	return result.User, result.Tokens, err
}

// Refresh rotates the session's token pair. A used refresh token is
// invalidated by the rotation, so the caller must persist the new pair.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (SessionTokens, error) {
	var result SessionTokens
	err := c.do(ctx, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": refreshToken}, &result)
	return result, err
}

// Logout revokes the session behind the access token.
func (c *Client) Logout(ctx context.Context, accessToken string) error {
	return c.do(ctx, "POST", "/v1/auth/logout", accessToken, nil, nil)
}

// Me returns the principal the access token resolves to.
func (c *Client) Me(ctx context.Context, accessToken string) (User, error) {
	var result User
	err := c.do(ctx, "GET", "/v1/me", accessToken, nil, &result)
	return result, err
}

// Organizations lists the caller's organizations.
func (c *Client) Organizations(ctx context.Context, accessToken string) ([]Organization, error) {
	var result []Organization
	err := c.do(ctx, "GET", "/v1/organizations", accessToken, nil, &result)
	return result, err
}

// Organizations lists the organizations the session's user belongs to. The
// first is the one fleet commands operate on unless the caller says otherwise.
func (session *Session) Organizations(ctx context.Context) ([]Organization, error) {
	var result []Organization
	return result, session.call(ctx, "GET", "/v1/organizations", nil, &result)
}

// Health probes the control plane without credentials.
func (c *Client) Health(ctx context.Context) error {
	var result map[string]any
	return c.do(ctx, "GET", "/health", "", nil, &result)
}

// Plans fetches the public plan catalog. No session is needed: the catalog is
// what the entitlement checks enforce, so pricing and limits are public by
// design and carry no account data.
func (c *Client) Plans(ctx context.Context, output *[]billing.Plan) error {
	return c.do(ctx, "GET", "/v1/plans", "", nil, output)
}

// Login authenticates and stores the resulting session: the pair is persisted
// through the token store before it is returned to the caller, so a login that
// cannot be saved is a login that did not happen.
func (session *Session) Login(ctx context.Context, email, password string) (User, error) {
	user, tokens, err := session.client.Login(ctx, email, password)
	if err != nil {
		return User{}, err
	}
	if err := session.Save(user, tokens); err != nil {
		return User{}, err
	}
	return user, nil
}

// SSOAuthorization is the browser half of a single sign-on login: where to send
// the user, and the state the issuer must echo back.
type SSOAuthorization struct {
	AuthorizationURL string `json:"authorization_url"`
	State            string `json:"state"`
}

// BeginSSO asks the control plane for the issuer's authorization URL. The
// challenge is the S256 digest of the verifier the caller generated and keeps
// secret until the code is redeemed; the redirect URI must be the caller's
// loopback listener, sent identically again at redemption.
func (c *Client) BeginSSO(ctx context.Context, email, redirectURI, challenge string) (SSOAuthorization, error) {
	var result SSOAuthorization
	err := c.do(ctx, "POST", "/v1/auth/sso/authorize", "", map[string]string{"email": email, "redirect_uri": redirectURI, "code_challenge": challenge}, &result)
	return result, err
}

// CompleteSSO redeems the authorization code the loopback listener received.
func (c *Client) CompleteSSO(ctx context.Context, code, state, verifier, redirectURI string) (User, SessionTokens, error) {
	var result struct {
		User   User          `json:"user"`
		Tokens SessionTokens `json:"tokens"`
	}
	err := c.do(ctx, "POST", "/v1/auth/sso/token", "", map[string]string{"code": code, "state": state, "code_verifier": verifier, "redirect_uri": redirectURI}, &result)
	return result.User, result.Tokens, err
}

// LoginSSO finishes a single sign-on login and stores the session, mirroring
// Login: a session that cannot be saved is a session that did not happen.
func (session *Session) LoginSSO(ctx context.Context, code, state, verifier, redirectURI string) (User, error) {
	user, tokens, err := session.client.CompleteSSO(ctx, code, state, verifier, redirectURI)
	if err != nil {
		return User{}, err
	}
	if err := session.Save(user, tokens); err != nil {
		return User{}, err
	}
	return user, nil
}

// SSORequired reports whether an email's domain is under SSO enforcement, so a
// client can route to the issuer before asking for a password that cannot
// succeed.
func (c *Client) SSORequired(ctx context.Context, email string) (bool, error) {
	var result struct {
		Required bool `json:"required"`
	}
	err := c.do(ctx, "GET", "/v1/auth/sso/required?email="+url.QueryEscape(email), "", nil, &result)
	return result.Required, err
}
