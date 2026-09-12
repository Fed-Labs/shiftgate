package transfer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/observability"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(endpoint, serverName, certificateFile, privateKeyFile, caFile string) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("peer endpoint must be an https URL")
	}
	certificate, err := tls.LoadX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client identity: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("load peer CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("peer CA file has no certificates")
	}
	if serverName == "" {
		serverName = parsed.Hostname()
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
			RootCAs: roots, ServerName: serverName,
		},
		MaxIdleConns: 20, MaxIdleConnsPerHost: 10, IdleConnTimeout: 90 * time.Second,
	}
	return &Client{baseURL: strings.TrimRight(endpoint, "/"), http: &http.Client{Transport: transport, Timeout: 0}}, nil
}

func NewDevelopmentClient(endpoint, certificateFile, privateKeyFile string) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("development peer endpoint must be an https URL")
	}
	certificate, err := tls.LoadX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		// Development mode still encrypts traffic but intentionally skips peer trust validation.
		// Insecure transport is the point here: the transfer client speaks to
		// a peer agent whose identity is pinned by the migration session key,
		// not by the web PKI.
		InsecureSkipVerify: true, //nolint:gosec // peer identity is established by the session key, not TLS PKI
	}}
	return &Client{baseURL: strings.TrimRight(endpoint, "/"), http: &http.Client{Transport: transport}}, nil
}

func (c *Client) Machine(ctx context.Context) (model.MachineCapabilities, error) {
	var result model.MachineCapabilities
	err := c.doJSON(ctx, http.MethodGet, "/v1/peer/machine", nil, &result)
	return result, err
}

func (c *Client) Reserve(ctx context.Context, input ReserveRequest) (Session, error) {
	var result Session
	err := c.doJSON(ctx, http.MethodPost, "/v1/peer/transfers", input, &result)
	return result, err
}

func (c *Client) ImportKey(ctx context.Context, transferID, workloadID string, version uint32, key []byte) error {
	input := KeyRequest{WorkloadID: workloadID, KeyVersion: version, Key: base64.RawStdEncoding.EncodeToString(key)}
	return c.doJSON(ctx, http.MethodPut, path.Join("/v1/peer/transfers", transferID, "key"), input, &Session{})
}

func (c *Client) ImportManifest(ctx context.Context, transferID string, manifest model.CheckpointManifest) (MissingResponse, error) {
	var result MissingResponse
	err := c.doJSON(ctx, http.MethodPost, path.Join("/v1/peer/transfers", transferID, "manifest"), manifest, &result)
	return result, err
}

