package observability

import (
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
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
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
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body := recorder.Body.String(); strings.Contains(body, "migration") {
		t.Fatalf("diagnostics series rendered without a recorder:\n%s", body)
	}
}
