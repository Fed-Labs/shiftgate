package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsHandlerComposesDiagnostics(t *testing.T) {
	metrics := NewMetrics()
	diagnostics := NewDiagnostics()
	diagnostics.MigrationFinished(OutcomeSuccess, time.Second, 0)
	metrics.SetDiagnostics(diagnostics)
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, expected := range []string{
		"shift_uptime_seconds",
		"shift_http_requests_total 0",
		`shift_migration_outcomes_total{outcome="success"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics output missing %q:\n%s", expected, body)
		}
	}
	if recorder.Header().Get("Content-Type") != "text/plain; version=0.0.4" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestMetricsHandlerWithoutDiagnostics(t *testing.T) {
	metrics := NewMetrics()
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if body := recorder.Body.String(); strings.Contains(body, "migration") {
		t.Fatalf("diagnostics series rendered without a recorder:\n%s", body)
	}
}

// TestMetricsHandlerRendersEachSeriesOnce is the duplicate-sample guard: the
// histogram's own _sum/_count pair is the only writer of those names, so a
// Prometheus scrape sees exactly one sample per series.
func TestMetricsHandlerRendersEachSeriesOnce(t *testing.T) {
	metrics := NewMetrics()
	metrics.ObserveDuration(0.02)
	metrics.ObserveDuration(0.3)
	metrics.Middleware(NewLogger("error"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/ping", nil))
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	counts := map[string]int{}
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		counts[strings.Fields(line)[0]]++
	}
	for _, series := range []string{
		"shift_http_request_duration_seconds_sum",
		"shift_http_request_duration_seconds_count",
		"shift_http_request_duration_seconds_bucket{le=\"+Inf\"}",
	} {
		if counts[series] != 1 {
			t.Fatalf("series %q rendered %d times, want 1:\n%s", series, counts[series], recorder.Body.String())
		}
	}
}

func TestRegisterGaugeRejectsSeriesSmugglingNames(t *testing.T) {
	metrics := NewMetrics()
	metrics.RegisterGauge(`evil{label="x"}`, "smuggled", func() float64 { return 1 })
	metrics.RegisterGauge("good_name", "fine", func() float64 { return 2 })
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	if strings.Contains(body, "evil") {
		t.Fatalf("a label-smuggling gauge name reached the scrape:\n%s", body)
	}
	if !strings.Contains(body, "shift_good_name 2") {
		t.Fatalf("valid gauge missing:\n%s", body)
	}
}
