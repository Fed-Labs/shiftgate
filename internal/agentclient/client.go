package agentclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/migration"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/observability"
	"shift.dev/shift/internal/update"
)

type Client struct {
	baseURL string
	http    *http.Client
	timeout time.Duration
}

// TLSOptions configures the control-plane identity used when it dispatches to
// a remote agent. The client certificate authenticates the control plane to
// the agent; PeerCAFile authenticates the agent certificate.
type TLSOptions struct {
	CertificateFile string
	PrivateKeyFile  string
	PeerCAFile      string
	ServerName      string
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

type CloneRequest struct {
	Count          int    `json:"count,omitempty"`
	NamePrefix     string `json:"name_prefix,omitempty"`
	Parallel       int    `json:"parallel,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

type MigrationCreateRequest struct {
	WorkloadID     string              `json:"workload_id"`
	IdempotencyKey string              `json:"idempotency_key,omitempty"`
	Destination    model.Destination   `json:"destination"`
	Mode           model.MigrationMode `json:"mode,omitempty"`
	PreCopyPasses  int                 `json:"pre_copy_passes,omitempty"`
	TimeoutSeconds int                 `json:"timeout_seconds,omitempty"`
}

// failoverRequest installs or clears a workload's warm-standby policy. An
// empty AgentURL with Off set clears it.
type failoverRequest struct {
	Off       bool   `json:"off,omitempty"`
	AgentURL  string `json:"agent_url,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
	KeepLast  int    `json:"keep_last,omitempty"`
}

type standbyTriggerRequest struct {
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Lazy         bool   `json:"lazy,omitempty"`
}

// StandbyTriggerResponse is the answer to an explicit failover: the duty as
// it now stands, what the pre-trigger probe observed, and — when the source
// still answered — the warning that two live copies exist.
type StandbyTriggerResponse struct {
	Duty            model.StandbyDuty `json:"duty"`
	SourceReachable string            `json:"source_reachable"`
	Warning         string            `json:"warning,omitempty"`
}

type GCRequest struct {
	GraceSeconds int  `json:"grace_seconds,omitempty"`
	DryRun       bool `json:"dry_run"`
}

type GCResponse struct {
	Plan    map[string]any `json:"plan"`
	Deleted int            `json:"deleted"`
	Skipped int            `json:"skipped"`
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
	return newClient(endpoint, timeout, TLSOptions{}, false)
}

// NewWithTLS constructs an agent client and requires HTTPS endpoints to use the
// supplied control-plane mTLS identity and peer CA. Plain HTTP and Unix socket
// endpoints retain their existing local-development behavior.
func NewWithTLS(endpoint string, timeout time.Duration, options TLSOptions) (*Client, error) {
	return newClient(endpoint, timeout, options, true)
}

func newClient(endpoint string, timeout time.Duration, options TLSOptions, requireTLS bool) (*Client, error) {
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
		if parsed.Scheme == "https" && (requireTLS || options.CertificateFile != "" || options.PrivateKeyFile != "" || options.PeerCAFile != "" || options.ServerName != "") {
			if err := configureTLS(transport, parsed, options); err != nil {
				return nil, err
			}
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
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: &http.Client{Transport: transport, Timeout: timeout}, timeout: timeout}, nil
}

func configureTLS(transport *http.Transport, parsed *url.URL, options TLSOptions) error {
	if options.CertificateFile == "" || options.PrivateKeyFile == "" || options.PeerCAFile == "" {
		return errors.New("HTTPS agent dispatch requires client certificate, private key, and peer CA")
	}
	certPEM, err := readPEMOrFile(options.CertificateFile)
	if err != nil {
		return fmt.Errorf("load agent client certificate: %w", err)
	}
	keyPEM, err := readPEMOrFile(options.PrivateKeyFile)
	if err != nil {
		return fmt.Errorf("load agent client key: %w", err)
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load agent client certificate: %w", err)
	}
	caPEM, err := readPEMOrFile(options.PeerCAFile)
	if err != nil {
		return fmt.Errorf("load agent peer CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("agent peer CA file contains no certificates")
	}
	serverName := strings.TrimSpace(options.ServerName)
	if serverName == "" {
		serverName = parsed.Hostname()
	}
	transport.TLSClientConfig = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      roots,
		ServerName:   serverName,
	}
	return nil
}

// WithTimeout returns a client sharing this one's endpoints and connections
// but giving one operation its own deadline. Synchronous long operations —
// checkpoint create, restore, fork, clone, migrate — run for minutes to
// hours; the shared default request timeout exists for the quick commands
// and must not cut those off mid-flight.
func (c *Client) WithTimeout(timeout time.Duration) *Client {
	clone := *c
	clone.http = &http.Client{Transport: c.http.Transport, Timeout: timeout}
	clone.timeout = timeout
	return &clone
}

func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	err := c.do(ctx, "health", http.MethodGet, "/v1/health", nil, &result)
	return result, err
}

