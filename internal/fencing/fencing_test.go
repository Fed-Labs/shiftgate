package fencing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDisabledAndSelfNeverAuthorizeAutomaticFailover(t *testing.T) {
	for _, provider := range []Provider{Disabled{}, Self{}} {
		result, err := provider.Fence(context.Background(), Request{WorkloadID: "w", SourceMachineID: "s", Epoch: 2})
		if AllowsAutomatic(result) || err == nil {
			t.Fatalf("%s returned an automatic-failover result: %s %v", provider.Name(), result, err)
		}
	}
}

func TestCommandProviderRequiresPositiveExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-specific")
	}
	directory := t.TempDir()
	script := filepath.Join(directory, "fence.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(directory, "status.sh")
	if err := os.WriteFile(status, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	result, err := (Command{Path: script, StatusPath: status, Timeout: time.Second, AllowedDirs: []string{directory}}).Fence(context.Background(), Request{WorkloadID: "w"})
	if err != nil || !AllowsAutomatic(result) {
		t.Fatalf("successful fence = %s %v", result, err)
	}
}

func TestCommandProviderRefusesPathsOutsideAllowlist(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-specific")
	}
	directory := t.TempDir()
	script := filepath.Join(directory, "fence.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(directory, "status.sh")
	if err := os.WriteFile(status, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]Command{
		"relative path":   {Path: "fence.sh", StatusPath: status, AllowedDirs: []string{directory}},
		"outside default": {Path: script, StatusPath: status},
		"status escapes":  {Path: script, StatusPath: "/bin/sh", AllowedDirs: []string{directory}},
	} {
		result, err := command.Fence(context.Background(), Request{WorkloadID: "w"})
		if AllowsAutomatic(result) || err == nil {
			t.Fatalf("%s must be refused, got %s %v", name, result, err)
		}
	}
}

// TestBlockedOutcomesWrapAutomaticFailoverBlocked pins the package's error
// contract: every outcome that does not positively confirm a fence must
// answer errors.Is(err, ErrAutomaticFailoverBlocked), including the timeout
// and refusal paths that used to return bare exec/context errors.
func TestBlockedOutcomesWrapAutomaticFailoverBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-specific")
	}
	directory := t.TempDir()
	refusing := filepath.Join(directory, "refuse.sh")
	if err := os.WriteFile(refusing, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	slow := filepath.Join(directory, "slow.sh")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(directory, "ok.sh")
	if err := os.WriteFile(ok, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, command := range map[string]Command{
		"refused":  {Path: refusing, StatusPath: ok, Timeout: time.Second, AllowedDirs: []string{directory}},
		"timeout":  {Path: slow, StatusPath: ok, Timeout: 50 * time.Millisecond, AllowedDirs: []string{directory}},
		"unlisted": {Path: refusing, StatusPath: ok, Timeout: time.Second},
	} {
		result, err := command.Fence(context.Background(), Request{WorkloadID: "w"})
		if AllowsAutomatic(result) {
			t.Fatalf("%s must not admit automatic failover, got %s", name, result)
		}
		if !errors.Is(err, ErrAutomaticFailoverBlocked) {
			t.Fatalf("%s error must wrap ErrAutomaticFailoverBlocked, got %v", name, err)
		}
	}
}

// TestEveryUnconfirmedResultFailsClosed pins the split-brain contract the
// standby restore depends on: nothing short of a positively confirmed fence
// may authorize an unattended restore, whatever result or error comes back.
func TestEveryUnconfirmedResultFailsClosed(t *testing.T) {
	for _, result := range []Result{ResultDisabled, ResultRefused, ResultTimedOut, ResultInconclusive, Result("fabricated")} {
		if AllowsAutomatic(result) {
			t.Fatalf("result %q must fail closed", result)
		}
	}
	for _, result := range []Result{ResultFenced, ResultAlreadyFenced} {
		if !AllowsAutomatic(result) {
			t.Fatalf("result %q is a positive fence confirmation", result)
		}
	}
}
