package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
	retries int
}

type Migration struct {
	ID           string         `json:"id"`
	Status       string         `json:"status"`
	Progress     map[string]any `json:"progress"`
	ErrorMessage string         `json:"error_message,omitempty"`
	CompletedAt  *time.Time     `json:"completed_at,omitempty"`
}

const migrationsPageSize = 500

type Event struct {
	ID         string    `json:"id"`
	Sequence   int64     `json:"sequence"`
	Stage      string    `json:"stage"`
	Message    string    `json:"message"`
	Progress   float64   `json:"progress"`
	BytesDone  int64     `json:"bytes_done"`
	BytesTotal int64     `json:"bytes_total"`
	CreatedAt  time.Time `json:"created_at"`
}

type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("control plane returned HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func NewClient(baseURL, token, caFile string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, errors.New("SHIFT_CONTROL_URL must use https://")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("control-plane token is empty")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, ServerName: ""}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("CA file contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return &Client{baseURL: baseURL, token: token, http: &http.Client{Transport: transport, Timeout: timeout}, retries: 3}, nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, body any, result any, idempotency string) error {
	return c.doWithRetries(ctx, method, endpoint, body, result, idempotency, c.retries)
}

func (c *Client) doWithRetries(ctx context.Context, method, endpoint string, body any, result any, idempotency string, retries int) error {
	if retries < 0 {
		retries = 0
	}
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt <= retries; attempt++ {
		var reader io.Reader
		if encoded != nil {
			reader = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, reader)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+c.token)
		request.Header.Set("Accept", "application/json")
		if encoded != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if idempotency != "" {
			request.Header.Set("Idempotency-Key", idempotency)
		}
		response, err := c.http.Do(request)
		if err != nil {
			if attempt == retries || ctx.Err() != nil {
				return err
			}
			sleep(ctx, retryDelay(attempt))
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		_ = response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
			if attempt < retries {
				sleep(ctx, retryDelay(attempt))
				continue
			}
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			var api APIError
			_ = json.Unmarshal(data, &api)
			api.Status = response.StatusCode
			if api.Message == "" {
				api.Message = strings.TrimSpace(string(data))
			}
			return &api
		}
		if result != nil && len(data) != 0 {
			if err := json.Unmarshal(data, result); err != nil {
				return fmt.Errorf("decode %s %s response: %w", method, endpoint, err)
			}
		}
		return nil
	}
	return errors.New("request retry loop exhausted")
}

// WithTimeout returns a client sharing this one's endpoints and connections
// but giving one operation its own deadline. The synchronous checkpoint
// command runs for minutes; the shared quick-operation timeout must not cut
// it off mid-dump.
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	clone := *c
	clone.http = &http.Client{Transport: c.http.Transport, Timeout: timeout}
	return &clone
}

func (c *Client) CreateMigration(ctx context.Context, organization string, body map[string]string, key string, dryRun bool) (Migration, error) {
	if dryRun {
		return Migration{Status: "dry_run", Progress: map[string]any{"request": body}}, nil
	}
	var result Migration
	err := c.do(ctx, http.MethodPost, path.Join("/v1/organizations", organization, "migrations"), body, &result, key)
	return result, err
}

// CreateCheckpoint triggers a full checkpoint of a workload through the
// control plane's machine commands endpoint, sending timeout_seconds so the
// control plane gives the dump the real budget (its own cap is ten minutes)
// instead of the 30-second default. The command is never retried: the
// commands endpoint has no durable idempotent replay yet, so a retry would
// run a second full dump of the same workload and leak checkpoint capacity.
func (c *Client) CreateCheckpoint(ctx context.Context, organization, machineID, workloadID string, timeoutSeconds int, key string) (json.RawMessage, error) {
	var result json.RawMessage
	body := map[string]any{"action": "checkpoint", "workload_id": workloadID, "timeout_seconds": timeoutSeconds}
	err := c.doWithRetries(ctx, http.MethodPost, path.Join("/v1/organizations", organization, "machines", machineID, "commands"), body, &result, key, 0)
	return result, err
}

// Migration resolves one migration job. The migrations list is paginated
// (newest first, no truncation signal), so scanning a single page reports
// "not found" for any in-flight job once ~100 newer records exist. Page
// through with an explicit limit/offset continuation until the id is found
// or the list is exhausted.
func (c *Client) Migration(ctx context.Context, organization, id string) (Migration, error) {
	for offset := 0; ; offset += migrationsPageSize {
		var jobs []Migration
		endpoint := fmt.Sprintf("%s?limit=%d&offset=%d", path.Join("/v1/organizations", organization, "migrations"), migrationsPageSize, offset)
		if err := c.do(ctx, http.MethodGet, endpoint, nil, &jobs, ""); err != nil {
			return Migration{}, err
		}
		for _, job := range jobs {
			if job.ID == id {
				return job, nil
			}
		}
		if len(jobs) < migrationsPageSize {
			return Migration{}, fmt.Errorf("migration %s was not found", id)
		}
	}
}

func (c *Client) Events(ctx context.Context, organization, id string) ([]Event, error) {
	var result []Event
	err := c.do(ctx, http.MethodGet, path.Join("/v1/organizations", organization, "migrations", id, "events"), nil, &result, "")
	return result, err
}

func (c *Client) Cancel(ctx context.Context, organization, id, key string) error {
	return c.do(ctx, http.MethodPost, path.Join("/v1/organizations", organization, "migrations", id, "cancel"), map[string]any{}, nil, key)
}

func retryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	return time.Duration(1<<attempt) * 250 * time.Millisecond
}

func sleep(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
