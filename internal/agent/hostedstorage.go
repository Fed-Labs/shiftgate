package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"shift.dev/shift/internal/objectstore"
)

// hostedstorage.go: the agent side of platform-hosted checkpoint storage.
// When object_store.backend is "control-plane", the agent has no static S3
// configuration — it polls its control plane with the machines-scope API key
// the reporter already carries, receives short-lived org-scoped credentials,
// and swaps them into the mirror's Deferred store. Before the first issuance
// every mirror operation fails closed with "credentials not yet available";
// an over-quota organization stops issuance, and with it mirroring, while
// local checkpointing continues untouched.

// ErrStorageQuotaExceeded is surfaced distinctly so checkpoint errors can say
// why the mirror stopped: the organization is past its storage plan.
var ErrStorageQuotaExceeded = errors.New("organization checkpoint storage quota is exceeded")

// ErrStorageNotConfigured means the control plane answered that it does not
// host storage — the deployment model is self-hosted mirroring, and the agent
// configuration does not match it.
var ErrStorageNotConfigured = errors.New("the control plane does not host checkpoint storage")

// hostedStorageConfig is everything the loop needs from the agent's static
// configuration.
type hostedStorageConfig struct {
	controlURL     string
	organization   string
	apiKey         string
	stateDir       string
	requestTimeout time.Duration
}

type hostedStorageLoop struct {
	configuration hostedStorageConfig
	store         *objectstore.Deferred
	logger        *slog.Logger
	client        *http.Client

	// issuedConfig is the location half of the last credential response —
	// endpoint, region, bucket, prefix — set by fetch. The secret half lives
	// only inside the store and is never logged or persisted.
	issuedConfig *issuedStorageConfig

	mu            sync.Mutex
	installed     *objectstore.S3
	expiration    time.Time
	lastQuota     bool
	credentialsOK bool
}

// issuedStorageConfig is the non-secret location configuration a credential
// response carried.
type issuedStorageConfig struct {
	Endpoint       string
	Region         string
	Bucket         string
	Prefix         string
	ForcePathStyle bool
}

func newHostedStorageLoop(configuration hostedStorageConfig, store *objectstore.Deferred, logger *slog.Logger) *hostedStorageLoop {
	if logger == nil {
		logger = slog.Default()
	}
	timeout := configuration.requestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &hostedStorageLoop{
		configuration: configuration,
		store:         store,
		logger:        logger,
		client:        &http.Client{Timeout: timeout},
	}
}

// run polls until the context ends: fetch once at start (so the deferred store
// fills as early as possible), then again when the current credential set is
// half-way to expiry, capped at thirty minutes. Failure responses do not stop
// the loop — the previous credentials keep serving until they expire, after
// which mirrors fail honestly with the underlying error.
func (loop *hostedStorageLoop) run(ctx context.Context) {
	loop.fetchAndApply(ctx)
	for {
		loop.mu.Lock()
		wait := loop.refreshDelay()
		loop.mu.Unlock()
		if wait <= 0 {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			loop.fetchAndApply(ctx)
		}
	}
}

// refreshDelay is how long to sleep before the next fetch: half the remaining
// credential lifetime (bounded to [1s, 30m]), or a short retry when there are
// no usable credentials. Callers hold loop.mu.
func (loop *hostedStorageLoop) refreshDelay() time.Duration {
	if !loop.credentialsOK {
		return 10 * time.Second
	}
	remaining := time.Until(loop.expiration)
	if remaining <= 0 {
		return 5 * time.Second
	}
	wait := remaining / 2
	if wait < time.Second {
		wait = time.Second
	}
	if wait > 30*time.Minute {
		wait = 30 * time.Minute
	}
	return wait
}

// apply installs the credentials: the first successful fetch constructs the
// S3 client and swaps it into the deferred store; later fetches rotate the
// credentials on that client, keeping in-flight operations on the generation
// they resolved.
func (loop *hostedStorageLoop) apply(credentials objectstore.STSCredentials) error {
	loop.mu.Lock()
	installed := loop.installed
	issued := loop.issuedConfig
	loop.mu.Unlock()
	if installed == nil {
		if issued == nil {
			return errors.New("hosted storage loop has no issued configuration to build a client from")
		}
		client, err := objectstore.OpenS3(objectstore.S3Config{
			Endpoint:        issued.Endpoint,
			Region:          issued.Region,
			Bucket:          issued.Bucket,
			AccessKeyID:     credentials.AccessKeyID,
			SecretAccessKey: credentials.SecretAccessKey,
			SessionToken:    credentials.SessionToken,
			Prefix:          issued.Prefix,
			StateDir:        loop.configuration.stateDir,
			ForcePathStyle:  issued.ForcePathStyle,
		})
		if err != nil {
			return err
		}
		loop.store.Swap(client)
		loop.mu.Lock()
		loop.installed = client
		loop.mu.Unlock()
		return nil
	}
	installed.SetCredentials(credentials.AccessKeyID, credentials.SecretAccessKey, credentials.SessionToken)
	return nil
}

