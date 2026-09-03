package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
)

func TestReporterSendsAuthorizedHeartbeat(t *testing.T) {
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/organizations/org-1/machines/machine-1/heartbeat" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if got := request.Header.Get("Authorization"); got != "Bearer shift_ak_secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		content, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(content, &body); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		if body["name"] != "Source" || body["status"] != "online" {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case requests <- request.Clone(request.Context()):
		default:
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	reporter := NewReporter(server.URL, "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Millisecond, time.Second, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	reporter.Start(ctx)
	defer reporter.Stop()
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat")
	}
}

func TestReporterObservesControlPlaneProtocolVersion(t *testing.T) {
	answered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set(model.ProtocolVersionHeader, strconv.Itoa(model.ProtocolVersion))
		writer.WriteHeader(http.StatusOK)
		select {
		case answered <- struct{}{}:
		default:
		}
	}))
	defer server.Close()

	reporter := NewReporter(server.URL, "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Millisecond, time.Second, slog.New(slog.DiscardHandler))
	if reporter.PeerProtocolVersion() != 0 {
		t.Fatal("no protocol requirement is known before the first heartbeat")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	reporter.Start(ctx)
	defer reporter.Stop()
	select {
	case <-answered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat")
	}
	deadline := time.After(time.Second)
	for reporter.PeerProtocolVersion() == 0 {
		select {
		case <-deadline:
			t.Fatal("the control plane's protocol version was never recorded")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if reporter.PeerProtocolVersion() != model.ProtocolVersion {
		t.Fatalf("expected protocol %d, got %d", model.ProtocolVersion, reporter.PeerProtocolVersion())
	}
}

func TestReporterIgnoresAnUnreadableProtocolHeader(t *testing.T) {
	reporter := NewReporter("https://control.example", "org-1", "machine-1", "Source", "", "shift_ak_secret", time.Minute, time.Second, slog.New(slog.DiscardHandler))
	for _, header := range []string{"", "   ", "one", "0", "-3"} {
		reporter.observeProtocol(header)
		if reporter.PeerProtocolVersion() != 0 {
			t.Fatalf("header %q must not be taken as a protocol requirement", header)
		}
	}
	reporter.observeProtocol(" 2 ")
	if reporter.PeerProtocolVersion() != 2 {
		t.Fatalf("expected protocol 2, got %d", reporter.PeerProtocolVersion())
	}
}