func (c *Client) GarbageCollect(ctx context.Context, input GCRequest) (GCResponse, error) {
	var result GCResponse
	err := c.do(ctx, "gc", http.MethodPost, "/v1/gc", input, &result)
	return result, err
}

func (c *Client) Doctor(ctx context.Context) (DoctorResponse, error) {
	var result DoctorResponse
	err := c.do(ctx, "doctor", http.MethodGet, "/v1/doctor", nil, &result)
	return result, err
}

func (c *Client) Machine(ctx context.Context) (model.MachineCapabilities, error) {
	var result model.MachineCapabilities
	err := c.do(ctx, "machine", http.MethodGet, "/v1/machine", nil, &result)
	return result, err
}

// Identity returns the agent's machine identity: its stable ID and the public
// key peers use to verify it.
func (c *Client) Identity(ctx context.Context) (model.MachineIdentity, error) {
	var result model.MachineIdentity
	err := c.do(ctx, "machine identity", http.MethodGet, "/v1/identity", nil, &result)
	return result, err
}

func (c *Client) Workloads(ctx context.Context) ([]model.Workload, error) {
	var result []model.Workload
	err := c.do(ctx, "workload list", http.MethodGet, "/v1/workloads", nil, &result)
	return result, err
}

func (c *Client) Workload(ctx context.Context, id string) (model.Workload, error) {
	var result model.Workload
	err := c.do(ctx, "workload inspect", http.MethodGet, path.Join("/v1/workloads", id), nil, &result)
	return result, err
}

func (c *Client) CreateWorkload(ctx context.Context, spec model.WorkloadSpec) (model.Workload, error) {
	var result model.Workload
	err := c.do(ctx, "workload create", http.MethodPost, "/v1/workloads", spec, &result)
	return result, err
}

func (c *Client) WorkloadAction(ctx context.Context, id, action string, input any) (model.Workload, error) {
	var result model.Workload
	err := c.do(ctx, "workload "+action, http.MethodPost, path.Join("/v1/workloads", id, action), input, &result)
	return result, err
}

func (c *Client) DeleteWorkload(ctx context.Context, id string) error {
	return c.do(ctx, "workload delete", http.MethodDelete, path.Join("/v1/workloads", id), nil, nil)
}

// SetCheckpointPolicy installs, replaces, or clears a workload's periodic
// checkpoint schedule. A non-positive interval clears it.
func (c *Client) SetCheckpointPolicy(ctx context.Context, workloadID string, intervalSeconds, keepLast int) (model.Workload, error) {
	input := struct {
		IntervalSeconds int `json:"interval_seconds"`
		KeepLast        int `json:"keep_last,omitempty"`
	}{IntervalSeconds: intervalSeconds, KeepLast: keepLast}
	var result model.Workload
	err := c.do(ctx, "workload policy", http.MethodPost, path.Join("/v1/workloads", workloadID, "policy"), input, &result)
	return result, err
}

func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	requestPath := path.Join("/v1/workloads", id, "logs")
	if tail > 0 {
		requestPath += "?tail=" + fmt.Sprint(tail)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+requestPath, http.NoBody)
	if err != nil {
		return "", err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", c.wrapRequestError("workload logs", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", c.decodeError(response, http.MethodGet, requestPath)
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
	err := c.do(ctx, "checkpoint list", http.MethodGet, requestPath, nil, &result)
	return result, err
}

