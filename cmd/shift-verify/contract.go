package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// api-contract diffs the routes the control plane registers against the paths
// the OpenAPI spec promises. The spec is the published contract
// (docs/api.md): a route that exists only in code is an undocumented
// endpoint, and a spec path that exists only in the spec is a promise nobody
// serves. Both directions fail the check.
//
// The third leg is the agent-consumed route set: URLs that shipped clients
// (the agent's control-plane reporter, the standby failover client, the
// hosted-storage credential fetch) build and call. A route a client consumes
// but the server never registers fails every request and yet sits invisible
// to a code↔spec diff when both sides omit it — the commands/poll drift that
// left relay migrations undeliverable while this check reported 66/66. Every
// fmt.Sprintf("%s/v1/…") client template must resolve to a registered route.

var routePattern = regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+) ([^"]+)"`)

// consumedPattern captures client endpoint templates: a variable assigned
// from fmt.Sprintf whose format string starts with the %s base-URL placeholder
// followed by a /v1/... path carrying %s/%d parameter segments.
var consumedPattern = regexp.MustCompile(`(\w+)\s*:?=\s*fmt\.Sprintf\("%s(/v1/[^"]*)"`)

var specMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true, "options": true}

type routeKey struct{ method, path string }

// consumedRoute is one client-built endpoint: the template's structure, the
// HTTP method the client uses, and where it was found for drift reports.
type consumedRoute struct {
	method string
	path   string // structural form: parameter segments are "*"
	source string
}

func verifyAPIContract(arguments []string) {
	flags := flag.NewFlagSet("api-contract", flag.ExitOnError)
	specPath := flags.String("spec", "api/openapi/shift.yaml", "OpenAPI document")
	routesPath := flags.String("routes", "internal/controlplane/server.go", "control-plane route registration source")
	clients := flags.String("clients", "internal/controlplane/reporter.go,internal/agent", "comma-separated client sources (files or directories) whose /v1/ URL templates must be registered")
	_ = flags.Parse(arguments)

	specSource, err := os.ReadFile(*specPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	routeSource, err := os.ReadFile(*routesPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	specRoutes := parseSpecRoutes(string(specSource))
	codeRoutes := parseCodeRoutes(string(routeSource))

	var missingFromSpec, missingFromCode []routeKey
	for route := range codeRoutes {
		if !specRoutes[route] {
			missingFromSpec = append(missingFromSpec, route)
		}
	}
	for route := range specRoutes {
		if !codeRoutes[route] {
			missingFromCode = append(missingFromCode, route)
		}
	}
	sortRoutes(missingFromSpec)
	sortRoutes(missingFromCode)

	consumed, err := collectConsumedRoutes(*clients)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var unregisteredConsumed []consumedRoute
	for _, route := range consumed {
		if !consumedRegistered(route, codeRoutes) {
			unregisteredConsumed = append(unregisteredConsumed, route)
		}
	}
	sort.Slice(unregisteredConsumed, func(i, j int) bool {
		if unregisteredConsumed[i].path != unregisteredConsumed[j].path {
			return unregisteredConsumed[i].path < unregisteredConsumed[j].path
		}
		return unregisteredConsumed[i].method < unregisteredConsumed[j].method
	})

	if len(missingFromSpec) == 0 && len(missingFromCode) == 0 && len(unregisteredConsumed) == 0 {
		fmt.Printf("api contract ok: %d spec routes match registered routes; %d agent-consumed routes are registered\n", len(specRoutes), len(consumed))
		return
	}
	for _, route := range missingFromSpec {
		fmt.Printf("UNDOCUMENTED ROUTE  %s %s  (registered in %s, absent from %s)\n", route.method, route.path, *routesPath, *specPath)
	}
	for _, route := range missingFromCode {
		fmt.Printf("UNREGISTERED SPEC   %s %s  (documented in %s, absent from %s)\n", route.method, route.path, *specPath, *routesPath)
	}
	for _, route := range unregisteredConsumed {
		fmt.Printf("AGENT-CONSUMED UNREGISTERED  %s %s  (consumed by %s, no matching registration in %s)\n", route.method, route.path, route.source, *routesPath)
	}
	fmt.Printf("api contract drift: %d undocumented route(s), %d unregistered spec path(s), %d unregistered agent-consumed route(s)\n", len(missingFromSpec), len(missingFromCode), len(unregisteredConsumed))
	os.Exit(1)
}

// collectConsumedRoutes walks the client sources (files or directories of
// non-test .go files) and extracts every /v1/ endpoint template together with
// the method the request is built with.
func collectConsumedRoutes(spec string) ([]consumedRoute, error) {
	var files []string
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		info, err := os.Stat(entry)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, entry)
			continue
		}
		// Client sources are walked recursively: a flat glob would silently
		// miss a client in a subdirectory — the exact "both sides omit it"
		// failure mode this check exists to prevent.
		err = filepath.WalkDir(entry, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var routes []consumedRoute
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		routes = append(routes, parseConsumedRoutes(string(source), file)...)
	}
	return routes, nil
}

