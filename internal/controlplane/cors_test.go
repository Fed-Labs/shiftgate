package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shift.dev/shift/internal/config"
)

// performCORSRequest runs one request through the CORS middleware wrapped
// around a handler that records whether it ran, so a test can assert both the
// response the browser sees and what the middleware forwarded.
func performCORSRequest(origins []string, request *http.Request) (*httptest.ResponseRecorder, bool) {
	configuration := config.DefaultControlPlane()
	configuration.AllowedOrigins = origins
	reached := false
	server := &Server{config: configuration}
	handler := server.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder, reached
}

func TestCORSServesAllowedOrigin(t *testing.T) {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/me", nil)
	request.Header.Set("Origin", "http://localhost:3001")
	response, reached := performCORSRequest([]string{"http://localhost:3001"}, request)
	if !reached {
		t.Fatal("a request from an allowed origin must reach the wrapped handler")
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3001" {
		t.Fatalf("the allowed origin must be echoed exactly, got %q", got)
	}
	if got := response.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("responses must be marked as varying by origin, got %q", got)
	}
}

func TestCORSPreflightIsAnsweredByTheMiddleware(t *testing.T) {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/auth/register", nil)
	request.Header.Set("Origin", "http://localhost:3001")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	response, reached := performCORSRequest([]string{"http://localhost:3001"}, request)
	if reached {
		t.Fatal("a preflight must be answered without reaching the wrapped handler")
	}
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight must answer 204, got %d", response.Code)
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3001" {
		t.Fatalf("preflight must echo the allowed origin, got %q", got)
	}
	methods := response.Header().Get("Access-Control-Allow-Methods")
	if !strings.Contains(methods, http.MethodPost) || !strings.Contains(methods, http.MethodDelete) {
		t.Fatalf("preflight must allow the API's methods, got %q", methods)
	}
	headers := response.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(headers, "Authorization") || !strings.Contains(headers, "Content-Type") {
		t.Fatalf("preflight must allow the dashboard's headers, got %q", headers)
	}
	if response.Header().Get("Access-Control-Max-Age") == "" {
		t.Fatal("preflight must set a cache duration")
	}
}

func TestCORSServesUnlistedOriginsWithoutCORSHeaders(t *testing.T) {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/me", nil)
	// Same host, different port: an origin is scheme, host, and port together.
	request.Header.Set("Origin", "http://localhost:3000")
	response, reached := performCORSRequest([]string{"http://localhost:3001"}, request)
	if !reached {
		t.Fatal("an unlisted origin is still served — only the browser declines it")
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("an unlisted origin must not be echoed, got %q", got)
	}
}

func TestCORSPassesOriginlessRequestsThrough(t *testing.T) {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/me", nil)
	response, reached := performCORSRequest([]string{"http://localhost:3001"}, request)
	if !reached {
		t.Fatal("CLI and agent requests carry no Origin and must pass through")
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("a request without Origin must not gain CORS headers, got %q", got)
	}
}

func TestCORSAllowsNoBrowserOriginByDefault(t *testing.T) {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/me", nil)
	request.Header.Set("Origin", "http://localhost:3001")
	response, reached := performCORSRequest(nil, request)
	if !reached {
		t.Fatal("the request itself must still be served")
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("no origin is allowed when allowed_origins is unset, got %q", got)
	}
	if got := response.Header().Get("Vary"); got != "" {
		t.Fatalf("responses must not be marked as varying when nothing is allowed, got %q", got)
	}
}

func TestHandlerChainAnswersPreflight(t *testing.T) {
	configuration := config.DefaultControlPlane()
	configuration.AllowedOrigins = []string{"http://localhost:3001"}
	server := New(configuration, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodOptions, "/v1/auth/register", nil)
	request.Header.Set("Origin", "http://localhost:3001")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("the full handler chain must answer a preflight with 204, got %d", recorder.Code)
	}
	if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3001" {
		t.Fatalf("the full handler chain must echo the allowed origin, got %q", got)
	}
}
