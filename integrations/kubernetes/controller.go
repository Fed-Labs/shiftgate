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

type Config struct {
	KubernetesURL string
	Token         string
	Namespace     string
	ControlURL    string
	ControlToken  string
	ControlCAFile string
	Organization  string
	CAFile        string
	PollInterval  time.Duration
}

type KubeClient struct {
	baseURL string
	token   string
	http    *http.Client
	ns      string
}

type ShiftMigration struct {
	Metadata kubeMetadata   `json:"metadata"`
	Spec     migrationSpec  `json:"spec"`
	Status   map[string]any `json:"status,omitempty"`
}

type kubeMetadata struct {
	Name              string     `json:"name"`
	Namespace         string     `json:"namespace"`
	UID               string     `json:"uid"`
	Generation        int64      `json:"generation"`
	ResourceVersion   string     `json:"resourceVersion"`
	Finalizers        []string   `json:"finalizers,omitempty"`
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty"`
}

type migrationSpec struct {
	WorkloadID           string `json:"workloadId"`
	SourceMachineID      string `json:"sourceMachineId"`
	DestinationMachineID string `json:"destinationMachineId"`
	Mode                 string `json:"mode,omitempty"`
}

type kubeList struct {
	Items []ShiftMigration `json:"items"`
}

type shiftMigration struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error_message,omitempty"`
}

func NewConfigFromEnv() (Config, error) {
	get := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	config := Config{KubernetesURL: strings.TrimRight(get("KUBERNETES_HOST"), "/"), Token: get("KUBERNETES_TOKEN"), Namespace: get("POD_NAMESPACE"), ControlURL: strings.TrimRight(get("SHIFT_CONTROL_URL"), "/"), ControlToken: get("SHIFT_CONTROL_TOKEN"), ControlCAFile: get("SHIFT_CA_FILE"), Organization: get("SHIFT_ORGANIZATION_ID"), CAFile: get("KUBERNETES_CA_FILE"), PollInterval: 5 * time.Second}
	if config.Namespace == "" {
		config.Namespace = "default"
	}
	if config.KubernetesURL == "" || config.Token == "" || config.ControlURL == "" || config.ControlToken == "" || config.Organization == "" {
		return Config{}, errors.New("KUBERNETES_HOST, KUBERNETES_TOKEN, SHIFT_CONTROL_URL, SHIFT_CONTROL_TOKEN, and SHIFT_ORGANIZATION_ID are required")
	}
	// Both configured URLs carry bearer tokens (kube and org-scoped control
	// plane). Same floor as the Slurm client: https only, so a misconfigured
	// URL cannot leak either token in cleartext.
	if !strings.HasPrefix(config.KubernetesURL, "https://") {
		return Config{}, errors.New("KUBERNETES_HOST must use https://")
	}
	if !strings.HasPrefix(config.ControlURL, "https://") {
		return Config{}, errors.New("SHIFT_CONTROL_URL must use https://")
	}
	return config, nil
}