// parseConsumedRoutes extracts client-built endpoint templates from one Go
// source file. The method is taken from the http.NewRequest* call that uses
// the endpoint variable; when no such call exists the route is checked for
// path presence under any method.
func parseConsumedRoutes(source, file string) []consumedRoute {
	lines := strings.Split(source, "\n")
	var routes []consumedRoute
	for index, line := range lines {
		match := consumedPattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		variable, template := match[1], match[2]
		method := ""
		methodCallPattern := regexp.MustCompile(`http\.Method(\w+)\s*,\s*` + regexp.QuoteMeta(variable) + `\b`)
		// The method search runs to the end of the caller's scope instead of
		// a fixed 30-line window: request construction far below the template
		// must still resolve, and stopping at a reassignment of the variable
		// keeps a later, unrelated request from claiming this route's method.
		reassignPattern := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(variable) + `\s*:?=`)
		for _, following := range lines[index+1:] {
			if reassignPattern.MatchString(following) {
				break
			}
			if call := methodCallPattern.FindStringSubmatch(following); call != nil {
				method = call[1]
				break
			}
		}
		routes = append(routes, consumedRoute{
			method: strings.ToUpper(method),
			path:   normalizeRoutePath(template),
			source: fmt.Sprintf("%s:%d", file, index+1),
		})
	}
	return routes
}

// normalizeRoutePath collapses parameter segments — {name} in registrations
// and specs, %s/%d in client templates — to "*", so a client template and a
// registration compare by structure rather than by placeholder spelling.
func normalizeRoutePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if strings.ContainsAny(segment, "%{}") {
			segments[i] = "*"
		}
	}
	return strings.Join(segments, "/")
}

// consumedRegistered reports whether some registered route matches the
// consumed route's structure (and method, when the client names one).
func consumedRegistered(route consumedRoute, codeRoutes map[routeKey]bool) bool {
	for registered := range codeRoutes {
		if normalizeRoutePath(registered.path) != route.path {
			continue
		}
		if route.method == "" || registered.method == route.method {
			return true
		}
	}
	return false
}

// parseCodeRoutes extracts "METHOD /path" registrations from Go source.
func parseCodeRoutes(source string) map[routeKey]bool {
	routes := make(map[routeKey]bool)
	for _, match := range routePattern.FindAllStringSubmatch(source, -1) {
		routes[routeKey{method: match[1], path: match[2]}] = true
	}
	return routes
}

// parseSpecRoutes extracts paths × methods from an OpenAPI document. The
// parser is deliberately line-based and strict about indentation: paths sit
// at two spaces under `paths:`, operations at four. This is enough for the
// repository's canonical spec without taking a YAML dependency.
func parseSpecRoutes(source string) map[routeKey]bool {
	routes := make(map[routeKey]bool)
	inPaths := false
	currentPath := ""
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if !inPaths {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") {
			break // next top-level document key
		}
		trimmed := strings.TrimRight(line, " ")
		if strings.HasPrefix(trimmed, "  /") && !strings.HasPrefix(trimmed, "   ") {
			currentPath = strings.TrimSuffix(strings.TrimSpace(trimmed), ":")
			continue
		}
		if currentPath != "" && strings.HasPrefix(trimmed, "    ") && !strings.HasPrefix(trimmed, "     ") {
			method := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(trimmed), ":"))
			if specMethods[method] {
				routes[routeKey{method: strings.ToUpper(method), path: currentPath}] = true
			}
		}
	}
	return routes
}

func sortRoutes(routes []routeKey) {
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].path != routes[j].path {
			return routes[i].path < routes[j].path
		}
		return routes[i].method < routes[j].method
	})
}
