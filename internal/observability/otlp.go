package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// otlpSpan is one span in the OTLP JSON encoding: the subset of fields a
// collector needs. Resource attributes identify the emitting process so spans
// from the agent, the control plane, and the CLI land in one trace view.
type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              int             `json:"kind"`
	StartTimeUnixNano uint64          `json:"startTimeUnixNano"`
	EndTimeUnixNano   uint64          `json:"endTimeUnixNano"`
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
	Status            otlpStatus      `json:"status"`
}

type otlpAttribute struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue wraps a scalar in OTLP's tagged value shape.
type otlpAnyValue struct {
	StringValue string `json:"stringValue,omitempty"`
}

type otlpStatus struct {
	Code int `json:"code"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}
type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type otlpExportRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// OTLPExporter batches finished spans and ships them to an OTLP/HTTP JSON
// endpoint (an OpenTelemetry collector's /v1/traces) over plain net/http — a
// real exporter with no SDK dependency. Emit never blocks and never fails: a
// full buffer drops a span with a counter, because telemetry must not fail a
// migration. Ship failures retry once with backoff on the exporter's own
// goroutine and are then dropped with a log line.
type OTLPExporter struct {
	logger   *slog.Logger
	endpoint string
	resource otlpResource
	client   *http.Client
	interval time.Duration
	maxBatch int
	spans    chan otlpSpan

	exported atomic.Uint64
	dropped  atomic.Uint64
}

// NewOTLPExporter builds an exporter posting to endpoint, for example
// "https://collector:4318/v1/traces". serviceName and serviceVersion label
// the emitted resource. The logger reports dropped batches — the caller's
// own logger, so drops surface wherever its logs go.
func NewOTLPExporter(logger *slog.Logger, endpoint, serviceName, serviceVersion string) *OTLPExporter {
	if logger == nil {
		logger = slog.Default()
	}
	return &OTLPExporter{
		logger:   logger,
		endpoint: endpoint,
		resource: otlpResource{Attributes: []otlpAttribute{
			{Key: "service.name", Value: otlpAnyValue{StringValue: serviceName}},
			{Key: "service.version", Value: otlpAnyValue{StringValue: serviceVersion}},
		}},
		client:   &http.Client{Timeout: 10 * time.Second},
		interval: 5 * time.Second,
		maxBatch: 512,
		spans:    make(chan otlpSpan, 4096),
	}
}

// Emit buffers a finished span for the next export. It is non-blocking: when
// the buffer is full the span is dropped and counted rather than stalling the
// caller.
func (e *OTLPExporter) Emit(span Span) {
	otlp := otlpSpan{
		TraceID:           span.TraceID,
		SpanID:            span.SpanID,
		ParentSpanID:      span.ParentSpanID,
		Name:              span.Name,
		Kind:              1, // internal
		StartTimeUnixNano: uint64(span.StartedAt.UnixNano()),
		EndTimeUnixNano:   uint64(span.EndedAt.UnixNano()),
	}
	for key, value := range span.Attributes {
		otlp.Attributes = append(otlp.Attributes, otlpAttribute{Key: key, Value: otlpAnyValue{StringValue: value}})
	}
	select {
	case e.spans <- otlp:
	default:
		e.dropped.Add(1)
	}
}

// ExportedSpans reports how many spans the collector accepted.
func (e *OTLPExporter) ExportedSpans() uint64 { return e.exported.Load() }

// DroppedSpans reports spans lost to a full buffer or a failing collector.
func (e *OTLPExporter) DroppedSpans() uint64 { return e.dropped.Load() }

// Run batches buffered spans and ships them until ctx is done. Batches go out
// every interval, when maxBatch spans accumulate, and once more at shutdown.
func (e *OTLPExporter) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	batch := make([]otlpSpan, 0, e.maxBatch)
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case span := <-e.spans:
					batch = append(batch, span)
					if len(batch) >= e.maxBatch {
						e.ship(batch)
						batch = batch[:0]
					}
				default:
					e.ship(batch)
					return
				}
			}
		case span := <-e.spans:
			batch = append(batch, span)
			if len(batch) >= e.maxBatch {
				e.ship(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			e.ship(batch)
			batch = batch[:0]
		}
	}
}

// ship posts one batch, retrying once. Retries sleep on the exporter's own
// goroutine, never on a caller's.
func (e *OTLPExporter) ship(batch []otlpSpan) {
	if len(batch) == 0 {
		return
	}
	encoded, err := json.Marshal(otlpExportRequest{ResourceSpans: []otlpResourceSpans{{
		Resource:   e.resource,
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: "shift"}, Spans: batch}},
	}}})
	if err != nil {
		e.dropped.Add(uint64(len(batch)))
		e.logger.Error("dropping OTLP span batch: encoding failed",
			"endpoint", e.endpoint, "spans", len(batch), "error", err)
		return
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.endpoint, bytes.NewReader(encoded))
		if err != nil {
			lastErr = err
			continue
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := e.client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
		_ = response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			e.exported.Add(uint64(len(batch)))
			return
		}
		lastErr = fmt.Errorf("collector responded %s", response.Status)
	}
	e.dropped.Add(uint64(len(batch)))
	if lastErr != nil {
		e.logger.Error("dropping OTLP span batch after retry",
			"endpoint", e.endpoint, "spans", len(batch), "error", lastErr)
	}
}
