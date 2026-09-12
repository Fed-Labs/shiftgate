package transfer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

type capturedRequest struct {
	body []byte
}

// TestDoJSONRetryResendsTheFullBody pins the retry contract: a retried peer
// call must carry the same body the first attempt carried. Reusing one reader
// across attempts would hand net/http a drained buffer — the request would
// announce Content-Length 0 and the peer would answer INVALID_JSON for a body
// that was never empty, masking whatever transient failure caused the retry.
func TestDoJSONRetryResendsTheFullBody(t *testing.T) {
	input := ReserveRequest{
		ID: "retry-body", SourceMachineID: "machine", WorkloadID: "workload",
		EstimatedBytes: 1 << 20, ExpiresAt: time.Now().Add(time.Minute),
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []capturedRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		requests = append(requests, capturedRequest{body: body})
		count := len(requests)
		mu.Unlock()
		if count == 1 {
			// A transient failure the client is expected to retry.
			writeJSON(writer, http.StatusServiceUnavailable, model.ErrorResponse{Code: "UNAVAILABLE", Message: "try again"})
			return
		}
		writeJSON(writer, http.StatusOK, Session{})
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	if _, err := client.Reserve(context.Background(), input); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("expected one retry, got %d requests", len(requests))
	}
	for i, request := range requests {
		if string(request.body) != string(encoded) {
			t.Fatalf("attempt %d body = %q, want the full request %q", i+1, request.body, encoded)
		}
	}
}

// TestDoJSONSurfacesDefinitiveRefusalImmediately pins the other half of the
// retry contract: a capacity refusal such as 507 STORAGE_INSUFFICIENT is not
// transient — retrying it cannot change the answer, and every extra attempt
// only delays the real reason from reaching the migration record. The call
// must return after one request, carrying the peer's own error code.
func TestDoJSONSurfacesDefinitiveRefusalImmediately(t *testing.T) {
	var attempts int
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts++
		writeJSON(writer, http.StatusInsufficientStorage, model.ErrorResponse{
			Code:    "STORAGE_INSUFFICIENT",
			Message: "destination has insufficient free storage for the reservation",
		})
	}))
	defer server.Close()

	client := &Client{baseURL: server.URL, http: server.Client()}
	_, err := client.Reserve(context.Background(), ReserveRequest{ID: "refusal", SourceMachineID: "machine"})
	if err == nil {
		t.Fatal("reserve against a refusing peer must fail")
	}
	if attempts != 1 {
		t.Fatalf("a definitive refusal must not be retried, saw %d attempts", attempts)
	}
	if !strings.Contains(err.Error(), "STORAGE_INSUFFICIENT") {
		t.Fatalf("error must carry the peer's refusal code, got %q", err)
	}
	if strings.Contains(err.Error(), "INVALID_JSON") {
		t.Fatalf("the peer's real refusal must not be masked as a decode error, got %q", err)
	}
}
