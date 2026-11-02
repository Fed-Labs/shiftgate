package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseDurationOrSecondsAcceptsBothForms(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"30":  30 * time.Second,
		"30s": 30 * time.Second,
		"15m": 15 * time.Minute,
		"2h":  2 * time.Hour,
		"0":   0,
	} {
		got, err := parseDurationOrSeconds(raw)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got != want {
			t.Fatalf("%q parsed as %s, want %s", raw, got, want)
		}
	}
	for _, raw := range []string{"", "abc", "-5", "30x"} {
		if _, err := parseDurationOrSeconds(raw); err == nil {
			t.Fatalf("%q was accepted as a duration", raw)
		}
	}
}

// TestParseCommandFlagsIsOrderInsensitive proves flags and positionals may
// interleave: `checkpoint create --timeout 60 demo` must not read "--timeout"
// as the workload id.
func TestParseCommandFlagsIsOrderInsensitive(t *testing.T) {
	flags := flagNew("checkpoint create")
	timeout := durationFlag(flags, "timeout", 1800*time.Second, "")
	flags.Bool("leave-running", true, "")
	for _, arguments := range [][]string{
		{"--timeout", "60", "demo"},
		{"demo", "--timeout", "60"},
		{"--timeout=60s", "demo"},
		{"demo", "--timeout", "1m"},
	} {
		*timeout = 1800 * time.Second
		positional, err := parseCommandFlags(flags, arguments)
		if err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
		if len(positional) != 1 || positional[0] != "demo" {
			t.Fatalf("%v: positionals = %v, want [demo]", arguments, positional)
		}
		if *timeout != time.Minute {
			t.Fatalf("%v: timeout = %s, want 1m", arguments, *timeout)
		}
	}
}

// TestParseCommandFlagsProtectsTheCommandTail proves everything after "--"
// stays positional even when it carries flags of its own.
func TestParseCommandFlagsProtectsTheCommandTail(t *testing.T) {
	flags := flagNew("workload create")
	root := flags.String("path", "", "")
	positional, err := parseCommandFlags(flags, []string{"demo", "--path", "/srv/demo", "--", "/bin/prog", "--json", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if *root != "/srv/demo" {
		t.Fatalf("path = %q", *root)
	}
	want := []string{"demo", "/bin/prog", "--json", "-v"}
	if strings.Join(positional, " ") != strings.Join(want, " ") {
		t.Fatalf("positionals = %v, want %v", positional, want)
	}
}

// TestExtractGlobalFlagsAnywhereInTheLine proves global flags are honored
// after the command word, and that a post-command --timeout stays with the
// command (its operation timeout) instead of being stolen globally.
func TestExtractGlobalFlagsAnywhereInTheLine(t *testing.T) {
	settings := options{timeout: 30 * time.Second}
	remaining, err := extractGlobalFlags([]string{"doctor", "--json", "--agent", "unix:///tmp/a.sock"}, &settings)
	if err != nil {
		t.Fatal(err)
	}
	if !settings.jsonOutput || settings.agentEndpoint != "unix:///tmp/a.sock" {
		t.Fatalf("globals not extracted: %+v", settings)
	}
	if strings.Join(remaining, " ") != "doctor" {
		t.Fatalf("remaining = %v", remaining)
	}

	settings = options{timeout: 30 * time.Second}
	remaining, err = extractGlobalFlags([]string{"--timeout", "30", "checkpoint", "create", "demo", "--timeout", "60"}, &settings)
	if err != nil {
		t.Fatal(err)
	}
	if settings.timeout != 30*time.Second {
		t.Fatalf("leading --timeout must set the request timeout, got %s", settings.timeout)
	}
	if strings.Join(remaining, " ") != "checkpoint create demo --timeout 60" {
		t.Fatalf("trailing --timeout must stay with the command, got %v", remaining)
	}
}

// TestGlobalFlagsAfterCommandAreHonored is the end-to-end shape the audit
// caught: `shiftgate version --json` used to print text and exit 0.
func TestGlobalFlagsAfterCommandAreHonored(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"version", "--json"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"version"`) {
		t.Fatalf("version --json must emit JSON, got %q", stdout.String())
	}
	stdout.Reset()
	if err := run([]string{"--json", "version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"version"`) {
		t.Fatalf("--json version must emit JSON, got %q", stdout.String())
	}
}

// TestHelpIsPolishedAndNeverAFatalFlagError proves `--help`/`-h`/`help` all
// print the product usage with exit 0, and `help COMMAND` prints that
// command's block.
func TestHelpIsPolishedAndNeverAFatalFlagError(t *testing.T) {
	for _, arguments := range [][]string{{"--help"}, {"-h"}, {"help"}} {
		var stdout, stderr bytes.Buffer
		if err := run(arguments, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", arguments, err)
		}
		if !strings.Contains(stdout.String(), "Usage: shiftgate") {
			t.Fatalf("%v: missing product usage:\n%s", arguments, stdout.String())
		}
		if strings.Contains(stdout.String(), "flag: help requested") {
			t.Fatalf("%v: leaked flag package output", arguments)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help", "workload"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "shiftgate workload SUBCOMMAND") {
		t.Fatalf("help workload printed the wrong block:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := run([]string{"workload", "--help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "shiftgate workload SUBCOMMAND") {
		t.Fatalf("workload --help printed the wrong block:\n%s", stdout.String())
	}
	stdout.Reset()
	if err := run([]string{"help", "definitely-not-a-command"}, &stdout, &stderr); err == nil {
		t.Fatal("help on an unknown topic must fail")
	}
}

// TestTrailingArgumentsAreRejected proves no command silently ignores junk.
func TestTrailingArgumentsAreRejected(t *testing.T) {
	for _, arguments := range [][]string{
		{"doctor", "these", "args"},
		{"version", "extra"},
		{"machines", "extra"},
		{"workloads", "extra"},
		{"doctor", "--timeout", "5"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(arguments, &stdout, &stderr)
		if err == nil {
			t.Fatalf("%v silently succeeded", arguments)
		}
		if !strings.Contains(err.Error(), "shiftgate") && !strings.Contains(err.Error(), "takes no arguments") && !strings.Contains(err.Error(), "--timeout") {
			t.Fatalf("%v: unhelpful error %q", arguments, err)
		}
	}
}

// TestMachinesRegisterNeverSilentlyNoops proves registration talks to the
// control plane (here: fails with the login remedy) instead of printing the
// local machine table and doing nothing.
func TestMachinesRegisterNeverSilentlyNoops(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"--control-url", "http://127.0.0.1:1", "--token-store", t.TempDir() + "/session.json",
		"machines", "register", "--machine-id", "x", "--name", "y", "--agent-url", "https://z:1"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("machines register without a login must fail loudly")
	}
	if !strings.Contains(err.Error(), "shiftgate login") {
		t.Fatalf("error must point at login, got %q", err)
	}
}

// TestDestructiveCommandsRequireYes proves delete refuses to run unattended
// without --yes instead of destroying state on a stray argument.
func TestDestructiveCommandsRequireYes(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = devnull.Close() }()
	saved := os.Stdin
	os.Stdin = devnull
	defer func() { os.Stdin = saved }()

	var stdout, stderr bytes.Buffer
	err = run([]string{"workload", "delete", "demo"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("workload delete without --yes must refuse on a non-terminal")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error must name --yes, got %q", err)
	}
}
