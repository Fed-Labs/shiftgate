// Package controlclient is the CLI's transport to the SHIFT control plane:
// session login and refresh, and typed wrappers over the organization
// endpoints the fleet CLI needs. Tokens live in a file on the operator's
// machine (0600), never in the process environment, and only the access
// token is ever sent — the refresh token is used exactly once per refresh.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"shift.dev/shift/internal/config"
)

// Client talks to one control plane. It is safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
}

// APIError carries the control plane's structured error body. The Code is the
// machine-readable contract (the same codes the OpenAPI document lists).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// New builds a client for a control-plane base URL. An empty URL means the
// platform's control plane — the platform hosts it, so its address is part of
// the product rather than per-operator configuration (overridable with
// --control-url or SHIFT_CONTROL_URL). Anything that is set but not http(s) is
// a configuration error, not a runtime one.
func New(baseURL string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = config.DefaultControlPlaneURL
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nil, fmt.Errorf("control plane URL must be http(s), got %q", baseURL)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{baseURL: baseURL, http: &http.Client{Timeout: timeout}}, nil
}

// URL is the base URL this client was built with.
func (c *Client) URL() string { return c.baseURL }

// do performs one request. accessToken may be empty for the public endpoints
// (login, plans). A non-2xx response is returned as *APIError.
func (c *Client) do(ctx context.Context, method, requestPath, accessToken string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if accessToken != "" {
		request.Header.Set("Authorization", "Bearer "+accessToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect to control plane: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeError(response)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode control-plane response: %w", err)
	}
	return nil
}

func decodeError(response *http.Response) error {
	var remote struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&remote); err == nil && remote.Message != "" {
		return &APIError{Status: response.StatusCode, Code: remote.Code, Message: remote.Message}
	}
	return &APIError{Status: response.StatusCode, Code: "HTTP_ERROR", Message: response.Status}
}
