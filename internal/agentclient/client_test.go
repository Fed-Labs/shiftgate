package agentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

func TestClientReturnsDecodedValues(t *testing.T) {
	client := testClient(func(request *http.Request) (*http.Response, error) {
		var value any
		switch request.URL.Path {
		case "/v1/health":
			value = map[string]any{"status": "ok", "machine_id": "machine-test"}
		case "/v1/workloads":
			value = []model.Workload{{Spec: model.WorkloadSpec{ID: "workload-test", Name: "test"}, Status: model.WorkloadRunning}}
		case "/v1/workloads/workload-test":
			value = model.Workload{Spec: model.WorkloadSpec{ID: "workload-test", Name: "test"}, Status: model.WorkloadRunning}
		case "/v1/checkpoints/checkpoint-test/mirror":
			if request.Method != http.MethodPost {
				return nil, fmt.Errorf("unexpected mirror method %s", request.Method)
			}
			value = checkpoint.MirrorResult{CheckpointID: "checkpoint-test", Objects: 3, Bytes: 4096}
		default:
			return nil, fmt.Errorf("unexpected request path %s", request.URL.Path)
		}
		return testResponse(http.StatusOK, value), nil
	})
	ctx := context.Background()
	health, err := client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health["machine_id"] != "machine-test" {
		t.Fatalf("health response was not returned: %#v", health)
	}
	workloads, err := client.Workloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(workloads) != 1 || workloads[0].Spec.ID != "workload-test" {
		t.Fatalf("workload list was not returned: %#v", workloads)
	}
	workload, err := client.Workload(ctx, "workload-test")
	if err != nil {
		t.Fatal(err)
	}
	if workload.Status != model.WorkloadRunning {
		t.Fatalf("workload response was not returned: %#v", workload)
	}
	mirrored, err := client.MirrorCheckpoint(ctx, "checkpoint-test")
	if err != nil {
		t.Fatal(err)
	}
	if mirrored.CheckpointID != "checkpoint-test" || mirrored.Objects != 3 || mirrored.Bytes != 4096 {
		t.Fatalf("mirror response was not returned: %#v", mirrored)
	}
}

func TestClientDecodesAPIErrors(t *testing.T) {
	client := testClient(func(_ *http.Request) (*http.Response, error) {
		return testResponse(http.StatusNotFound, model.ErrorResponse{Code: "WORKLOAD_NOT_FOUND", Message: "missing workload"}), nil
	})
	_, err := client.Workload(context.Background(), "missing")
	apiError, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected APIError, got %T: %v", err, err)
	}
	if apiError.Status != http.StatusNotFound || apiError.Code != "WORKLOAD_NOT_FOUND" {
		t.Fatalf("unexpected API error: %#v", apiError)
	}
}

func TestAgentHintNamesSocketFailures(t *testing.T) {
	dial := func(err error) error {
		return &url.Error{Op: "Get", URL: "http://shift-agent/v1/doctor", Err: &net.OpError{Op: "dial", Net: "unix", Err: &os.SyscallError{Syscall: "connect", Err: err}}}
	}
	if hint := agentHint(dial(syscall.ENOENT)); !strings.Contains(hint, "shift-agent running") {
		t.Fatalf("missing socket must hint at the service, got %q", hint)
	}
	if hint := agentHint(dial(syscall.ECONNREFUSED)); !strings.Contains(hint, "restart the agent") {
		t.Fatalf("refused socket must hint at a restart, got %q", hint)
	}
	if hint := agentHint(dial(syscall.EACCES)); !strings.Contains(hint, "shift' group") {
		t.Fatalf("denied socket must hint at the group, got %q", hint)
	}
	tcp := &url.Error{Op: "Get", URL: "http://shift-agent/v1/doctor", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}
	if hint := agentHint(tcp); hint != "" {
		t.Fatalf("non-unix dial must carry no hint, got %q", hint)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func testClient(roundTrip roundTripFunc) *Client {
	return &Client{baseURL: "http://shift-agent", http: &http.Client{Transport: roundTrip, Timeout: time.Second}}
}

func testResponse(status int, value any) *http.Response {
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d", status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
