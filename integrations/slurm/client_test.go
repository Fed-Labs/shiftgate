package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	caPath := t.TempDir() + "/ca.pem"
	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, certBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, "token", caPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client, caPath
}

func TestClientRetriesAndPreservesIdempotencyKey(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Idempotency-Key"); got != "stable-key" {
			t.Fatalf("idempotency key = %q", got)
		}
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": "m1", "status": "queued", "progress": map[string]any{}})
	}))
	defer server.Close()
	caPath := t.TempDir() + "/ca.pem"
	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caPath, certBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(server.URL, "token", caPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.CreateMigration(context.Background(), "org", map[string]string{"workload_id": "w"}, "stable-key", false)
	if err != nil || result.ID != "m1" || requests.Load() != 2 {
		t.Fatalf("result=%#v err=%v requests=%d", result, err, requests.Load())
	}
}

func TestMigrationPaginatesPastFirstPage(t *testing.T) {
	var requests atomic.Int32
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/organizations/org/migrations" {
			http.NotFound(writer, request)
			return
		}
		offset, err := strconv.Atoi(request.URL.Query().Get("offset"))
		if err != nil {
			t.Fatalf("offset: %v", err)
		}
		if limit := request.URL.Query().Get("limit"); limit != strconv.Itoa(migrationsPageSize) {
			t.Fatalf("limit = %q, want %d", limit, migrationsPageSize)
		}
		all := make([]Migration, 0, migrationsPageSize+1)
		for i := 0; i < migrationsPageSize; i++ {
			all = append(all, Migration{ID: fmt.Sprintf("m-old-%d", i), Status: "completed"})
		}
		all = append(all, Migration{ID: "m-target", Status: "running"})
		start := offset
		if start > len(all) {
			start = len(all)
		}
		end := start + migrationsPageSize
		if end > len(all) {
			end = len(all)
		}
		_ = json.NewEncoder(writer).Encode(all[start:end])
	}))
	result, err := client.Migration(context.Background(), "org", "m-target")
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "m-target" || result.Status != "running" {
		t.Fatalf("result = %#v", result)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (page 1 + continuation page)", requests.Load())
	}
}

func TestMigrationNotFoundAfterFullScan(t *testing.T) {
	var requests atomic.Int32
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(writer).Encode([]Migration{{ID: "m1", Status: "completed"}})
	}))
	if _, err := client.Migration(context.Background(), "org", "m-missing"); err == nil {
		t.Fatal("expected not-found error after exhausting pages")
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (short page ends the scan)", requests.Load())
	}
}

func TestCreateCheckpointSendsRealTimeoutAndNeverRetries(t *testing.T) {
	var requests atomic.Int32
	var body map[string]any
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/organizations/org/machines/m1/commands" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		if request.Header.Get("Idempotency-Key") == "" {
			t.Fatal("checkpoint command must carry an idempotency key")
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		writer.WriteHeader(http.StatusBadGateway)
	}))
	_, err := client.CreateCheckpoint(context.Background(), "org", "m1", "w1", 600, "key")
	if err == nil {
		t.Fatal("expected failure to surface, not be swallowed")
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1: checkpoint commands must not be retried (no durable replay)", requests.Load())
	}
	if body["action"] != "checkpoint" || body["workload_id"] != "w1" {
		t.Fatalf("body = %#v", body)
	}
	if got, ok := body["timeout_seconds"].(float64); !ok || int(got) != 600 {
		t.Fatalf("timeout_seconds = %#v, want 600 (the real dump budget, not a 30s ceiling)", body["timeout_seconds"])
	}
}

func TestCreateCheckpointReturnsRegisteredCheckpoint(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"checkpoint": map[string]any{"id": "cp1"}})
	}))
	result, err := client.CreateCheckpoint(context.Background(), "org", "m1", "w1", 600, "key")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(result, &payload); err != nil {
		t.Fatal(err)
	}
	checkpoint, ok := payload["checkpoint"].(map[string]any)
	if !ok || checkpoint["id"] != "cp1" {
		t.Fatalf("payload = %#v", payload)
	}
}