func (c *Client) Checkpoint(ctx context.Context, id string) (model.CheckpointManifest, error) {
	var result model.CheckpointManifest
	err := c.do(ctx, "checkpoint inspect", http.MethodGet, path.Join("/v1/checkpoints", id), nil, &result)
	return result, err
}

func (c *Client) CreateCheckpoint(ctx context.Context, input CheckpointCreateRequest) (model.CheckpointManifest, error) {
	var result model.CheckpointManifest
	err := c.do(ctx, "checkpoint create", http.MethodPost, "/v1/checkpoints", input, &result)
	return result, err
}

func (c *Client) MirrorCheckpoint(ctx context.Context, id string) (checkpoint.MirrorResult, error) {
	var result checkpoint.MirrorResult
	err := c.do(ctx, "checkpoint mirror", http.MethodPost, path.Join("/v1/checkpoints", id, "mirror"), nil, &result)
	return result, err
}

// RestoreRequest is what a restore caller asks for beyond the checkpoint.
type RestoreRequest struct {
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
	Lazy           bool `json:"lazy,omitempty"`
}

func (c *Client) Restore(ctx context.Context, checkpointID string, input RestoreRequest) (checkpoint.RestoreRecord, error) {
	var result checkpoint.RestoreRecord
	err := c.do(ctx, "restore", http.MethodPost, path.Join("/v1/checkpoints", checkpointID, "restore"), input, &result)
	return result, err
}

func (c *Client) ForkWorkload(ctx context.Context, workloadID string, input ForkRequest) (checkpoint.ForkRecord, error) {
	var result checkpoint.ForkRecord
	err := c.do(ctx, "fork", http.MethodPost, path.Join("/v1/workloads", workloadID, "fork"), input, &result)
	return result, err
}

func (c *Client) ForkCheckpoint(ctx context.Context, checkpointID string, input ForkRequest) (checkpoint.ForkRecord, error) {
	var result checkpoint.ForkRecord
	err := c.do(ctx, "fork", http.MethodPost, path.Join("/v1/checkpoints", checkpointID, "fork"), input, &result)
	return result, err
}

func (c *Client) Forks(ctx context.Context) ([]checkpoint.ForkRecord, error) {
	var result []checkpoint.ForkRecord
	err := c.do(ctx, "fork list", http.MethodGet, "/v1/forks", nil, &result)
	return result, err
}

func (c *Client) Fork(ctx context.Context, id string) (checkpoint.ForkRecord, error) {
	var result checkpoint.ForkRecord
	err := c.do(ctx, "fork inspect", http.MethodGet, path.Join("/v1/forks", id), nil, &result)
	return result, err
}

func (c *Client) CloneCheckpoint(ctx context.Context, checkpointID string, input CloneRequest) (checkpoint.CloneRecord, error) {
	var result checkpoint.CloneRecord
	err := c.do(ctx, "clone", http.MethodPost, path.Join("/v1/checkpoints", checkpointID, "clone"), input, &result)
	return result, err
}

func (c *Client) Clones(ctx context.Context) ([]checkpoint.CloneRecord, error) {
	var result []checkpoint.CloneRecord
	err := c.do(ctx, "clone list", http.MethodGet, "/v1/clones", nil, &result)
	return result, err
}

func (c *Client) Clone(ctx context.Context, id string) (checkpoint.CloneRecord, error) {
	var result checkpoint.CloneRecord
	err := c.do(ctx, "clone inspect", http.MethodGet, path.Join("/v1/clones", id), nil, &result)
	return result, err
}

func (c *Client) CloneRollback(ctx context.Context, id string) (checkpoint.CloneRecord, error) {
	var result checkpoint.CloneRecord
	err := c.do(ctx, "clone rollback", http.MethodPost, path.Join("/v1/clones", id, "rollback"), nil, &result)
	return result, err
}
func (c *Client) Migrations(ctx context.Context) ([]model.Migration, error) {
	var result []model.Migration
	err := c.do(ctx, "status", http.MethodGet, "/v1/migrations", nil, &result)
	return result, err
}