// fetchAndApply performs one credential round trip and applies the result.
func (loop *hostedStorageLoop) fetchAndApply(ctx context.Context) {
	credentials, err := loop.fetch(ctx)
	if err != nil {
		switch {
		case errors.Is(err, ErrStorageQuotaExceeded):
			loop.mu.Lock()
			wasQuota := loop.lastQuota
			loop.lastQuota = true
			loop.credentialsOK = false
			loop.mu.Unlock()
			if !wasQuota {
				loop.logger.Warn("hosted storage quota exceeded; mirroring is suspended until space is freed or the plan is upgraded", "error", err)
			}
		case errors.Is(err, ErrStorageNotConfigured):
			loop.mu.Lock()
			loop.credentialsOK = false
			loop.mu.Unlock()
			loop.logger.Warn("control plane does not host checkpoint storage; this agent's object-store backend should be s3 or local", "error", err)
		default:
			loop.mu.Lock()
			usable := loop.credentialsOK && time.Until(loop.expiration) > 0
			loop.mu.Unlock()
			if usable {
				loop.logger.Warn("hosted storage refresh failed; keeping current credentials until they expire", "error", err)
			} else {
				loop.logger.Warn("hosted storage credentials are not available", "error", err)
			}
		}
		return
	}
	if err := loop.apply(credentials); err != nil {
		loop.logger.Error("hosted storage credentials could not be applied", "error", err)
		return
	}
	loop.mu.Lock()
	loop.expiration = credentials.Expiration
	loop.lastQuota = false
	loop.credentialsOK = true
	loop.mu.Unlock()
	loop.logger.Debug("hosted storage credentials applied", "expiration", credentials.Expiration.Format(time.RFC3339))
}

// fetch performs the HTTP round trip to the control plane's credential
// endpoint. Only the fetch touches the network; apply is local.
func (loop *hostedStorageLoop) fetch(ctx context.Context) (objectstore.STSCredentials, error) {
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/storage/credentials", strings.TrimRight(loop.configuration.controlURL, "/"), loop.configuration.organization)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return objectstore.STSCredentials{}, err
	}
	request.Header.Set("Authorization", "Bearer "+loop.configuration.apiKey)
	response, err := loop.client.Do(request)
	if err != nil {
		return objectstore.STSCredentials{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return objectstore.STSCredentials{}, err
	}
	switch {
	case response.StatusCode == http.StatusForbidden && strings.Contains(string(body), "STORAGE_QUOTA_EXCEEDED"):
		return objectstore.STSCredentials{}, ErrStorageQuotaExceeded
	case response.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(body), "STORAGE_NOT_CONFIGURED"):
		return objectstore.STSCredentials{}, ErrStorageNotConfigured
	case response.StatusCode == http.StatusUnauthorized:
		return objectstore.STSCredentials{}, fmt.Errorf("control plane rejected the machine API key (status %d)", response.StatusCode)
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return objectstore.STSCredentials{}, fmt.Errorf("control plane credential endpoint returned status %d", response.StatusCode)
	}
	var issued struct {
		Endpoint        string    `json:"endpoint"`
		Region          string    `json:"region"`
		Bucket          string    `json:"bucket"`
		Prefix          string    `json:"prefix"`
		AccessKeyID     string    `json:"access_key_id"`
		SecretAccessKey string    `json:"secret_access_key"`
		SessionToken    string    `json:"session_token"`
		Expiration      time.Time `json:"expiration"`
		ForcePathStyle  bool      `json:"force_path_style"`
	}
	if err := json.Unmarshal(body, &issued); err != nil {
		return objectstore.STSCredentials{}, fmt.Errorf("decode credential response: %w", err)
	}
	if issued.Endpoint == "" || issued.Region == "" || issued.Bucket == "" || issued.AccessKeyID == "" || issued.SecretAccessKey == "" || issued.SessionToken == "" {
		return objectstore.STSCredentials{}, errors.New("credential response is missing required fields")
	}
	if !issued.Expiration.After(time.Now().UTC()) {
		return objectstore.STSCredentials{}, errors.New("credential response is already expired")
	}
	loop.mu.Lock()
	loop.issuedConfig = &issuedStorageConfig{
		Endpoint:       issued.Endpoint,
		Region:         issued.Region,
		Bucket:         issued.Bucket,
		Prefix:         issued.Prefix,
		ForcePathStyle: issued.ForcePathStyle,
	}
	loop.mu.Unlock()
	return objectstore.STSCredentials{
		AccessKeyID:     issued.AccessKeyID,
		SecretAccessKey: issued.SecretAccessKey,
		SessionToken:    issued.SessionToken,
		Expiration:      issued.Expiration,
	}, nil
}
