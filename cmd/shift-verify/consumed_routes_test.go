package main

import "testing"

// Regression cover for the contract-tool gap (ship finding 02 #16): the
// agent consumes commands/poll and commands/complete from the control plane,
// both sides of the code↔spec diff omitted them, and the checker passed 66/66
// while relay migrations were undeliverable. Client-built /v1/ templates must
// resolve to registered routes or the check fails.
func TestParseConsumedRoutes(t *testing.T) {
	source := `
package controlplane
func (reporter *Reporter) pollCommand(ctx context.Context) {
	endpoint := fmt.Sprintf("%s/v1/organizations/%s/machines/%s/commands/poll", reporter.url, reporter.organizationID, reporter.machineID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	_ = req
	completeEndpoint := fmt.Sprintf("%s/v1/organizations/%s/machines/%s/commands/%s/complete", reporter.url, reporter.organizationID, reporter.machineID, command.ID)
	complete, createErr := http.NewRequestWithContext(ctx, http.MethodPost, completeEndpoint, bytes.NewReader(body))
	_ = complete
	_ = createErr
	_ = err
}
`
	routes := parseConsumedRoutes(source, "reporter.go")
	if len(routes) != 2 {
		t.Fatalf("parsed %d consumed routes, want 2: %+v", len(routes), routes)
	}
	wantPoll := consumedRoute{method: "GET", path: "/v1/organizations/*/machines/*/commands/poll", source: "reporter.go:4"}
	wantComplete := consumedRoute{method: "POST", path: "/v1/organizations/*/machines/*/commands/*/complete", source: "reporter.go:7"}
	if routes[0] != wantPoll {
		t.Fatalf("poll route = %+v, want %+v", routes[0], wantPoll)
	}
	if routes[1] != wantComplete {
		t.Fatalf("complete route = %+v, want %+v", routes[1], wantComplete)
	}
}

// A client template that no mux registration matches must be detectable, and
// the structural comparison must tolerate placeholder spelling differences.
func TestConsumedRegistered(t *testing.T) {
	codeRoutes := map[routeKey]bool{
		{method: "GET", path: "/v1/organizations/{organizationID}/machines/{machineID}/commands/poll"}: true,
		{method: "POST", path: "/v1/organizations/{organizationID}/machines/{machineID}/heartbeat"}:    true,
	}
	poll := consumedRoute{method: "GET", path: "/v1/organizations/*/machines/*/commands/poll"}
	if !consumedRegistered(poll, codeRoutes) {
		t.Fatal("registered commands/poll must satisfy the consumed route")
	}
	wrongMethod := consumedRoute{method: "POST", path: "/v1/organizations/*/machines/*/commands/poll"}
	if consumedRegistered(wrongMethod, codeRoutes) {
		t.Fatal("a consumed route must match the method the client uses")
	}
	ghost := consumedRoute{method: "GET", path: "/v1/organizations/*/machines/*/commands/*/complete"}
	if consumedRegistered(ghost, codeRoutes) {
		t.Fatal("unregistered commands/complete must not match anything")
	}
}

// The live repository is the regression CI runs: every endpoint the shipped
// clients build must be registered in the control plane's mux. This is the
// check that would have caught the commands/poll drift.
func TestAgentConsumedRoutesAreRegistered(t *testing.T) {
	routeSource, err := readRepoFile("internal/controlplane/server.go")
	if err != nil {
		t.Skipf("route source not reachable from test working directory: %v", err)
	}
	consumed, err := collectConsumedRoutes("../../internal/controlplane/reporter.go,../../internal/agent")
	if err != nil {
		t.Skipf("client sources not reachable from test working directory: %v", err)
	}
	if len(consumed) == 0 {
		t.Fatal("client scan found no /v1/ templates; the parser or the client layout changed")
	}
	codeRoutes := parseCodeRoutes(string(routeSource))
	for _, route := range consumed {
		if !consumedRegistered(route, codeRoutes) {
			t.Errorf("AGENT-CONSUMED UNREGISTERED %s %s (%s)", route.method, route.path, route.source)
		}
	}
}
