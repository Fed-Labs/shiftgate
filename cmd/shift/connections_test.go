package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectionsResolveAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connections.json")
	t.Setenv("SHIFT_CONNECTIONS_FILE", path)
	var output bytes.Buffer
	settings := options{stdout: &output, stderr: &output}
	if err := runConnect(context.Background(), settings, []string{"gpu-west", "--to", "https://gpu.example:8443", "--machine-id", "machine-real"}); err != nil {
		t.Fatal(err)
	}
	destination, err := resolveDestination("gpu-west", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if destination.AgentURL != "https://gpu.example:8443" || destination.MachineID != "machine-real" || destination.ServerName != "gpu.example" {
		t.Fatalf("destination = %#v", destination)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("connections mode = %o", info.Mode().Perm())
	}
}

// TestDisconnectUnknownAliasIsHonest proves disconnect no longer claims
// success for an alias that was never saved.
func TestDisconnectUnknownAliasIsHonest(t *testing.T) {
	t.Setenv("SHIFT_CONNECTIONS_FILE", filepath.Join(t.TempDir(), "connections.json"))
	var output bytes.Buffer
	settings := options{stdout: &output, stderr: &output}
	err := runDisconnect(settings, []string{"ghost"})
	if err == nil {
		t.Fatal("disconnect of an unknown alias must fail")
	}
	var exit exitError
	if !errors.As(err, &exit) || exit.code != 4 {
		t.Fatalf("want exit code 4, got %v", err)
	}
}

// TestConnectReportsAliasReplacement proves an overwrite says what it
// replaced instead of a flat "Saved".
func TestConnectReportsAliasReplacement(t *testing.T) {
	t.Setenv("SHIFT_CONNECTIONS_FILE", filepath.Join(t.TempDir(), "connections.json"))
	var output bytes.Buffer
	settings := options{stdout: &output, stderr: &output}
	if err := runConnect(context.Background(), settings, []string{"gpu", "--to", "https://old.example:8443", "--machine-id", "m1"}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runConnect(context.Background(), settings, []string{"gpu", "--to", "https://new.example:8443", "--machine-id", "m1"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Replaced existing connection gpu (was https://old.example:8443)") {
		t.Fatalf("overwrite not reported: %q", output.String())
	}
}
