package main

import (
	"os"
	"testing"
)

// TestParseCodeRoutes reads the real registration shape used by the control
// plane (method + path literal in a mux call) and ignores everything else.
func TestParseCodeRoutes(t *testing.T) {
	source := `
package controlplane
func (server *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", server.handleHealth)
	mux.HandleFunc("POST /v1/organizations/{organizationID}/ownership/fence", server.handleOwnershipFence)
	mux.Handle("GET /metrics", server.metrics.Handler())
	notARoute := "POST /v1/ignored"
	return mux
}
`
	routes := parseCodeRoutes(source)
	for _, want := range []routeKey{
		{"GET", "/health"},
		{"POST", "/v1/organizations/{organizationID}/ownership/fence"},
		{"GET", "/metrics"},
	} {
		if !routes[want] {
			t.Fatalf("missing route %v in %v", want, routes)
		}
	}
	if len(routes) != 3 {
		t.Fatalf("parsed %d routes, want 3: %v", len(routes), routes)
	}
}

// TestParseSpecRoutes reads the canonical spec's indentation shape: paths at
// two spaces, operations at four.
func TestParseSpecRoutes(t *testing.T) {
	source := `openapi: 3.1.0
info:
  title: SHIFT API
paths:
  /health:
    get:
      summary: Health
  /v1/organizations/{organizationID}/ownership/fence:
    parameters: [{ $ref: '#/components/parameters/OrganizationID' }]
    post:
      operationId: fence
    delete:
      operationId: nope
components:
  schemas:
    Thing:
      type: object
`
	routes := parseSpecRoutes(source)
	for _, want := range []routeKey{
		{"GET", "/health"},
		{"POST", "/v1/organizations/{organizationID}/ownership/fence"},
		{"DELETE", "/v1/organizations/{organizationID}/ownership/fence"},
	} {
		if !routes[want] {
			t.Fatalf("missing spec route %v in %v", want, routes)
		}
	}
	if len(routes) != 3 {
		t.Fatalf("parsed %d spec routes, want 3: %v", len(routes), routes)
	}
}

// readRepoFile reads a repository-root-relative path from the package
// directory the test runs in.
func readRepoFile(relative string) ([]byte, error) {
	return os.ReadFile("../../" + relative)
}

// TestContractParsersAgreeOnTheRealFiles is the regression the CI step
// enforces: every registered control-plane route is documented, and every
// documented path is registered. If this fails, api/openapi/shift.yaml and
// internal/controlplane/server.go have drifted — see docs/api.md for the
// spec's role as the published contract.
func TestContractParsersAgreeOnTheRealFiles(t *testing.T) {
	specSource, err := readRepoFile("api/openapi/shift.yaml")
	if err != nil {
		t.Skipf("spec not reachable from test working directory: %v", err)
	}
	routeSource, err := readRepoFile("internal/controlplane/server.go")
	if err != nil {
		t.Skipf("route source not reachable from test working directory: %v", err)
	}
	specRoutes := parseSpecRoutes(string(specSource))
	codeRoutes := parseCodeRoutes(string(routeSource))
	if len(specRoutes) == 0 || len(codeRoutes) == 0 {
		t.Fatalf("parsers found nothing: %d spec, %d code", len(specRoutes), len(codeRoutes))
	}
	for route := range codeRoutes {
		if !specRoutes[route] {
			t.Errorf("UNDOCUMENTED ROUTE %s %s", route.method, route.path)
		}
	}
	for route := range specRoutes {
		if !codeRoutes[route] {
			t.Errorf("UNREGISTERED SPEC %s %s", route.method, route.path)
		}
	}
}
