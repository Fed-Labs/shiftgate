package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseTraceParentAcceptsValidHeader(t *testing.T) {
	trace, ok := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok {
		t.Fatal("valid traceparent rejected")
	}
	if trace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("trace id = %q", trace.TraceID)
	}
	if trace.SpanID != "00f067aa0ba902b7" {
		t.Fatalf("span id = %q", trace.SpanID)
	}
	if !trace.Sampled {
		t.Fatal("flags 01 means sampled")
	}
}

func TestParseTraceParentNormalizesUppercase(t *testing.T) {
	trace, ok := ParseTraceParent("00-4BF92F3577B34DA6A3CE929D0E0E4736-00F067AA0BA902B7-00")
	if !ok || trace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("uppercase header not normalized: %+v ok=%v", trace, ok)
	}
	if trace.Sampled {
		t.Fatal("flags 00 means not sampled")
	}
}

func TestParseTraceParentRejectsMalformed(t *testing.T) {
	invalid := []string{
		"",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-ff",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"zz-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4bf92f3577b34da6a3ce929d0e0e47-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0g",
	}
	for _, value := range invalid {
		if _, ok := ParseTraceParent(value); ok {
			t.Fatalf("malformed traceparent accepted: %q", value)
		}
	}
}

func TestTraceContextHeaderRoundTrip(t *testing.T) {
	trace, ok := ParseTraceParent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok {
		t.Fatal("seed header rejected")
	}
	child := trace.Child()
	parsed, ok := ParseTraceParent(child.Header())
	if !ok {
		t.Fatal("child header rejected")
	}
	if parsed.TraceID != trace.TraceID {
		t.Fatalf("child left the trace: %q != %q", parsed.TraceID, trace.TraceID)
	}
	if parsed.SpanID == trace.SpanID {
		t.Fatal("child reused the parent span id")
	}
	if parsed.Header() != "00-"+parsed.TraceID+"-"+parsed.SpanID+"-01" {
		t.Fatalf("header = %q", parsed.Header())
	}
}

func TestTraceContextCarryThroughContext(t *testing.T) {
	trace := NewTraceContext()
	ctx := ContextWithTrace(context.Background(), trace)
	carried, ok := TraceFromContext(ctx)
	if !ok || carried.TraceID != trace.TraceID {
		t.Fatalf("trace did not survive the context: %+v ok=%v", carried, ok)
	}
	if _, ok := TraceFromContext(context.Background()); ok {
		t.Fatal("empty context claimed to carry a trace")
	}
}

func TestInjectTraceHeader(t *testing.T) {
	trace := NewTraceContext()
	traced, err := http.NewRequestWithContext(
		ContextWithTrace(context.Background(), trace),
		http.MethodGet, "http://agent/v1/migrations", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	InjectTraceHeader(traced)
	parsed, ok := ParseTraceParent(traced.Header.Get(TraceParentHeader))
	if !ok || parsed.TraceID != trace.TraceID {
		t.Fatalf("injected header = %q", traced.Header.Get(TraceParentHeader))
	}
	plain, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://agent/v1/migrations", nil)
	if err != nil {
		t.Fatal(err)
	}
	InjectTraceHeader(plain)
	if plain.Header.Get(TraceParentHeader) != "" {
		t.Fatal("request without a trace in context gained a traceparent header")
	}
}

func TestTraceMiddlewarePropagatesAndEchoes(t *testing.T) {
	incoming := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	var served TraceContext
	handler := TraceMiddleware(nil, http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		served, _ = TraceFromContext(request.Context())
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/migrations", nil)
	request.Header.Set(TraceParentHeader, incoming)
	handler.ServeHTTP(recorder, request)
	if served.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("handler saw trace %q", served.TraceID)
	}
	echoed := recorder.Header().Get(TraceParentHeader)
	if !strings.HasPrefix(echoed, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("response echoed %q", echoed)
	}
}

func TestTraceMiddlewareMintsRootForUntracedRequests(t *testing.T) {
	handler := TraceMiddleware(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/workloads", nil))
	if recorder.Header().Get(TraceParentHeader) == "" {
		t.Fatal("untraced request got no root trace")
	}
}

func TestTraceMiddlewareSkipsProbes(t *testing.T) {
	handler := TraceMiddleware(nil, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if recorder.Header().Get(TraceParentHeader) != "" {
		t.Fatal("metrics scrape was traced")
	}
}

func TestTracerSpansChainWithinOneTrace(t *testing.T) {
	sink := &recordSink{}
	tracer := NewTracer(nil, sink)
	ctx, parent := tracer.Start(context.Background(), "migration", nil)
	_, child := tracer.Start(ctx, "transfer", map[string]string{"stage": "upload"})
	child.End()
	parent.End()
	if len(sink.spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(sink.spans))
	}
	finishedChild, finishedParent := sink.spans[0], sink.spans[1]
	if finishedChild.TraceID != finishedParent.TraceID {
		t.Fatal("child span left the trace")
	}
	if finishedChild.ParentSpanID != finishedParent.SpanID {
		t.Fatalf("child parent = %q, want %q", finishedChild.ParentSpanID, finishedParent.SpanID)
	}
	if finishedChild.Attributes["stage"] != "upload" {
		t.Fatalf("attributes lost: %+v", finishedChild.Attributes)
	}
	if finishedChild.EndedAt.Before(finishedChild.StartedAt) {
		t.Fatal("span ended before it started")
	}
}

func TestSpanEndIsIdempotent(t *testing.T) {
	sink := &recordSink{}
	tracer := NewTracer(nil, sink)
	_, span := tracer.Start(context.Background(), "once", nil)
	span.End()
	span.End()
	if len(sink.spans) != 1 {
		t.Fatalf("double End emitted %d spans", len(sink.spans))
	}
}

func TestNilTracerIsSafe(t *testing.T) {
	var tracer *Tracer
	ctx, span := tracer.Start(context.Background(), "migration", nil)
	span.End()
	if _, ok := TraceFromContext(ctx); ok {
		t.Fatal("nil tracer injected a trace")
	}
}

type recordSink struct {
	spans []Span
}

func (s *recordSink) Emit(span Span) {
	s.spans = append(s.spans, span)
}
