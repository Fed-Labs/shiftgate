package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/observability"
	"shift.dev/shift/internal/update"
)

type Client struct {
	baseURL string
	http    *http.Client
}

type CheckpointCreateRequest struct {
	WorkloadID     string               `json:"workload_id"`
	Kind           model.CheckpointKind `json:"kind,omitempty"`
	ParentID       string               `json:"parent_id,omitempty"`
	LeaveRunning   *bool                `json:"leave_running,omitempty"`
	TCPState       bool                 `json:"tcp_state,omitempty"`
	TimeoutSeconds int                  `json:"timeout_seconds,omitempty"`
}

type ForkRequest struct {
	Name           string `json:"name,omitempty"`
	RootPath       string `json:"root_path,omitempty"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	Activate       bool   `json:"activate,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type MigrationCreateRequest struct {
	WorkloadID     string              `json:"workload_id"`
	Destination    model.Destination   `json:"destination"`
	Mode           model.MigrationMode `json:"mode,omitempty"`
	TimeoutSeconds int                 `json:"timeout_seconds,omitempty"`
}

type DoctorResponse struct {
	Healthy bool `json:"healthy"`
	Checks  []struct {
		Name    string `json:"name"`
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	} `json:"checks"`
}

func New(endpoint string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{IdleConnTimeout: 60 * time.Second}
	baseURL := endpoint
	switch parsed.Scheme {
	case "unix":
		if parsed.Path == "" {
			return nil, errors.New("agent Unix socket path is required")
		}
		socketPath := parsed.Path
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialer := net.Dialer{}
			return dialer.DialContext(ctx, "unix", socketPath)
		}
		baseURL = "http://shift-agent"
	case "http", "https":
		if parsed.Host == "" {
			return nil, errors.New("agent host is required")
		}
	case "tcp":
		if parsed.Host == "" {
			return nil, errors.New("agent TCP address is required")
		}
		baseURL = "http://" + parsed.Host
	default:
		return nil, fmt.Errorf("unsupported agent endpoint scheme %q", parsed.Scheme)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Transport: transport, Timeout: timeout}}, nil
}

func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := c.do(ctx, http.MethodGet, "/v1/health", nil, &result)
	return result, err
}

func (c *Client) Doctor(ctx context.Context) (DoctorResponse, error) {
	var result DoctorResponse
	err := c.do(ctx, http.MethodGet, "/v1/doctor", nil, &result)
	return result, err
}

func (c *Client) Machine(ctx context.Context) (model.MachineCapabilities, error) {
	var result model.MachineCapabilities
	err := c.do(ctx, http.MethodGet, "/v1/machine", nil, &result)
	return result, err
}

// Identity returns the agent's machine identity: its stable ID and the public
// key peers use to verify it.
func (c *Client) Identity(ctx context.Context) (model.MachineIdentity, error) {
	var result model.MachineIdentity
	err := c.do(ctx, http.MethodGet, "/v1/identity", nil, &result)
	return result, err
}

func (c *Client) Workloads(ctx context.Context) ([]model.Workload, error) {
	var result []model.Workload
	err := c.do(ctx, http.MethodGet, "/v1/workloads", nil, &result)
	return result, err
}

func (c *Client) Workload(ctx context.Context, id string) (model.Workload, error) {
	var result model.Workload
	err := c.do(ctx, http.MethodGet, path.Join("/v1/workloads", id), nil, &result)
	return result, err
}

func (c *Client) CreateWorkload(ctx context.Context, spec model.WorkloadSpec) (model.Workload, error) {
	var result model.Workload
	err := c.do(ctx, http.MethodPost, "/v1/workloads", spec, &result)
	return result, err
}

func (c *Client) WorkloadAction(ctx context.Context, id, action string, input any) (model.Workload, error) {
	var result model.Workload
	err := c.do(ctx, http.MethodPost, path.Join("/v1/workloads", id, action), input, &result)
	return result, err
}

