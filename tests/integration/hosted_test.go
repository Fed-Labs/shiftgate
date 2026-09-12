// Hosted checkpoint storage end to end: a real agent in control-plane
// object-store mode checkpointing a real workload with CRIU, its mirror
// landing in a real MinIO bucket through brokered credentials, and the
// control plane's own reconciliation counting the mirrored bytes.
//
// Gates (all required, each with its own skip reason):
//
//	SHIFT_TEST_E2E=1              root + CRIU (requireE2E)
//	SHIFT_TEST_HOSTED_CONTROL_URL control plane base URL
//	SHIFT_TEST_HOSTED_ORG_ID      organization id
//	SHIFT_TEST_HOSTED_API_KEY     machines-scope API key
//	SHIFT_TEST_HOSTED_S3_ENDPOINT MinIO endpoint (parent credentials for assertions)
//	SHIFT_TEST_HOSTED_S3_ACCESS_KEY
//	SHIFT_TEST_HOSTED_S3_SECRET_KEY
//	SHIFT_TEST_HOSTED_BUCKET
//
// Run alongside the other e2e suites (as root against the live stack):
//
//	SHIFT_TEST_E2E=1 go test ./tests/integration/ -run TestE2EHosted -v
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/agentclient"
	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/objectstore"
)

// hostedEnv is the live stack one hosted-storage run points at.
type hostedEnv struct {
	controlURL string
	orgID      string
	apiKey     string
	endpoint   string
	accessKey  string
	secretKey  string
	bucket     string
	region     string
}

// requireHosted skips unless a live control plane and MinIO are configured.
func requireHosted(t *testing.T) hostedEnv {
	t.Helper()
	requireE2E(t)
	env := hostedEnv{
		controlURL: os.Getenv("SHIFT_TEST_HOSTED_CONTROL_URL"),
		orgID:      os.Getenv("SHIFT_TEST_HOSTED_ORG_ID"),
		apiKey:     os.Getenv("SHIFT_TEST_HOSTED_API_KEY"),
		endpoint:   os.Getenv("SHIFT_TEST_HOSTED_S3_ENDPOINT"),
		accessKey:  os.Getenv("SHIFT_TEST_HOSTED_S3_ACCESS_KEY"),
		secretKey:  os.Getenv("SHIFT_TEST_HOSTED_S3_SECRET_KEY"),
		bucket:     os.Getenv("SHIFT_TEST_HOSTED_BUCKET"),
		region:     os.Getenv("SHIFT_TEST_HOSTED_S3_REGION"),
	}
	if env.region == "" {
		env.region = "us-east-1"
	}
	missing := []string{}
	if env.controlURL == "" {
		missing = append(missing, "SHIFT_TEST_HOSTED_CONTROL_URL")
	}
	if env.orgID == "" {
		missing = append(missing, "SHIFT_TEST_HOSTED_ORG_ID")
	}
	if env.apiKey == "" {
		missing = append(missing, "SHIFT_TEST_HOSTED_API_KEY")
	}
	if env.endpoint == "" || env.accessKey == "" || env.secretKey == "" || env.bucket == "" {
		missing = append(missing, "SHIFT_TEST_HOSTED_S3_ENDPOINT/ACCESS_KEY/SECRET_KEY/BUCKET")
	}
	if len(missing) > 0 {
		t.Skipf("set %s to run hosted-storage end-to-end tests", strings.Join(missing, ", "))
	}
	return env
}

// registerHostedMachine registers the agent's machine with the control plane
// so its heartbeats are accepted. The agent URL is the agent's own TLS peer
// listener, which the scheduler and migrations would dial.
func registerHostedMachine(t *testing.T, env hostedEnv, machineID, agentURL string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"machine_id": machineID,
		"name":       machineID,
		"agent_url":  agentURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		fmt.Sprintf("%s/v1/organizations/%s/machines", env.controlURL, env.orgID), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+env.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("register machine: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusConflict {
		t.Fatalf("register machine returned %d", response.StatusCode)
	}
}

