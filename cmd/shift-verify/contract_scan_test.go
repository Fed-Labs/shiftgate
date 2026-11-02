package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCollectConsumedRoutesFindsNestedClients covers the flat-glob hole: a
// client below the top level of a --clients directory must not escape the
// AGENT-CONSUMED check. The template sits 40 lines above its request
// construction, which the old 30-line method window could not see.
func TestCollectConsumedRoutesFindsNestedClients(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "internal", "agent", "subpkg")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package subpkg

import (
	"context"
	"fmt"
	"net/http"
)

func poll(ctx context.Context, base, org, machine string) *http.Request {
	url := fmt.Sprintf("%s/v1/organizations/%s/machines/%s/commands/poll", base, org, machine)
` + strings.Repeat("\t_ = url\n", 40) + `	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	return request
}
`
	if err := os.WriteFile(filepath.Join(nested, "client.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	routes, err := collectConsumedRoutes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("nested client not collected: %#v", routes)
	}
	if routes[0].method != "POST" {
		t.Fatalf("method beyond the old window not resolved: %#v", routes[0])
	}
	if routes[0].path != "/v1/organizations/*/machines/*/commands/poll" {
		t.Fatalf("path = %q", routes[0].path)
	}
}

// TestConsumedRouteMethodStopsAtReassignment keeps a later, unrelated request
// from claiming an earlier template's method after the variable is reused.
func TestConsumedRouteMethodStopsAtReassignment(t *testing.T) {
	root := t.TempDir()
	source := `package client

import (
	"context"
	"fmt"
	"net/http"
)

func first(ctx context.Context, base string) *http.Request {
	url := fmt.Sprintf("%s/v1/first", base)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	return request
}

func second(ctx context.Context, base string) *http.Request {
	url := fmt.Sprintf("%s/v1/second", base)
	request, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	return request
}
`
	if err := os.WriteFile(filepath.Join(root, "client.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	routes, err := collectConsumedRoutes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %#v", routes)
	}
	if routes[0].path != "/v1/first" || routes[0].method != "GET" {
		t.Fatalf("first route = %#v", routes[0])
	}
	if routes[1].path != "/v1/second" || routes[1].method != "DELETE" {
		t.Fatalf("second route = %#v", routes[1])
	}
}