func (c *Client) DeleteWorkload(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, path.Join("/v1/workloads", id), nil, nil)
}

func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	requestPath := path.Join("/v1/workloads", id, "logs")
	if tail > 0 {
		requestPath += "?tail=" + fmt.Sprint(tail)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+requestPath, nil)
	if err != nil {
		return "", err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", decodeError(response)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	return string(content), err
}

func (c *Client) Checkpoints(ctx context.Context, workloadID string) ([]checkpoint.Summary, error) {
	requestPath := "/v1/checkpoints"
	if workloadID != "" {
		requestPath += "?workload_id=" + url.QueryEscape(workloadID)
	}
	var result []checkpoint.Summary
	err := c.do(ctx, http.MethodGet, requestPath, nil, &result)
	return result, err
}

func (c *Client) Checkpoint(ctx context.Context, id string) (model.CheckpointManifest, error) {
	var result model.CheckpointManifest
	err := c.do(ctx, http.MethodGet, path.Join("/v1/checkpoints", id), nil, &result)
	return result, err
}

func (c *Client) CreateCheckpoint(ctx context.Context, input CheckpointCreateRequest) (model.CheckpointManifest, error) {
	var result model.CheckpointManifest
	err := c.do(ctx, http.MethodPost, "/v1/checkpoints", input, &result)
	return result, err
}

func (c *Client) MirrorCheckpoint(ctx context.Context, id string) (checkpoint.MirrorResult, error) {
	var result checkpoint.MirrorResult
	err := c.do(ctx, http.MethodPost, path.Join("/v1/checkpoints", id, "mirror"), nil, &result)
	return result, err
}

func (c *Client) Restore(ctx context.Context, checkpointID string, timeoutSeconds int) (checkpoint.RestoreRecord, error) {
	var result checkpoint.RestoreRecord
	input := map[string]int{"timeout_seconds": timeoutSeconds}
	err := c.do(ctx, http.MethodPost, path.Join("/v1/checkpoints", checkpointID, "restore"), input, &result)
	return result, err
}

func (c *Client) ForkWorkload(ctx context.Context, workloadID string, input ForkRequest) (checkpoint.ForkRecord, error) {
	var result checkpoint.ForkRecord
	err := c.do(ctx, http.MethodPost, path.Join("/v1/workloads", workloadID, "fork"), input, &result)
	return result, err
}

func (c *Client) ForkCheckpoint(ctx context.Context, checkpointID string, input ForkRequest) (checkpoint.ForkRecord, error) {
	var result checkpoint.ForkRecord
	err := c.do(ctx, http.MethodPost, path.Join("/v1/checkpoints", checkpointID, "fork"), input, &result)
	return result, err
}

func (c *Client) Forks(ctx context.Context) ([]checkpoint.ForkRecord, error) {
	var result []checkpoint.ForkRecord
	err := c.do(ctx, http.MethodGet, "/v1/forks", nil, &result)
	return result, err
}

func (c *Client) Fork(ctx context.Context, id string) (checkpoint.ForkRecord, error) {
	var result checkpoint.ForkRecord
	err := c.do(ctx, http.MethodGet, path.Join("/v1/forks", id), nil, &result)
	return result, err
}

func (c *Client) Migrations(ctx context.Context) ([]model.Migration, error) {
	var result []model.Migration
	err := c.do(ctx, http.MethodGet, "/v1/migrations", nil, &result)
	return result, err
}

func (c *Client) Migration(ctx context.Context, id string) (model.Migration, error) {
	var result model.Migration
	err := c.do(ctx, http.MethodGet, path.Join("/v1/migrations", id), nil, &result)
	return result, err
}

func (c *Client) CreateMigration(ctx context.Context, input MigrationCreateRequest) (model.Migration, error) {
	// A migration is the one CLI operation that spans machines: mint the root
	// trace here so the whole chain — this request, the orchestrator's run, the
	// peer calls, the destination's restore — lands in one trace view.
	if _, ok := observability.TraceFromContext(ctx); !ok {
		ctx = observability.ContextWithTrace(ctx, observability.NewTraceContext())
	}
	var result model.Migration
	err := c.do(ctx, http.MethodPost, "/v1/migrations", input, &result)
	return result, err
}