// TestE2EHostedStorageCheckpoint: a workload checkpointed through an agent
// whose object store has no static configuration at all — the mirror rides
// credentials the control plane brokered from MinIO STS — and the mirrored
// ciphertext is verifiably present in the org's prefix, readable only through
// those credentials.
func TestE2EHostedStorageCheckpoint(t *testing.T) {
	env := requireHosted(t)
	machineID := fmt.Sprintf("e2e-hosted-%d", os.Getpid())
	stateDir := t.TempDir()
	peerPort, err := freePort()
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	agentProc := startAgentWith(t, "hosted-agent", stateDir, t.TempDir(), func(configuration *config.Agent) {
		configuration.RemoteListen = fmt.Sprintf("tcp://127.0.0.1:%d", peerPort)
		configuration.ObjectStore = objectstore.Config{
			Enabled:  true,
			Backend:  objectstore.ControlPlaneBackend,
			StateDir: filepath.Join(stateDir, "objectstore-state"),
		}
		configuration.ControlPlane = config.ControlReporter{
			URL:            env.controlURL,
			OrganizationID: env.orgID,
			MachineID:      machineID,
			AgentURL:       fmt.Sprintf("https://127.0.0.1:%d", peerPort),
			APIKey:         env.apiKey,
			Interval:       5 * time.Second,
		}
	})
	registerHostedMachine(t, env, machineID, fmt.Sprintf("https://127.0.0.1:%d", peerPort))

	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ni=0\nwhile true; do\n  i=$((i+1))\n  echo \"$i\" >> progress.log\n  sleep 1\ndone\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workload := createWorkload(t, agentProc.client, "hosted-counter", root, script)
	logPath := filepath.Join(root, "progress.log")
	waitUntil(t, 30*time.Second, "counter to make progress", func() (bool, string) {
		return fileLineCount(t, logPath) >= 2, fmt.Sprintf("lines=%d", fileLineCount(t, logPath))
	})

	// The hosted-storage loop fetches credentials at startup, but the workload
	// reaching running state does not prove the fetch completed; a checkpoint
	// attempted before it lands fails closed with "mirror failed" while still
	// persisting locally. Retry the mirror until it succeeds — that is the
	// honest observable — and count a create error only when nothing lands.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manifest, createErr := agentProc.client.CreateCheckpoint(ctx, agentclient.CheckpointCreateRequest{
		WorkloadID:   workload.Spec.ID,
		LeaveRunning: func(v bool) *bool { return &v }(true),
	})
	checkpointID := ""
	if createErr == nil {
		checkpointID = manifest.ID
	} else {
		checkpoints, listErr := agentProc.client.Checkpoints(ctx, workload.Spec.ID)
		if listErr != nil || len(checkpoints) == 0 {
			t.Fatalf("checkpoint create failed (%v) and no checkpoint was persisted (list: %v)", createErr, listErr)
		}
		checkpointID = checkpoints[len(checkpoints)-1].ID
	}
	var mirrored checkpoint.MirrorResult
	waitUntil(t, 2*time.Minute, "checkpoint to mirror through brokered credentials", func() (bool, string) {
		result, err := agentProc.client.MirrorCheckpoint(ctx, checkpointID)
		if err != nil {
			return false, err.Error()
		}
		mirrored = result
		return result.Objects > 0 && result.Bytes > 0, fmt.Sprintf("objects=%d bytes=%d", result.Objects, result.Bytes)
	})

	// The bucket, read with the parent credential the way the control plane's
	// reconciler reads it, must show the mirrored ciphertext under the
	// organization's prefix — metering is from the bucket, never agent claims.
	assertion, err := objectstore.OpenS3(objectstore.S3Config{
		Endpoint: env.endpoint, Region: env.region, Bucket: env.bucket,
		AccessKeyID: env.accessKey, SecretAccessKey: env.secretKey,
		Prefix: "org/" + env.orgID, StateDir: t.TempDir(), ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var listed int64
	var listedObjects int
	err = assertion.ListPrefix(ctx, "", func(info objectstore.ObjectInfo) error {
		listed += info.Size
		listedObjects++
		return nil
	})
	if err != nil {
		t.Fatalf("list the organization prefix: %v", err)
	}
	if listedObjects == 0 || listed <= 0 {
		t.Fatalf("organization prefix is empty after a mirrored checkpoint (objects=%d bytes=%d)", listedObjects, listed)
	}
	t.Logf("organization prefix holds %d objects, %d bytes (this checkpoint mirrored %d objects, %d bytes)",
		listedObjects, listed, mirrored.Objects, mirrored.Bytes)
}
