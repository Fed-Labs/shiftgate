package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TraceParentHeader is the W3C Trace Context propagation header.
const TraceParentHeader = "traceparent"

// TraceContext is a parsed W3C Trace Context "traceparent" value: the trace an
// operation belongs to, the span performing it, and the sampling flag.
type TraceContext struct {
	TraceID string
	SpanID  string
	Sampled bool
}

// ParseTraceParent validates a traceparent header value. Anything malformed —
// wrong field count, non-hex characters, an all-zero id, the reserved version
// — is rejected rather than repaired, so a broken header starts a fresh trace
// instead of corrupting an existing one.
func ParseTraceParent(value string) (TraceContext, bool) {
	parts := strings.Split(value, "-")
	if len(parts) != 4 {
		return TraceContext{}, false
	}
	version, traceID, spanID, flags := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || !isHex(version) || strings.EqualFold(version, "ff") {
		return TraceContext{}, false
	}
	if len(traceID) != 32 || !isHex(traceID) || strings.Trim(traceID, "0") == "" {
		return TraceContext{}, false
	}
	if len(spanID) != 16 || !isHex(spanID) || strings.Trim(spanID, "0") == "" {
		return TraceContext{}, false
	}
	if len(flags) != 2 || !isHex(flags) {
		return TraceContext{}, false
	}
	sampled, err := strconv.ParseUint(flags, 16, 8)
	if err != nil {
		return TraceContext{}, false
	}
	return TraceContext{
		TraceID: strings.ToLower(traceID),
		SpanID:  strings.ToLower(spanID),
		Sampled: sampled&1 == 1,
	}, true
}

// Header renders the context as a traceparent header value.
func (t TraceContext) Header() string {
	flags := "00"
	if t.Sampled {
		flags = "01"
	}
	return "00-" + t.TraceID + "-" + t.SpanID + "-" + flags
}

// Child starts a new span id within the same trace.
func (t TraceContext) Child() TraceContext {
	return TraceContext{TraceID: t.TraceID, SpanID: randomHex(8), Sampled: t.Sampled}
}

// NewTraceContext mints a root trace.
func NewTraceContext() TraceContext {
	return TraceContext{TraceID: randomHex(16), SpanID: randomHex(8), Sampled: true}
}

func isHex(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}

func randomHex(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand does not fail on a healthy Linux system; if it somehow
		// does, a clock-derived value still keeps ids unique.
		return fmt.Sprintf("%0*x", size*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

type traceContextKey struct{}

// ContextWithTrace carries a trace context through a context chain so a whole
// migration — API request, orchestrator goroutine, peer calls — shares one
// trace.
func ContextWithTrace(ctx context.Context, trace TraceContext) context.Context {
	return context.WithValue(ctx, traceContextKey{}, trace)
}

// TraceFromContext returns the trace context carried by ctx, if any.
func TraceFromContext(ctx context.Context) (TraceContext, bool) {
	trace, ok := ctx.Value(traceContextKey{}).(TraceContext)
	return trace, ok
}

// TraceFromRequest returns the trace context of an incoming request: the one
// the caller propagated, or a new root trace when no valid header arrived.
func TraceFromRequest(request *http.Request) TraceContext {
	if trace, ok := ParseTraceParent(request.Header.Get(TraceParentHeader)); ok {
		return trace
	}
	return NewTraceContext()
}

// InjectTraceHeader propagates the trace context in a request's context to the
// outbound traceparent header. Requests without a trace in context are left
// untouched.
func InjectTraceHeader(request *http.Request) {
	trace, ok := TraceFromContext(request.Context())
	if !ok {
		return
	}
	request.Header.Set(TraceParentHeader, trace.Header())
}

// Span is one timed operation within a trace. Ids and attributes are strings
// by construction; no workload data is placed in them — SHIFT never traces
// secrets or computational contents.
type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	StartedAt    time.Time
	EndedAt      time.Time
	Attributes   map[string]string
}

// SpanSink receives finished spans.
type SpanSink interface {
	Emit(span Span)
}

// Tracer starts spans and emits finished ones to its sinks. A nil Tracer is
// valid and discards everything, so call sites never need nil checks.
type Tracer struct {
	sinks []SpanSink
}

// NewTracer builds a tracer that fans finished spans out to every sink. A nil
// logger skips the log sink.
func NewTracer(logger *slog.Logger, sinks ...SpanSink) *Tracer {
	if logger != nil {
		sinks = append(sinks, logSpanSink{logger: logger})
	}
	return &Tracer{sinks: sinks}
}

// Start opens a span under the trace in ctx (or a new root trace) and returns
// a context carrying the span so nested operations chain. Attributes must
// contain only operational facts — ids, stages, sizes — never payloads.
func (t *Tracer) Start(ctx context.Context, name string, attributes map[string]string) (context.Context, *SpanHandle) {
	if t == nil {
		return ctx, nil
	}
	trace, ok := TraceFromContext(ctx)
	if !ok {
		trace = NewTraceContext()
	}
	child := trace.Child()
	span := Span{
		TraceID:      child.TraceID,
		SpanID:       child.SpanID,
		ParentSpanID: trace.SpanID,
		Name:         name,
		StartedAt:    time.Now().UTC(),
		Attributes:   attributes,
	}
	return ContextWithTrace(ctx, child), &SpanHandle{tracer: t, span: span}
}

// SpanHandle is an in-flight span. A nil handle (from a nil Tracer) is safe to
// End.
type SpanHandle struct {
	tracer *Tracer
	span   Span
}

// End finishes the span and emits it to every sink. End on a nil handle or a
// double End is a no-op.
func (h *SpanHandle) End() {
	if h == nil || h.tracer == nil {
		return
	}
	if !h.span.EndedAt.IsZero() {
		return
	}
	h.span.EndedAt = time.Now().UTC()
	for _, sink := range h.tracer.sinks {
		sink.Emit(h.span)
	}
}

type logSpanSink struct {
	logger *slog.Logger
}

func (s logSpanSink) Emit(span Span) {
	attributes := make([]any, 0, 2*len(span.Attributes)+4)
	attributes = append(attributes, "trace_id", span.TraceID, "span_id", span.SpanID, "duration", span.EndedAt.Sub(span.StartedAt))
	for _, key := range sortedKeys(span.Attributes) {
		attributes = append(attributes, key, span.Attributes[key])
	}
	// Debug, not Info: every HTTP request already produces one structured log
	// line in the metrics middleware, and a chunk transfer is thousands of
	// requests. At info the span sink would duplicate each of them; a collector
	// receives every span regardless, and debug level turns the log sink on.
	s.logger.Debug("span "+span.Name, attributes...)
}

// TraceMiddleware gives every request a trace: the one the caller propagated,
// or a fresh root. The trace is echoed in the response so a client can
// correlate, carried in the request context, and (when a tracer is present)
// covered by a span. Metrics scrapes and health probes are exempt: they carry
// no caller trace, and minting one per scrape would bury real operations.
func TraceMiddleware(tracer *Tracer, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/metrics" || request.URL.Path == "/v1/health" {
			next.ServeHTTP(writer, request)
			return
		}
		trace := TraceFromRequest(request)
		request = request.WithContext(ContextWithTrace(request.Context(), trace))
		writer.Header().Set(TraceParentHeader, trace.Header())
		if tracer == nil {
			next.ServeHTTP(writer, request)
			return
		}
		_, span := tracer.Start(request.Context(), "http "+request.Method, map[string]string{
			"http.method": request.Method,
			"http.path":   request.URL.Path,
		})
		next.ServeHTTP(writer, request)
		span.End()
	})
}
