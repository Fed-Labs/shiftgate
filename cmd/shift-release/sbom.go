package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// sbom.go: release software bill of materials.
//
// The SBOM is an honest inventory, not a license oracle: it lists every
// declared dependency with the version the build pinned, and it names a
// license only where this repository's NOTICE already records one. Anything
// else is NOASSERTION — the standard SPDX answer for "not asserted" — so the
// document never invents a license claim. Regenerate it whenever go.mod,
// go.sum, or a frontend manifest changes and attach it to the release.

// Component is one dependency of the shipped artifacts.
type Component struct {
	Name      string `json:"name"`
	Version   string `json:"version,omitempty"`
	Ecosystem string `json:"ecosystem"`
	Scope     string `json:"scope"`
	License   string `json:"license"`
	Source    string `json:"source"`
}

// SBOM is the document `shift-release sbom` emits: one JSON object with the
// generation metadata and the component list.
type SBOM struct {
	Format     string      `json:"format"`
	Version    string      `json:"version"`
	Generated  time.Time   `json:"generated"`
	Repository string      `json:"repository"`
	Components []Component `json:"components"`
}

// knownLicenses mirrors NOTICE: name → SPDX id. Components outside this map
// are reported as NOASSERTION rather than guessed.
var knownLicenses = map[string]string{
	"github.com/jackc/pgx/v5":        "MIT",
	"github.com/jackc/pgpassfile":    "MIT",
	"github.com/jackc/pgservicefile": "MIT",
	"github.com/jackc/puddle/v2":     "MIT",
	"github.com/klauspost/compress":  "BSD-3-Clause",
	"golang.org/x/crypto":            "BSD-3-Clause",
	"golang.org/x/sync":              "BSD-3-Clause",
	"golang.org/x/sys":               "BSD-3-Clause",
	"golang.org/x/text":              "BSD-3-Clause",
	"golang.org/x/net":               "BSD-3-Clause",
	"next":                           "MIT",
	"react":                          "MIT",
	"framer-motion":                  "MIT",
}

func licenseFor(name string) string {
	if id, ok := knownLicenses[name]; ok {
		return id
	}
	return "NOASSERTION"
}

func sbomCommand(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("sbom", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root to inventory")
	outPath := flags.String("out", "", "where to write the SBOM JSON (default stdout)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	components, err := collectComponents(*root)
	if err != nil {
		return err
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Ecosystem != components[j].Ecosystem {
			return components[i].Ecosystem < components[j].Ecosystem
		}
		return components[i].Name < components[j].Name
	})
	document := SBOM{
		Format:     "shift-sbom-1",
		Version:    "1.0",
		Generated:  time.Now().UTC(),
		Repository: "https://github.com/Fed-Labs/shiftgate",
		Components: components,
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if *outPath == "" {
		_, err = output.Write(encoded)
		return err
	}
	if err := os.WriteFile(*outPath, encoded, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(output, "SBOM with %d components written to %s.\n", len(components), *outPath)
	return nil
}

// collectComponents walks the dependency manifests the repository actually
// ships: the Go module, and the two frontend manifests. Transitive Go
// dependencies come from go.mod's require blocks (indirect ones included,
// marked as such); package manifests list their own direct ranges.
func collectComponents(root string) ([]Component, error) {
	var components []Component
	goModules, err := parseGoMod(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, err
	}
	components = append(components, goModules...)
	for _, manifest := range []string{"apps/web/package.json", "apps/desktop/package.json"} {
		frontend, err := parsePackageJSON(filepath.Join(root, manifest), manifest)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		components = append(components, frontend...)
	}
	return components, nil
}

var goRequirePattern = regexp.MustCompile(`^\s*([^\s]+)\s+(v[^\s]+)(\s*//\s*(indirect))?`)

// parseGoMod extracts require-block entries. go.mod is a line-oriented
// document for the parts an SBOM needs, so a strict scanner is enough and
// keeps the tool dependency-free.
func parseGoMod(path string) ([]Component, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var components []Component
	inRequire := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "require (" {
			inRequire = true
			continue
		}
		if inRequire && trimmed == ")" {
			inRequire = false
			continue
		}
		if !inRequire {
			continue
		}
		match := goRequirePattern.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		scope := "direct"
		if match[4] != "" {
			scope = "indirect"
		}
		components = append(components, Component{
			Name: match[1], Version: match[2], Ecosystem: "go", Scope: scope,
			License: licenseFor(match[1]), Source: "go.mod",
		})
	}
	return components, nil
}

// parsePackageJSON extracts the dependencies and devDependencies objects of a
// Node manifest without a JSON dependency: only those two keys are arrays of
// "name": "range" pairs at one fixed nesting level.
func parsePackageJSON(path, label string) ([]Component, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	var components []Component
	add := func(dependencies map[string]string, scope string) {
		for name, version := range dependencies {
			components = append(components, Component{
				Name: name, Version: version, Ecosystem: "npm", Scope: scope,
				License: licenseFor(name), Source: label,
			})
		}
	}
	add(manifest.Dependencies, "direct")
	add(manifest.DevDependencies, "dev")
	return components, nil
}