func (c *Client) Migration(ctx context.Context, id string) (model.Migration, error) {
	var result model.Migration
	err := c.do(ctx, "status", http.MethodGet, path.Join("/v1/migrations", id), nil, &result)
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
	err := c.do(ctx, "migrate", http.MethodPost, "/v1/migrations", input, &result)
	return result, err
}

func (c *Client) CancelMigration(ctx context.Context, id string) error {
	return c.do(ctx, "migration cancel", http.MethodPost, path.Join("/v1/migrations", id, "cancel"), struct{}{}, &map[string]string{})
}

// PreflightMigration runs a migration's checks without creating one: the
// agent reaches the destination over the peer channel, runs the same
// compatibility check the migration's validate stage would, and returns the
// report. It is the client half of `migrate --dry-run`.
func (c *Client) PreflightMigration(ctx context.Context, input MigrationCreateRequest) (migration.PreflightResult, error) {
	var result migration.PreflightResult
	err := c.do(ctx, "migrate preflight", http.MethodPost, "/v1/migrations/preflight", input, &result)
	return result, err
}

// SetFailoverPolicy installs, replaces, or clears a workload's warm-standby
// policy. An empty agentURL clears it; installing one requires the workload
// to already carry a checkpoint policy — the agent enforces the pair.
func (c *Client) SetFailoverPolicy(ctx context.Context, workloadID, agentURL, machineID string, keepLast int) (model.Workload, error) {
	input := failoverRequest{AgentURL: agentURL, MachineID: machineID, KeepLast: keepLast}
	if agentURL == "" {
		input.Off = true
	}
	var result model.Workload
	err := c.do(ctx, "workload failover", http.MethodPost, path.Join("/v1/workloads", workloadID, "failover"), input, &result)
	return result, err
}

// StandbyStatus is the standby agent's own view: the duties it holds and
// whether it can confirm a source's death on its own (a control plane is
// configured). When it cannot, armed duties never fail over automatically.
type StandbyStatus struct {
	Duties            []model.StandbyDuty `json:"duties"`
	AutomaticFailover bool                `json:"automatic_failover"`
}

// StandbyStatus lists the standby duties this agent holds: the workloads
// whose replicated state it protects and watches.
func (c *Client) StandbyStatus(ctx context.Context) (StandbyStatus, error) {
	var result StandbyStatus
	err := c.do(ctx, "standby list", http.MethodGet, "/v1/standby", nil, &result)
	return result, err
}

// TriggerStandbyFailover commands an explicit failover of one standby duty.
// An empty checkpointID restores the duty's newest replicated checkpoint. The
// call runs the restore synchronously and can take minutes.
func (c *Client) TriggerStandbyFailover(ctx context.Context, workloadID, checkpointID string, lazy bool) (StandbyTriggerResponse, error) {
	var result StandbyTriggerResponse
	input := standbyTriggerRequest{CheckpointID: checkpointID, Lazy: lazy}
	err := c.do(ctx, "standby trigger", http.MethodPost, path.Join("/v1/standby", workloadID, "trigger"), input, &result)
	return result, err
}

// ReplicationEntries lists this agent's replication ledger: for every
// workload with a failover policy, what was pushed to which standby and how
// the last attempt went.
func (c *Client) ReplicationEntries(ctx context.Context) ([]model.ReplicationEntry, error) {
	var result []model.ReplicationEntry
	err := c.do(ctx, "failover list", http.MethodGet, "/v1/failover", nil, &result)
	return result, err
}

// UpdateStatus reports the machine's update state: what is installed, what is
// pending a restart, what is available, and what was blocked.
func (c *Client) UpdateStatus(ctx context.Context) (update.Status, error) {
	var result update.Status
	err := c.do(ctx, "update status", http.MethodGet, "/v1/updates", nil, &result)
	return result, err
}

