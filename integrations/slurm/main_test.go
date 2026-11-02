package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func runArgs(serverURL, caPath string, args ...string) []string {
	base := []string{"-control-url", serverURL, "-token", "token", "-organization", "org", "-job-id", "7", "-ca-file", caPath, "-timeout", "5s"}
	return append(base, args...)
}

func runServer(t *testing.T, handler http.Handler) (string, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	caPath := t.TempDir() + "/ca.pem"
	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, certBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return server.URL, caPath
}

func TestRunCheckpointTriggersRealCheckpoint(t *testing.T) {
	var body map[string]any
	serverURL, caPath := runServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/organizations/org/machines/m1/commands" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"checkpoint": map[string]any{"id": "cp1"}})
	}))
	var out, errOut bytes.Buffer
	err := run(runArgs(serverURL, caPath, "checkpoint", "m1", "w1"), &out, &errOut)
	if err != nil {
		t.Fatalf("checkpoint must be supported, got error: %v", err)
	}
	if body["action"] != "checkpoint" || body["workload_id"] != "w1" {
		t.Fatalf("body = %#v", body)
	}
	if got := body["timeout_seconds"].(float64); int(got) != 600 {
		t.Fatalf("timeout_seconds = %v, want 600", got)
	}
	if !strings.Contains(out.String(), "cp1") {
		t.Fatalf("stdout = %q, want registered checkpoint id", out.String())
	}
}

func TestRunCheckpointFailureIsLoud(t *testing.T) {
	serverURL, caPath := runServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte(`{"code":"AGENT_UNAVAILABLE","message":"agent down"}`))
	}))
	var out, errOut bytes.Buffer
	err := run(runArgs(serverURL, caPath, "checkpoint", "m1", "w1"), &out, &errOut)
	if err == nil {
		t.Fatal("a failed checkpoint must surface as an error, not be swallowed")
	}
	if !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("error = %v, want it to name the failed checkpoint", err)
	}
}

func TestRunPollDegradesWhenEventsUnavailable(t *testing.T) {
	serverURL, caPath := runServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/events"):
			writer.WriteHeader(http.StatusNotFound)
		case request.URL.Path == "/v1/organizations/org/migrations":
			_ = json.NewEncoder(writer).Encode([]map[string]any{{"id": "m1", "status": "failed", "error_message": "boom"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	var out, errOut bytes.Buffer
	if err := run(runArgs(serverURL, caPath, "poll", "m1"), &out, &errOut); err != nil {
		t.Fatalf("poll must degrade gracefully when events fail: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["events_error"] == nil {
		t.Fatalf("payload = %#v, want events_error recorded", payload)
	}
	migration, ok := payload["migration"].(map[string]any)
	if !ok || migration["status"] != "failed" {
		t.Fatalf("payload = %#v, want migration record preserved", payload)
	}
}

func TestRunRejectsMissingCheckpointArguments(t *testing.T) {
	serverURL, caPath := runServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	var out, errOut bytes.Buffer
	err := run(runArgs(serverURL, caPath, "checkpoint", "m1"), &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "usage: checkpoint") {
		t.Fatalf("err = %v, want usage error", err)
	}
}

func TestRunDryRunCheckpointPrintsPlan(t *testing.T) {
	serverURL, caPath := runServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("dry-run must not call the control plane")
	}))
	var out, errOut bytes.Buffer
	if err := run(runArgs(serverURL, caPath, "-dry-run", "checkpoint", "m1", "w1"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "checkpoint") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestCheckpointBudgetCoversRealWorkloads(t *testing.T) {
	if checkpointTimeoutSeconds < 600 {
		t.Fatalf("checkpointTimeoutSeconds = %d, want >= 600 (minutes for a real CRIU dump)", checkpointTimeoutSeconds)
	}
}