func (c *Client) CancelMigration(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, path.Join("/v1/migrations", id, "cancel"), struct{}{}, &map[string]string{})
}

// UpdateStatus reports the machine's update state: what is installed, what is
// pending a restart, what is available, and what was blocked.
func (c *Client) UpdateStatus(ctx context.Context) (update.Status, error) {
	var result update.Status
	err := c.do(ctx, http.MethodGet, "/v1/updates", nil, &result)
	return result, err
}

// UpdateCheck consults the release feed now and returns every evaluated release.
func (c *Client) UpdateCheck(ctx context.Context) (update.CheckResult, error) {
	var result update.CheckResult
	err := c.do(ctx, http.MethodPost, "/v1/updates/check", struct{}{}, &result)
	return result, err
}

// UpdateApply installs a release: the current selection when version is empty,
// or one specific version an operator chose.
func (c *Client) UpdateApply(ctx context.Context, version string) (update.Installed, error) {
	var result update.Installed
	err := c.do(ctx, http.MethodPost, "/v1/updates/apply", map[string]string{"version": version}, &result)
	return result, err
}

// UpdateRollback reinstalls a preserved binary: the most recent backup when
// backupID is empty, or one specific preserved version.
func (c *Client) UpdateRollback(ctx context.Context, backupID string) (update.Installed, error) {
	var result update.Installed
	err := c.do(ctx, http.MethodPost, "/v1/updates/rollback", map[string]string{"backup_id": backupID}, &result)
	return result, err
}

// UpdateBlock refuses one version on this machine.
func (c *Client) UpdateBlock(ctx context.Context, version, reason string) (update.Status, error) {
	var result update.Status
	err := c.do(ctx, http.MethodPost, "/v1/updates/block", map[string]string{"version": version, "reason": reason}, &result)
	return result, err
}

// UpdateUnblock allows a previously blocked version again.
func (c *Client) UpdateUnblock(ctx context.Context, version string) (update.Status, error) {
	var result update.Status
	err := c.do(ctx, http.MethodPost, "/v1/updates/unblock", map[string]string{"version": version}, &result)
	return result, err
}

// agentHint names what to check when the agent cannot be reached, for the
// three ways a Unix-socket dial actually fails: the socket not existing (the
// agent is not running), nothing accepting on it (a stale socket), and
// permission denied (the caller is not in the socket's group).
func agentHint(err error) string {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || opErr.Net != "unix" {
		return ""
	}
	switch {
	case errors.Is(err, syscall.ENOENT):
		return " — the agent socket does not exist; is shift-agent running? (systemctl status shift-agent)"
	case errors.Is(err, syscall.ECONNREFUSED):
		return " — nothing is listening on the agent socket; restart the agent (systemctl restart shift-agent)"
	case errors.Is(err, syscall.EACCES):
		return " — permission denied on the agent socket; join the 'shift' group and log in again (newgrp shift)"
	}
	return ""
}

func (c *Client) do(ctx context.Context, method, requestPath string, input, output any) error {
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
	// Propagates the caller's trace when one is in the context (a migration
	// chain); ordinary read paths carry no trace and send no header.
	observability.InjectTraceHeader(request)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("connect to SHIFT agent: %w%s", err, agentHint(err))
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
		return fmt.Errorf("decode agent response: %w", err)
	}
	return nil
}

func decodeError(response *http.Response) error {
	var remote model.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&remote); err == nil && remote.Message != "" {
		return &APIError{Status: response.StatusCode, Code: remote.Code, Message: remote.Message}
	}
	return &APIError{Status: response.StatusCode, Code: "HTTP_ERROR", Message: response.Status}
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return e.Code + ": " + e.Message
}