// UpdateCheck consults the release feed now and returns every evaluated release.
func (c *Client) UpdateCheck(ctx context.Context) (update.CheckResult, error) {
	var result update.CheckResult
	err := c.do(ctx, "update check", http.MethodPost, "/v1/updates/check", struct{}{}, &result)
	return result, err
}

// UpdateApply installs a release: the current selection when version is empty,
// or one specific version an operator chose.
func (c *Client) UpdateApply(ctx context.Context, version string) (update.Installed, error) {
	var result update.Installed
	err := c.do(ctx, "update apply", http.MethodPost, "/v1/updates/apply", map[string]string{"version": version}, &result)
	return result, err
}

// UpdateRollback reinstalls a preserved binary: the most recent backup when
// backupID is empty, or one specific preserved version.
func (c *Client) UpdateRollback(ctx context.Context, backupID string) (update.Installed, error) {
	var result update.Installed
	err := c.do(ctx, "update rollback", http.MethodPost, "/v1/updates/rollback", map[string]string{"backup_id": backupID}, &result)
	return result, err
}

// UpdateBlock refuses one version on this machine.
func (c *Client) UpdateBlock(ctx context.Context, version, reason string) (update.Status, error) {
	var result update.Status
	err := c.do(ctx, "update block", http.MethodPost, "/v1/updates/block", map[string]string{"version": version, "reason": reason}, &result)
	return result, err
}

// UpdateUnblock allows a previously blocked version again.
func (c *Client) UpdateUnblock(ctx context.Context, version string) (update.Status, error) {
	var result update.Status
	err := c.do(ctx, "update unblock", http.MethodPost, "/v1/updates/unblock", map[string]string{"version": version}, &result)
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

// isTimeout reports whether a request failed on a deadline rather than on
// connectivity: the http.Client.Timeout expiry, a context deadline, or a
// net-level timeout all mean "the agent did not answer in time", not "the
// agent could not be reached".
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// wrapRequestError classifies a request failure for the operator: a timeout
// says which operation ran out of time and how to raise its budget; anything
// else keeps the connectivity hint where a Unix-socket dial actually failed.
func (c *Client) wrapRequestError(op string, err error) error {
	if isTimeout(err) {
		return fmt.Errorf("timed out talking to SHIFT agent during %s after %s: %w (raise the global --timeout or the command's operation --timeout)", op, c.timeout, err)
	}
	return fmt.Errorf("talk to SHIFT agent (%s): %w%s", op, err, agentHint(err))
}

func (c *Client) do(ctx context.Context, op, method, requestPath string, input, output any) error {
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
		return c.wrapRequestError(op, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return c.decodeError(response, method, requestPath)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<20)).Decode(output); err != nil {
		return fmt.Errorf("decode agent response (%s): %w", op, err)
	}
	return nil
}

func (c *Client) decodeError(response *http.Response, method, requestPath string) error {
	var remote model.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&remote); err == nil && remote.Message != "" {
		message := remote.Message
		if message == "record not found" {
			message = "no such record; the id may be wrong or it may have been deleted"
		}
		return &APIError{Status: response.StatusCode, Code: remote.Code, Message: message}
	}
	// A non-JSON body usually means a version skew: a proxy, or an agent too
	// old to know this endpoint (or to answer it with the error envelope).
	// Name the endpoint and the remedy instead of leaking a bare status line.
	return &APIError{Status: response.StatusCode, Code: "HTTP_ERROR",
		Message: fmt.Sprintf("the agent at %s answered %s for %s %s — it may predate this CLI; check 'shiftgate update status' and 'shiftgate version'",
			c.baseURL, response.Status, method, requestPath)}
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return e.Code + ": " + e.Message
}

// readPEMOrFile returns the material a TLSOption names: inline PEM (any value
// with a PEM BEGIN line — control-plane deployments carry TLS material as
// variables rather than files) or the contents of the file the value names.
func readPEMOrFile(value string) ([]byte, error) {
	if strings.Contains(value, "-----BEGIN") {
		return []byte(value), nil
	}
	return os.ReadFile(value)
}
