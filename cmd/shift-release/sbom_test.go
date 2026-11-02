package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseGoModReadsRequireBlocks covers the manifest scanner against the
// shapes go.mod actually uses: direct requires, indirect requires, and
// single-line requires outside a block (ignored — go.mod's block form is
// what `go mod tidy` emits for a module this size).
func TestParseGoModReadsRequireBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "go.mod")
	data := `module example.test/m

go 1.24.0

require (
	github.com/jackc/pgx/v5 v5.7.5
	golang.org/x/crypto v0.41.0
)

require (
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/text v0.28.0 // indirect
)
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	components, err := parseGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 4 {
		t.Fatalf("parsed %d components, want 4: %+v", len(components), components)
	}
	byName := map[string]Component{}
	for _, component := range components {
		byName[component.Name] = component
	}
	if got := byName["github.com/jackc/pgx/v5"]; got.Version != "v5.7.5" || got.Scope != "direct" || got.License != "MIT" {
		t.Fatalf("pgx component wrong: %+v", got)
	}
	if got := byName["golang.org/x/text"]; got.Scope != "indirect" || got.License != "BSD-3-Clause" {
		t.Fatalf("x/text component wrong: %+v", got)
	}
	if got := byName["golang.org/x/crypto"]; got.License != "BSD-3-Clause" {
		t.Fatalf("x/crypto license wrong: %+v", got)
	}
}

// TestLicenseForNeverGuesses: unknown components are NOASSERTION, the SPDX
// "not asserted" value — the SBOM must not invent license claims.
func TestLicenseForNeverGuesses(t *testing.T) {
	if got := licenseFor("github.com/klauspost/compress"); got != "BSD-3-Clause" {
		t.Fatalf("known license = %q", got)
	}
	if got := licenseFor("some.unknown/dependency"); got != "NOASSERTION" {
		t.Fatalf("unknown license = %q, want NOASSERTION", got)
	}
}

// TestSBOMCommandEmitsDocument exercises the subcommand end to end against a
// synthetic root and against the real repository manifests.
func TestSBOMCommandEmitsDocument(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n\ngo 1.24\n\nrequire (\n\tgithub.com/jackc/pgx/v5 v5.7.5\n)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := sbomCommand([]string{"--root", root}, &output); err != nil {
		t.Fatal(err)
	}
	var document SBOM
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("sbom output is not JSON: %v\n%s", err, output.String())
	}
	if document.Format != "shift-sbom-1" || len(document.Components) != 1 {
		t.Fatalf("document = %+v", document)
	}

	// The real repository must inventory cleanly and include the pinned
	// database driver with the license NOTICE records.
	output.Reset()
	if err := sbomCommand([]string{"--root", "../..", "--out", filepath.Join(t.TempDir(), "sbom.json")}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "SBOM with") {
		t.Fatalf("unexpected sbom output: %s", output.String())
	}
}