func NewKubeClient(config Config) (*KubeClient, error) {
	if !strings.HasPrefix(config.KubernetesURL, "https://") {
		return nil, errors.New("KUBERNETES_HOST must use https://")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// TLS 1.2 floor: some frontends terminate TLS 1.2 only, and 1.2 is the
	// compatibility baseline. Nothing here requires 1.3 specifically.
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if config.CAFile != "" {
		pem, err := os.ReadFile(config.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("KUBERNETES_CA_FILE contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return &KubeClient{baseURL: config.KubernetesURL, token: config.Token, http: &http.Client{Transport: transport, Timeout: 30 * time.Second}, ns: config.Namespace}, nil
}

func (c *KubeClient) list(ctx context.Context) ([]ShiftMigration, error) {
	var result kubeList
	err := c.do(ctx, http.MethodGet, path.Join("/apis/shiftgate.dev/v1alpha1/namespaces", c.ns, "shiftmigrations"), nil, &result)
	return result.Items, err
}

func (c *KubeClient) update(ctx context.Context, migration ShiftMigration) error {
	endpoint := path.Join("/apis/shiftgate.dev/v1alpha1/namespaces", c.ns, "shiftmigrations", migration.Metadata.Name, "status")
	return c.do(ctx, http.MethodPut, endpoint, map[string]any{"apiVersion": "shiftgate.dev/v1alpha1", "kind": "ShiftMigration", "metadata": migration.Metadata, "status": migration.Status}, nil)
}

func (c *KubeClient) addFinalizer(ctx context.Context, migration ShiftMigration, finalizer string) error {
	seen := false
	for _, value := range migration.Metadata.Finalizers {
		if value == finalizer {
			seen = true
		}
	}
	if seen {
		return nil
	}
	migration.Metadata.Finalizers = append(migration.Metadata.Finalizers, finalizer)
	return c.do(ctx, http.MethodPut, path.Join("/apis/shiftgate.dev/v1alpha1/namespaces", c.ns, "shiftmigrations", migration.Metadata.Name), migration, nil)
}

func (c *KubeClient) removeFinalizer(ctx context.Context, migration ShiftMigration, finalizer string) error {
	filtered := migration.Metadata.Finalizers[:0]
	for _, value := range migration.Metadata.Finalizers {
		if value != finalizer {
			filtered = append(filtered, value)
		}
	}
	migration.Metadata.Finalizers = filtered
	return c.do(ctx, http.MethodPut, path.Join("/apis/shiftgate.dev/v1alpha1/namespaces", c.ns, "shiftmigrations", migration.Metadata.Name), migration, nil)
}

func (c *KubeClient) do(ctx context.Context, method, endpoint string, body, result any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("kubernetes API returned %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if result != nil && len(data) != 0 {
		return json.Unmarshal(data, result)
	}
	return nil
}

type ShiftClient struct {
	baseURL, token, organization string
	http                         *http.Client
}

const migrationsPageSize = 500

func NewShiftClient(config Config) (*ShiftClient, error) {
	if !strings.HasPrefix(config.ControlURL, "https://") {
		return nil, errors.New("SHIFT_CONTROL_URL must use https://")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if config.ControlCAFile != "" {
		pem, err := os.ReadFile(config.ControlCAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("SHIFT_CA_FILE contains no certificates")
		}
		transport.TLSClientConfig.RootCAs = pool
	}
	return &ShiftClient{baseURL: config.ControlURL, token: config.ControlToken, organization: config.Organization, http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}, nil
}

func (c *ShiftClient) createMigration(ctx context.Context, spec migrationSpec, key string) (shiftMigration, error) {
	var result shiftMigration
	err := c.do(ctx, http.MethodPost, path.Join("/v1/organizations", c.organization, "migrations"), map[string]string{"workload_id": spec.WorkloadID, "source_machine_id": spec.SourceMachineID, "destination_machine_id": spec.DestinationMachineID, "mode": normalizeMode(spec.Mode)}, &result, key)
	return result, err
}
func (c *ShiftClient) getMigration(ctx context.Context, id string) (shiftMigration, error) {
	// The migrations list is paginated (newest first, bare array, no
	// truncation signal), so scanning a single page reports "not found" for
	// any in-flight job once ~100 newer records exist in the organization.
	// Page through with an explicit limit/offset continuation until the id
	// is found or the list is exhausted.
	for offset := 0; ; offset += migrationsPageSize {
		var jobs []shiftMigration
		endpoint := fmt.Sprintf("%s?limit=%d&offset=%d", path.Join("/v1/organizations", c.organization, "migrations"), migrationsPageSize, offset)
		if err := c.do(ctx, http.MethodGet, endpoint, nil, &jobs, ""); err != nil {
			return shiftMigration{}, err
		}
		for _, job := range jobs {
			if job.ID == id {
				return job, nil
			}
		}
		if len(jobs) < migrationsPageSize {
			return shiftMigration{}, fmt.Errorf("migration %s not found", id)
		}
	}
}
func (c *ShiftClient) cancelMigration(ctx context.Context, id, key string) error {
	return c.do(ctx, http.MethodPost, path.Join("/v1/organizations", c.organization, "migrations", id, "cancel"), map[string]any{}, nil, key)
}
func (c *ShiftClient) do(ctx context.Context, method, endpoint string, body, result any, key string) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("SHIFT API returned %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	if result != nil {
		return json.Unmarshal(data, result)
	}
	return nil
}

func normalizeMode(value string) string {
	if value == "live" {
		return "live"
	}
	return "cold"
}
func operationKey(uid string, generation int64) string {
	return fmt.Sprintf("kubernetes-%s-%d", uid, generation)
}
func setStatus(migration *ShiftMigration, phase, reason string, generation int64, operationID string) {
	migration.Status = map[string]any{"phase": phase, "reason": reason, "observedGeneration": generation, "operationID": operationID, "lastObservedAt": time.Now().UTC().Format(time.RFC3339Nano)}
}

// observedGeneration reads the generation a status block was written for.
// JSON decoding lands numbers as float64 (and hand-written status may use
// int64), so normalize before comparing against metadata.generation.
func observedGeneration(status map[string]any) int64 {
	switch value := status["observedGeneration"].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return -1
	}
}

// migrationTerminal reports whether the control plane considers the
// migration settled: no capacity is held and the workload accepts new
// migrations again.
func migrationTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}
