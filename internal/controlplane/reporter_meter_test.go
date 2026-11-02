package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// relayReportServer records heartbeat bodies and answers with a fixed status.
type relayReportServer struct {
	mu       sync.Mutex
	bodies   []map[string]any
	status   int
	response string
}

func (fake *relayReportServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	content, _ := io.ReadAll(request.Body)
	var body map[string]any
	_ = json.Unmarshal(content, &body)
	fake.mu.Lock()
	fake.bodies = append(fake.bodies, body)
	status, response := fake.status, fake.response
	fake.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(response))
}

func (fake *relayReportServer) heartbeats() []map[string]any {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	out := make([]map[string]any, len(fake.bodies))
	copy(out, fake.bodies)
	return out
}

func (fake *relayReportServer) set(status int, response string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.status = status
	fake.response = response
}

func newReporterForTest(t *testing.T, handler http.Handler) *Reporter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	reporter := NewReporter(server.URL, "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Second, 200*time.Millisecond, slog.New(slog.DiscardHandler))
	return reporter
}

func TestReporterDropsRelayChannelBytes(t *testing.T) {
	fake := &relayReportServer{status: http.StatusOK, response: `{}`}
	reporter := newReporterForTest(t, fake)
	if err := reporter.RecordTransferBytes(context.Background(), "org-1", relayTransferChannel, 1000); err != nil {
		t.Fatal(err)
	}
	if err := reporter.RecordTransferBytes(context.Background(), "org-1", "peer", 64); err != nil {
		t.Fatal(err)
	}
	reporter.report(context.Background())
	heartbeats := fake.heartbeats()
	if len(heartbeats) != 1 {
		t.Fatalf("got %d heartbeats, want 1", len(heartbeats))
	}
	metered, ok := heartbeats[0]["transfer_bytes"].(map[string]any)
	if !ok {
		t.Fatalf("heartbeat carried no transfer_bytes: %v", heartbeats[0])
	}
	if _, relayed := metered[relayTransferChannel]; relayed {
		t.Fatalf("relayed bytes were metered through the heartbeat too: %v (the relay server is the single meter)", metered)
	}
	if metered["peer"] != float64(64) {
		t.Fatalf("peer bytes = %v, want 64", metered["peer"])
	}
}

func TestReporterKeepsMeteringAfterUnrelatedBadRequest(t *testing.T) {
	fake := &relayReportServer{status: http.StatusBadRequest, response: `{"error":{"code":"INVALID_JSON","message":"unknown field \"name\""}}`}
	reporter := newReporterForTest(t, fake)
	_ = reporter.RecordTransferBytes(context.Background(), "org-1", "peer", 100)
	reporter.report(context.Background())
	if reporter.transferDisabled.Load() {
		t.Fatal("an unrelated 400 latched transfer metering off")
	}
	fake.set(http.StatusOK, `{}`)
	_ = reporter.RecordTransferBytes(context.Background(), "org-1", "peer", 50)
	reporter.report(context.Background())
	heartbeats := fake.heartbeats()
	last := heartbeats[len(heartbeats)-1]
	metered, _ := last["transfer_bytes"].(map[string]any)
	if metered["peer"] != float64(150) {
		t.Fatalf("peer bytes = %v, want 150 (the earlier delta must survive an unrelated 400)", metered["peer"])
	}
}

func TestReporterPausesMeteringOnlyWhenTransferFieldIsRejected(t *testing.T) {
	fake := &relayReportServer{status: http.StatusBadRequest, response: `{"error":{"code":"INVALID_JSON","message":"json: unknown field \"transfer_bytes\""}}`}
	reporter := newReporterForTest(t, fake)
	_ = reporter.RecordTransferBytes(context.Background(), "org-1", "peer", 100)
	reporter.report(context.Background())
	if !reporter.transferDisabled.Load() {
		t.Fatal("a rejection naming transfer_bytes must pause metering")
	}
	fake.set(http.StatusOK, `{}`)
	reporter.report(context.Background())
	heartbeats := fake.heartbeats()
	last := heartbeats[len(heartbeats)-1]
	if _, sent := last["transfer_bytes"]; sent {
		t.Fatalf("paused metering still sent deltas: %v", last)
	}
}

func TestReporterDropsDeltasOnAmbiguousHeartbeatFailure(t *testing.T) {
	fake := &relayReportServer{status: http.StatusInternalServerError, response: `{"error":{"code":"MACHINE_HEARTBEAT_FAILED","error":"boom"}}`}
	reporter := newReporterForTest(t, fake)
	_ = reporter.RecordTransferBytes(context.Background(), "org-1", "peer", 100)
	reporter.report(context.Background())
	fake.set(http.StatusOK, `{}`)
	_ = reporter.RecordTransferBytes(context.Background(), "org-1", "peer", 50)
	reporter.report(context.Background())
	heartbeats := fake.heartbeats()
	last := heartbeats[len(heartbeats)-1]
	metered, _ := last["transfer_bytes"].(map[string]any)
	if metered["peer"] != float64(50) {
		t.Fatalf("peer bytes = %v, want 50: deltas from a rejected heartbeat must not be re-sent (double metering)", metered["peer"])
	}
}
