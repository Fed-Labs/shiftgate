package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/securestore"
	"shift.dev/shift/internal/supportmatrix"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("shift-verify", config.Version)
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: shift-verify support-probe|state|api-contract --matrix PATH --state-dir PATH --spec PATH --routes PATH [--json]")
		fmt.Fprintln(os.Stderr, "exit codes: 0 certified/verified, 2 usage, 3 not certified, 4 platform unsupported, 5 state verification failed")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "support-probe":
		verifySupportProbe(os.Args[2:])
	case "state":
		verifyState(os.Args[2:])
	case "api-contract":
		verifyAPIContract(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: shift-verify support-probe|state|api-contract")
		os.Exit(2)
	}
}

// Exit codes for support-probe. They are the enforcement surface for the
// support matrix's policy block: 0 means certified (or explicitly accepted
// experimental), 3 means the combination is not certified so automatic
// operations are blocked, 4 means the platform is unsupported, 2 means the
// probe itself was invoked wrong or the matrix is unreadable, and 5 means
// encrypted-state verification failed (corrupt, incomplete, or empty state) —
// never 4, so automation can tell "restore is bad" from "host is unsupported".
const (
	exitCertified    = 0
	exitUsage        = 2
	exitNotCertified = 3
	exitUnsupported  = 4
	exitStateFailed  = 5
)

func verifySupportProbe(arguments []string) {
	flags := flag.NewFlagSet("support-probe", flag.ExitOnError)
	matrixPath := flags.String("matrix", "docs/support-matrix.json", "support matrix JSON")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	allowExperimental := flags.Bool("allow-experimental", false, "accept experimental_only combinations (operator override; still refuses unknown and unsupported)")
	_ = flags.Parse(arguments)
	matrix, err := supportmatrix.Load(*matrixPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitUsage)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	evidence := supportmatrix.Probe(ctx, matrix)
	decision := matrix.Decide(evidence)
	if *jsonOutput {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			supportmatrix.Evidence
			Decision supportmatrix.Decision `json:"decision"`
		}{evidence, decision})
	} else {
		fmt.Printf("matrix=%s\narchitecture=%s\ndistribution=%s\nkernel=%s\ncriu=%s\ncriu_check=%s\nprobe_status=%s\nmatrix_status=%s\naction=%s\nallow_automatic=%v\nreason=%s\n",
			evidence.MatrixVersion, evidence.Architecture, evidence.Distribution, evidence.Kernel, evidence.CRIU, evidence.CRIUCheck,
			evidence.PlatformStatus, decision.Status, decision.Action, decision.AllowAutomatic, decision.Reason)
	}
	// Enforcement of the matrix policy (B-6): a combination the matrix does
	// not certify fails non-zero, so nothing downstream can treat an unknown
	// platform as green. Unsupported always fails; unknown and
	// experimental_only fail unless the operator opts in explicitly.
	allow := decision.AllowAutomatic
	if !allow && *allowExperimental && decision.Action == supportmatrix.ActionExperimentalOnly {
		allow = true
		// The notice goes to stderr: with --json the stdout stream is one
		// JSON value and a trailing plain line would break strict decoders.
		fmt.Fprintln(os.Stderr, "override: --allow-experimental accepted an experimental_only combination")
	}
	if allow {
		os.Exit(exitCertified)
	}
	if decision.Status == "unsupported" {
		os.Exit(exitUnsupported)
	}
	os.Exit(exitNotCertified)
}

func verifyState(arguments []string) {
	flags := flag.NewFlagSet("state", flag.ExitOnError)
	stateDir := flags.String("state-dir", "", "agent state directory")
	_ = flags.Parse(arguments)
	if *stateDir == "" {
		fmt.Fprintln(os.Stderr, "--state-dir is required")
		os.Exit(exitUsage)
	}
	if err := verifyStateDir(context.Background(), *stateDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitStateFailed)
	}
	fmt.Println("encrypted state verification passed")
}

// verifyStateDir verifies one restored state directory and refuses to certify
// nothing. The store libraries create missing keys, indexes, and chunk
// directories on open, so the directory must already hold real state before
// the verifier touches it: without that, an empty or nonexistent directory
// verifies a trivially empty loop, prints "passed", and even writes fresh key
// material into the directory it was asked to certify read-only.
func verifyStateDir(ctx context.Context, stateDir string) error {
	if err := requireStateArtifacts(stateDir); err != nil {
		return err
	}
	if err := supportmatrix.VerifyState(ctx, stateDir); err != nil {
		return err
	}
	return checkManifestCoverage(stateDir)
}

// requireStateArtifacts insists the directory under test already holds the
// keyring, checkpoint index, and chunk directory — the artifacts every real
// agent state has and every empty, nonexistent, or wrong path lacks.
func requireStateArtifacts(stateDir string) error {
	info, err := os.Stat(stateDir)
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("state directory %s is not a directory", stateDir)
	}
	for _, artifact := range []string{
		filepath.Join("keys", "master-keyring.json"),
		filepath.Join("checkpoints", "index.enc.json"),
		filepath.Join("objects", "chunks"),
	} {
		if _, err := os.Stat(filepath.Join(stateDir, artifact)); err != nil {
			return fmt.Errorf("state directory is missing %s — empty, partial, or not an agent state directory: %w", artifact, err)
		}
	}
	return nil
}

// checkManifestCoverage fails the run when nothing was verified or when the
// checkpoint directory holds manifests the index does not list: a restore
// that lost its index cannot load those checkpoints and maintenance will
// scavenge their chunks, so "verified" must never be reported for them.
func checkManifestCoverage(stateDir string) error {
	keys, err := securestore.Open(filepath.Join(stateDir, "keys"))
	if err != nil {
		return fmt.Errorf("open restored keys: %w", err)
	}
	repository, err := checkpoint.OpenRepository(filepath.Join(stateDir, "checkpoints"), keys)
	if err != nil {
		return fmt.Errorf("open restored checkpoint repository: %w", err)
	}
	indexed := map[string]bool{}
	for _, summary := range repository.List("") {
		indexed[summary.ID] = true
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "checkpoints"))
	if err != nil {
		return fmt.Errorf("read checkpoint directory: %w", err)
	}
	var unindexed []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(stateDir, "checkpoints", entry.Name(), "manifest.enc.json")); err != nil {
			continue
		}
		if !indexed[entry.Name()] {
			unindexed = append(unindexed, entry.Name())
		}
	}
	sort.Strings(unindexed)
	if len(unindexed) > 0 {
		return fmt.Errorf("unindexed checkpoint manifest(s) not listed in checkpoints/index.enc.json: %s", strings.Join(unindexed, ", "))
	}
	if len(indexed) == 0 {
		return errors.New("state directory contains no checkpoints to verify")
	}
	return nil
}