func (c *Client) UploadChunk(ctx context.Context, transferID string, ref model.ChunkRef, store *chunkstore.Store) error {
	reference, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	pipeReader, pipeWriter := io.Pipe()
	exportDone := make(chan error, 1)
	go func() {
		err := store.ExportChunk(ref, pipeWriter)
		_ = pipeWriter.CloseWithError(err)
		exportDone <- err
	}()
	endpoint := c.baseURL + path.Join("/v1/peer/transfers", transferID, "chunks", strconv.FormatUint(uint64(ref.KeyVersion), 10), ref.Address)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, pipeReader)
	if err != nil {
		_ = pipeReader.Close()
		return err
	}
	request.ContentLength = ref.StoredSize
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Shift-Chunk-Ref", base64.RawURLEncoding.EncodeToString(reference))
	observability.InjectTraceHeader(request)
	response, err := c.http.Do(request)
	if err != nil {
		_ = pipeReader.CloseWithError(err)
		<-exportDone
		return err
	}
	defer func() { _ = response.Body.Close() }()
	exportErr := <-exportDone
	if exportErr != nil {
		return exportErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return decodeRemoteError(response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return nil
}

func (c *Client) Verify(ctx context.Context, transferID string) (Session, error) {
	var result Session
	err := c.doJSON(ctx, http.MethodPost, path.Join("/v1/peer/transfers", transferID, "verify"), struct{}{}, &result)
	return result, err
}

// Hold parks a verified replication session on the standby and hands it the
// duty facts. The session returned is the standby's record of what it now
// protects; a retry after a lost response gets the same HELD session back.
func (c *Client) Hold(ctx context.Context, transferID string, keepLast int, sourceAgentURL string) (Session, error) {
	var result Session
	err := c.doJSON(ctx, http.MethodPost, path.Join("/v1/peer/transfers", transferID, "hold"), HoldRequest{
		KeepLast: keepLast, SourceAgentURL: sourceAgentURL,
	}, &result)
	return result, err
}

// Withdraw releases this machine's standby duty for a workload — the source's
// way of telling a standby it no longer holds state for it.
func (c *Client) Withdraw(ctx context.Context, workloadID string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/peer/standby/withdraw", WithdrawRequest{WorkloadID: workloadID}, nil)
}

func (c *Client) Restore(ctx context.Context, transferID string, timeout time.Duration) (checkpoint.RestoreRecord, error) {
	var result checkpoint.RestoreRecord
	input := RestoreRequest{TimeoutSeconds: int(timeout.Seconds())}
	err := c.doJSON(ctx, http.MethodPost, path.Join("/v1/peer/transfers", transferID, "restore"), input, &result)
	return result, err
}

func (c *Client) Commit(ctx context.Context, transferID string) (Session, error) {
	var result Session
	err := c.doJSON(ctx, http.MethodPost, path.Join("/v1/peer/transfers", transferID, "commit"), struct{}{}, &result)
	return result, err
}

func (c *Client) Rollback(ctx context.Context, transferID string) (Session, error) {
	var result Session
	err := c.doJSON(ctx, http.MethodPost, path.Join("/v1/peer/transfers", transferID, "rollback"), struct{}{}, &result)
	return result, err
}

func (c *Client) Get(ctx context.Context, transferID string) (Session, error) {
	var result Session
	err := c.doJSON(ctx, http.MethodGet, path.Join("/v1/peer/transfers", transferID), nil, &result)
	return result, err
}

func (c *Client) doJSON(ctx context.Context, method, requestPath string, input, output any) error {
	var encoded []byte
	if input != nil {
		var err error
		if encoded, err = json.Marshal(input); err != nil {
			return err
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		// A fresh reader per attempt: reusing one would hand NewRequest a
		// drained buffer, and the request would carry Content-Length 0 — the
		// peer would answer INVALID_JSON for a body that was never empty.
		var body io.Reader
		if encoded != nil {
			body = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, body)
		if err != nil {
			return err
		}
		observability.InjectTraceHeader(request)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := c.http.Do(request)
		if err != nil {
			lastErr = err
		} else {
			// Every attempt closes its own response body: one left open by
			// a retried request would hold its connection until this
			// function returns, and four attempts would hold four of them.
			attemptErr := consumeResponse(response, output)
			_ = response.Body.Close()
			if attemptErr == nil || !retryableStatus(response.StatusCode) {
				return attemptErr
			}
			// Only the transient classes are worth another attempt; a peer
			// that refused the request outright — quota, storage, identity —
			// answers the same way in 200ms, and every retry only delays the
			// real error from reaching the migration record.
			lastErr = attemptErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 200 * time.Millisecond):
		}
	}
	return lastErr
}

// retryableStatus reports whether a peer response describes a transient
// condition worth retrying: the classic gateway/proxy failures and explicit
// rate limiting. Everything else — including 507 STORAGE_INSUFFICIENT, a
// definitive capacity refusal — is terminal for this request.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		http.StatusTooManyRequests:
		return true
	}
	return false
}

// consumeResponse reads a response's body to its outcome — decoding output on
// success, a remote error otherwise — leaving the body ready for its caller to
// close. A successful response with no output is drained, keeping its
// connection reusable.
func consumeResponse(response *http.Response, output any) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if output == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			return nil
		}
		return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output)
	}
	return decodeRemoteError(response)
}

func decodeRemoteError(response *http.Response) error {
	var remote model.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&remote); err == nil && remote.Message != "" {
		return fmt.Errorf("peer %s: %s: %s", response.Status, remote.Code, remote.Message)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("peer %s: %s", response.Status, strings.TrimSpace(string(body)))
}
