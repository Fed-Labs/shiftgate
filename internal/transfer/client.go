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
		InsecureSkipVerify: true, //nolint:gosec
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
	defer response.Body.Close()
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
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		request, err := http.NewRequestWithContext(ctx, method, c.baseURL+requestPath, body)
		if err != nil {
			return err
		}
		observability.InjectTraceHeader(request)
		if input != nil {
			request.Header.Set("Content-Type", "application/json")
			if seeker, ok := body.(io.Seeker); ok {
				_, _ = seeker.Seek(0, io.SeekStart)
			}
		}
		response, err := c.http.Do(request)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				if output == nil {
					_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
					return nil
				}
				return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(output)
			}
			if response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
				return decodeRemoteError(response)
			}
			lastErr = decodeRemoteError(response)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(1<<attempt) * 200 * time.Millisecond):
		}
	}
	return lastErr
}

func decodeRemoteError(response *http.Response) error {
	var remote model.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&remote); err == nil && remote.Message != "" {
		return fmt.Errorf("peer %s: %s: %s", response.Status, remote.Code, remote.Message)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return fmt.Errorf("peer %s: %s", response.Status, strings.TrimSpace(string(body)))
}
