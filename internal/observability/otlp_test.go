package observability

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// collectorServer is a stand-in OTLP/HTTP JSON endpoint: it records the
// request bodies and can be told to reject them.
type collectorServer struct {
	mu     sync.Mutex
	bodies []map[string]any
	reject bool
}

func (c *collectorServer) handler(writer http.ResponseWriter, request *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if request.Method != http.MethodPost || request.URL.Path != "/v1/traces" {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	if c.reject {
		writer.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	c.bodies = append(c.bodies, decoded)
	writer.WriteHeader(http.StatusOK)
}

func (c *collectorServer) received() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.bodies...)
}

func sampleSpan() Span {
	return Span{
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:     "00f067aa0ba902b7",
		Name:       "migration",
		StartedAt:  time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		EndedAt:    time.Date(2026, 8, 28, 12, 0, 3, 0, time.UTC),
		Attributes: map[string]string{"migration_id": "mig_1"},
	}
}

func TestOTLPExporterShipsCollectorShape(t *testing.T) {
	collector := &collectorServer{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()
	exporter := NewOTLPExporter(nil, server.URL+"/v1/traces", "shift-agent-test", "1.2.3")
	exporter.Emit(sampleSpan())
	span, ok := <-exporter.spans
	if !ok {
		t.Fatal("Emit did not buffer a span")
	}
	exporter.ship([]otlpSpan{span})
	if exporter.ExportedSpans() != 1 {
		t.Fatalf("exported = %d, want 1", exporter.ExportedSpans())
	}
	if exporter.DroppedSpans() != 0 {
		t.Fatalf("dropped = %d, want 0", exporter.DroppedSpans())
	}
	bodies := collector.received()
	if len(bodies) != 1 {
		t.Fatalf("collector received %d exports, want 1", len(bodies))
	}
	exported := bodies[0]
	resourceSpans, ok := exported["resourceSpans"].([]any)
	if !ok || len(resourceSpans) != 1 {
		t.Fatalf("resourceSpans missing: %v", exported)
	}
	first := resourceSpans[0].(map[string]any)
	resource := first["resource"].(map[string]any)
	attributes := resource["attributes"].([]any)
	if len(attributes) != 2 {
		t.Fatalf("resource attributes = %v", attributes)
	}
	scopeSpans := first["scopeSpans"].([]any)[0].(map[string]any)
	spans := scopeSpans["spans"].([]any)
	if len(spans) != 1 {
		t.Fatalf("spans = %v", spans)
	}
	spanJSON := spans[0].(map[string]any)
	if spanJSON["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("traceId = %v", spanJSON["traceId"])
	}
	if spanJSON["name"] != "migration" {
		t.Fatalf("name = %v", spanJSON["name"])
	}
	if spanJSON["startTimeUnixNano"] != float64(time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC).UnixNano()) {
		t.Fatalf("startTimeUnixNano = %v", spanJSON["startTimeUnixNano"])
	}
	spanAttributes := spanJSON["attributes"].([]any)
	if len(spanAttributes) != 1 || spanAttributes[0].(map[string]any)["key"] != "migration_id" {
		t.Fatalf("span attributes = %v", spanAttributes)
	}
}

func TestOTLPExporterDropsWhenCollectorFails(t *testing.T) {
	collector := &collectorServer{reject: true}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()
	exporter := NewOTLPExporter(nil, server.URL+"/v1/traces", "shift-agent-test", "1.2.3")
	exporter.Emit(sampleSpan())
	span, ok := <-exporter.spans
	if !ok {
		t.Fatal("Emit did not buffer a span")
	}
	// ship retries once with backoff on its own goroutine, so a rejected batch
	// takes under a second to be counted as dropped.
	before := time.Now()
	exporter.ship([]otlpSpan{span})
	if elapsed := time.Since(before); elapsed > 5*time.Second {
		t.Fatalf("ship retried for %s", elapsed)
	}
	if exporter.ExportedSpans() != 0 {
		t.Fatalf("exported = %d, want 0", exporter.ExportedSpans())
	}
	if exporter.DroppedSpans() != 1 {
		t.Fatalf("dropped = %d, want 1", exporter.DroppedSpans())
	}
}

func TestOTLPExporterRunFlushesUntilContextDone(t *testing.T) {
	collector := &collectorServer{}
	server := httptest.NewServer(http.HandlerFunc(collector.handler))
	defer server.Close()
	exporter := NewOTLPExporter(nil, server.URL+"/v1/traces", "shift-agent-test", "1.2.3")
	exporter.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		exporter.Run(ctx)
		close(done)
	}()
	exporter.Emit(sampleSpan())
	exporter.Emit(sampleSpan())
	deadline := time.After(5 * time.Second)
	for exporter.ExportedSpans() < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("exporter did not ship spans within the deadline")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
