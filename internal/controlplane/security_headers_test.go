package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"shift.dev/shift/internal/config"
)

// security_headers_test.go pins the baseline hardening headers every response
// carries — including the CORS short-circuits — and the one deliberate
// exception (handlers caching on purpose).

func newSecurityHeaderServer() *Server {
	return New(config.DefaultControlPlane(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestSecurityHeadersBaseline(t *testing.T) {
	server := newSecurityHeaderServer()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
	if got := recorder.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	// A plain-HTTP request must not pin hosts to https: HSTS is TLS-only.
	if got := recorder.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("HSTS on a plaintext request = %q, want absent", got)
	}
}

func TestSecurityHeadersHSTSBehindTLSEdge(t *testing.T) {
	server := newSecurityHeaderServer()
	// The TLS-terminating edge forwards over plaintext and says so in
	// X-Forwarded-Proto; the browser saw https and must receive HSTS.
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if got := recorder.Header().Get("Strict-Transport-Security"); got != "max-age=31536000" {
		t.Fatalf("HSTS behind TLS edge = %q, want max-age=31536000", got)
	}
}

// Handlers caching on purpose override the no-store default — the public
// stats page publishes Cache-Control: public, max-age=300 and must keep it.
func TestSecurityHeadersAllowHandlerCacheOverride(t *testing.T) {
	server := newSecurityHeaderServer()
	handler := server.securityHeaders(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Cache-Control", "public, max-age=300")
		writer.WriteHeader(http.StatusOK)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/public/stats", nil))
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("handler Cache-Control = %q, want the handler's own value", got)
	}
}

// The preflight answer is the response a hostile page provokes most easily;
// it must carry the hardening headers like everything else (but no
// Cache-Control — preflight caching rides Access-Control-Max-Age).
func TestSecurityHeadersOnPreflightShortCircuit(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.AllowedOrigins = []string{"http://localhost:3001"}
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/auth/register", nil)
	request.Header.Set("Origin", "http://localhost:3001")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", recorder.Code)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("preflight X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("preflight Cache-Control = %q, want absent", got)
	}
}
