package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/model"
)

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var exit exitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected an exit-coded error, got %v", err)
	}
	return exit.code
}

// TestStrayFlagIsAUsageError: a stray flag after a command must exit 2
// (usage), not 1 (unexpected) — CI scripts distinguish "my bug" from
// "environment failure" by that digit.
func TestStrayFlagIsAUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"doctor", "--jsonx"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("a stray flag must fail")
	}
	if code := exitCodeOf(t, err); code != 2 {
		t.Fatalf("stray flag exited %d, want 2 (usage)", code)
	}
}

// TestUnknownGlobalFlagIsNotAnUnknownCommand: a mistyped global flag must say
// "unknown flag", never suggest a command for a flag token.
func TestUnknownGlobalFlagIsNotAnUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"--bogus", "doctor"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("an unknown global flag must fail")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("error must say unknown flag, got %q", err)
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("a flag token must not be reported as a command: %q", err)
	}
	if code := exitCodeOf(t, err); code != 2 {
		t.Fatalf("unknown global flag exited %d, want 2 (usage)", code)
	}
}

// TestNetworkPolicyIsValidated: --network accepts only preserve, reconnect,
// or drain; an arbitrary string must not be persisted as a policy.
func TestNetworkPolicyIsValidated(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"workload", "create", "demo", "--path", t.TempDir(), "--network", "bogus", "--", "/bin/true"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("--network bogus must be refused")
	}
	if !strings.Contains(err.Error(), "network policy") {
		t.Fatalf("error must name the network policy, got %q", err)
	}
	if code := exitCodeOf(t, err); code != 2 {
		t.Fatalf("--network validation exited %d, want 2 (usage)", code)
	}
}

// TestTypoGuardNeverBlocksRealNames: the fork/clone typo heuristic must not
// refuse an argument that could be a real workload or checkpoint name — it
// only decorates a genuine not-found error afterwards.
func TestTypoGuardNeverBlocksRealNames(t *testing.T) {
	if hint := subcommandTypo("best", forkSubcommands); hint == "" {
		t.Fatal("the heuristic still notices near-misses for the hint")
	}
	// "best" is edit-distance 2 from "list" but is a perfectly good
	// workload name: withSubcommandTypoHint must leave non-not-found errors
	// untouched so the operation, not the heuristic, decides.
	sentinel := errors.New("connection refused")
	if got := withSubcommandTypoHint(sentinel, "best", forkSubcommands); got != sentinel {
		t.Fatalf("non-not-found errors must pass through unchanged, got %v", got)
	}
	if got := withSubcommandTypoHint(nil, "best", forkSubcommands); got != nil {
		t.Fatalf("nil stays nil, got %v", got)
	}
}

// TestDecliningADestructivePromptIsUsageClass: saying N to the confirmation
// is the operator changing their mind — a usage-level outcome (exit 2), not
// an unexpected failure (exit 1).
func TestDecliningADestructivePromptIsUsageClass(t *testing.T) {
	declined := usageError("aborted")
	if code := exitCodeOf(t, declined); code != 2 {
		t.Fatalf("abort exited %d, want 2 (usage)", code)
	}
}

// startAgentStub serves one handler over a throwaway Unix-socket agent
// endpoint, the same shape the real agent listens on.
func startAgentStub(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "agent.sock")
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socketPath)
	if err != nil {
		t.Skipf("Unix socket integration is unavailable in this environment: %v", err)
	}
	server := &http.Server{Handler: handler}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		<-serveDone
	})
	return socketPath
}

// writeFakeSession stores a session file so control-plane commands pass the
// login gate without a server, for tests whose guards fire before any request.
func writeFakeSession(t *testing.T, controlURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli-session.json")
	content, err := json.Marshal(map[string]any{
		"control_plane_url": controlURL,
		"user_id":           "user-1",
		"email":             "operator@example.com",
		"access_token":      "shift_at_test",
		"refresh_token":     "shift_rt_test",
		"expires_at":        time.Now().Add(time.Hour).UTC(),
		"saved_at":          time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMigrateJSONExitCodesMatchHumanContract: `migrate --json --wait` must
// exit 5 for ROLLED_BACK/FAILED/CANCELLED and for COMPLETED carrying a
// failure code, and 0 only for a clean COMPLETED — the same digits human
// mode reports. CI scripts branch on that digit; a rolled-back move that
// exits 0 reads as success to `set -e`.
func TestMigrateJSONExitCodesMatchHumanContract(t *testing.T) {
	cases := []struct {
		name     string
		record   model.Migration
		jsonMode bool
		wantCode int
	}{
		{"json rolled-back", model.Migration{ID: "mig-1", Stage: model.MigrationRolledBack, FailureReason: "restore failed"}, true, 5},
		{"json failed", model.Migration{ID: "mig-1", Stage: model.MigrationFailed, FailureReason: "transfer failed"}, true, 5},
		{"json cancelled", model.Migration{ID: "mig-1", Stage: model.MigrationCancelled, FailureReason: "cancelled"}, true, 5},
		{"json completed with failure code", model.Migration{ID: "mig-1", Stage: model.MigrationCompleted, FailureCode: "RESTORE_FAILED", FailureReason: "restore failed"}, true, 5},
		{"json clean completed", model.Migration{ID: "mig-1", Stage: model.MigrationCompleted}, true, 0},
		{"human rolled-back", model.Migration{ID: "mig-1", Stage: model.MigrationRolledBack, FailureReason: "restore failed"}, false, 5},
		{"human completed with failure code", model.Migration{ID: "mig-1", Stage: model.MigrationCompleted, FailureCode: "RESTORE_FAILED", FailureReason: "restore failed"}, false, 5},
		{"human clean completed", model.Migration{ID: "mig-1", Stage: model.MigrationCompleted}, false, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := testCase.record
			socketPath := startAgentStub(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/migrations" && !strings.HasPrefix(request.URL.Path, "/v1/migrations/") {
					http.Error(writer, "unexpected request "+request.URL.Path, http.StatusBadRequest)
					return
				}
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(record)
			})
			arguments := []string{"--agent", "unix://" + socketPath, "--token-store", filepath.Join(t.TempDir(), "none.json")}
			if testCase.jsonMode {
				arguments = append(arguments, "--json")
			}
			arguments = append(arguments, "migrate", "demo", "--to", "https://127.0.0.1:1", "--wait")
			var stdout, stderr bytes.Buffer
			err := run(arguments, &stdout, &stderr)
			if testCase.wantCode == 0 {
				if err != nil {
					t.Fatalf("expected success, got %v (stderr: %s)", err, stderr.String())
				}
			} else {
				if err == nil {
					t.Fatalf("expected exit %d, got success", testCase.wantCode)
				}
				if code := exitCodeOf(t, err); code != testCase.wantCode {
					t.Fatalf("exit %d, want %d (err: %v)", code, testCase.wantCode, err)
				}
			}
			if testCase.jsonMode {
				var payload map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
					t.Fatalf("--json output is not one JSON document: %v\n%s", err, stdout.String())
				}
			}
		})
	}
}

// TestMachinesListWorksInLocalMode: `machines list` is the first form in the
// help block, so the local mode must accept it — identical to bare `machines`.
func TestMachinesListWorksInLocalMode(t *testing.T) {
	socketPath := startAgentStub(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/machine" {
			http.Error(writer, "unexpected request "+request.URL.Path, http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(model.MachineCapabilities{
			MachineID: "983da9077690dfb819724b0173ee754ea1a716dcb83f991412991dfc83c36380",
			Hostname:  "workstation", OS: "linux", Architecture: "amd64",
		})
	})
	for _, arguments := range [][]string{{"machines"}, {"machines", "list"}} {
		var stdout, stderr bytes.Buffer
		whole := append([]string{"--agent", "unix://" + socketPath}, arguments...)
		if err := run(whole, &stdout, &stderr); err != nil {
			t.Fatalf("%v failed: %v: %s", arguments, err, stderr.String())
		}
		if !strings.Contains(stdout.String(), "MACHINE 983da9077690dfb819724b0173ee754ea1a716dcb83f991412991dfc83c36380") {
			t.Fatalf("%v missing the machine identity:\n%s", arguments, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	err := run([]string{"--agent", "unix://" + socketPath, "machines", "list", "junk"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("machines list junk must not succeed")
	}
	if code := exitCodeOf(t, err); code != 2 {
		t.Fatalf("machines list junk exited %d, want 2 (usage)", code)
	}
}

// TestCheckpointListFiltersByWorkloadName: `checkpoint list --workload NAME`
// must find a workload's checkpoints by the name shown in the WORKLOAD
// column, not only by generated UUID — the docs and demos use names.
func TestCheckpointListFiltersByWorkloadName(t *testing.T) {
	summaries := []checkpoint.Summary{
		{ID: "ckpt-1", WorkloadID: "uuid-1", WorkloadName: "runner", Kind: model.CheckpointFull, CreatedAt: time.Now()},
		{ID: "ckpt-2", WorkloadID: "uuid-2", WorkloadName: "other", Kind: model.CheckpointFull, CreatedAt: time.Now()},
	}
	socketPath := startAgentStub(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/checkpoints" {
			http.Error(writer, "unexpected request "+request.URL.Path, http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(summaries)
	})
	cases := []struct {
		filter string
		want   []string
	}{
		{"runner", []string{"ckpt-1"}},
		{"uuid-2", []string{"ckpt-2"}},
		{"", []string{"ckpt-1", "ckpt-2"}},
	}
	for _, testCase := range cases {
		arguments := []string{"--agent", "unix://" + socketPath, "--json", "checkpoint", "list"}
		if testCase.filter != "" {
			arguments = append(arguments, "--workload", testCase.filter)
		}
		var stdout, stderr bytes.Buffer
		if err := run(arguments, &stdout, &stderr); err != nil {
			t.Fatalf("filter %q failed: %v: %s", testCase.filter, err, stderr.String())
		}
		var values []checkpoint.Summary
		if err := json.Unmarshal(stdout.Bytes(), &values); err != nil {
			t.Fatalf("filter %q: bad JSON: %v", testCase.filter, err)
		}
		got := make([]string, 0, len(values))
		for _, value := range values {
			got = append(got, value.ID)
		}
		if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
			t.Fatalf("filter %q returned %v, want %v", testCase.filter, got, testCase.want)
		}
	}
}

// TestDoctorJSONExitCodesMatchHumanContract: the exit-3 contract is not a
// human-renderer detail — `--json doctor` must fail the same digit when a
// required check fails, so CI branching on the exit code cannot pass a
// machine that cannot checkpoint or restore.
func TestDoctorJSONExitCodesMatchHumanContract(t *testing.T) {
	healthy := false
	socketPath := startAgentStub(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/doctor" {
			http.Error(writer, "unexpected request "+request.URL.Path, http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"healthy": healthy,
			"checks":  []map[string]any{{"name": "criu", "ok": healthy, "message": "criu probe"}},
		})
	})
	tokenStore := filepath.Join(t.TempDir(), "none.json")
	for _, testCase := range []struct {
		jsonMode bool
		healthy  bool
		wantCode int
	}{
		{true, false, 3},
		{true, true, 0},
		{false, false, 3},
		{false, true, 0},
	} {
		healthy = testCase.healthy
		arguments := []string{"--agent", "unix://" + socketPath, "--token-store", tokenStore}
		if testCase.jsonMode {
			arguments = append(arguments, "--json")
		}
		arguments = append(arguments, "doctor")
		var stdout, stderr bytes.Buffer
		err := run(arguments, &stdout, &stderr)
		if testCase.wantCode == 0 {
			if err != nil {
				t.Fatalf("healthy=%t --json=%t: unexpected failure %v", testCase.healthy, testCase.jsonMode, err)
			}
		} else {
			if err == nil {
				t.Fatalf("healthy=%t --json=%t: expected exit %d, got success", testCase.healthy, testCase.jsonMode, testCase.wantCode)
			}
			if code := exitCodeOf(t, err); code != testCase.wantCode {
				t.Fatalf("healthy=%t --json=%t: exit %d, want %d", testCase.healthy, testCase.jsonMode, code, testCase.wantCode)
			}
		}
	}
}

// TestTrialAndReferAreDiscoverable: the paywall and the post-move hint both
// name `shiftgate trial start` / `shiftgate refer`, so help, the usage block,
// and shell completion must all know those commands exist.
func TestTrialAndReferAreDiscoverable(t *testing.T) {
	for topic, want := range map[string]string{
		"trial":    "shiftgate trial start|status",
		"refer":    "shiftgate refer",
		"referral": "shiftgate refer",
	} {
		for _, arguments := range [][]string{{"help", topic}, {topic, "--help"}} {
			var stdout, stderr bytes.Buffer
			if err := run(arguments, &stdout, &stderr); err != nil {
				t.Fatalf("%v: %v", arguments, err)
			}
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("%v did not print %q:\n%s", arguments, want, stdout.String())
			}
		}
	}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		var stdout, stderr bytes.Buffer
		if err := run([]string{"completion", shell}, &stdout, &stderr); err != nil {
			t.Fatalf("completion %s: %v", shell, err)
		}
		for _, word := range []string{"trial", "refer"} {
			if !strings.Contains(stdout.String(), word) {
				t.Fatalf("completion %s omits %q:\n%s", shell, word, stdout.String())
			}
		}
	}
	var stdout, stderr bytes.Buffer
	if err := run([]string{"--help"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"trial start|status", "refer [--org ID]"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("global usage omits %q:\n%s", want, stdout.String())
		}
	}
}

// TestOptionalArgumentCommandsRejectStrayFlags: `status MIGRATION` and
// `failover [status]` take an optional argument, but a stray flag is still a
// usage error (exit 2) — never a migration-id lookup or a silent success.
func TestOptionalArgumentCommandsRejectStrayFlags(t *testing.T) {
	for _, arguments := range [][]string{
		{"status", "--jsonx"},
		{"failover", "--jsonx"},
		{"status", "one", "two"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(arguments, &stdout, &stderr)
		if err == nil {
			t.Fatalf("%v silently succeeded", arguments)
		}
		if code := exitCodeOf(t, err); code != 2 {
			t.Fatalf("%v exited %d, want 2 (usage)", arguments, code)
		}
	}
}

// TestStrayJunkIsNeverIgnored: a mistyped argument must fail as usage (exit
// 2), never run a different command than the operator asked for.
func TestStrayJunkIsNeverIgnored(t *testing.T) {
	tokenStore := writeFakeSession(t, "http://127.0.0.1:1")
	for _, arguments := range [][]string{
		{"login", "--email", "operator@example.com", "junk"},
		{"--control-url", "http://127.0.0.1:1", "fleet", "audit", "junk"},
		{"--control-url", "http://127.0.0.1:1", "fleet", "entitlement", "junk"},
		{"--control-url", "http://127.0.0.1:1", "marketplace", "offers", "junk"},
		{"--control-url", "http://127.0.0.1:1", "marketplace", "inventory", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "fleet", "--org", "org-1", "machines", "list", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "fleet", "--org", "org-1", "usage", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "fleet", "--org", "org-1", "migrations", "--jsonx"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "fleet", "--org", "org-1", "checkpoints", "--jsonx"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "place", "--workload", "w", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "publish", "--machine", "m", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "reserve", "--offer", "o", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "reservations", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "commit", "--reservation", "r", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "release", "--reservation", "r", "junk"},
		{"--control-url", "http://127.0.0.1:1", "--token-store", tokenStore, "marketplace", "--org", "org-1", "fail", "--reservation", "r", "--reason", "x", "junk"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(arguments, &stdout, &stderr)
		if err == nil {
			t.Fatalf("%v silently succeeded", arguments)
		}
		if code := exitCodeOf(t, err); code != 2 {
			t.Fatalf("%v exited %d, want 2 (usage): %v", arguments, code, err)
		}
	}
}

// TestDocumentedNumericBoundsAreUsageErrors: the caps docs/cli.md states
// (--mode cold|live, --count 1-128, --parallel 1-16, non-negative --cpu and
// --tail) are CLI contract — a violation is exit 2 before any request, not an
// API-error exit after one.
func TestDocumentedNumericBoundsAreUsageErrors(t *testing.T) {
	for _, arguments := range [][]string{
		{"clone", "ckpt-x", "--count", "500"},
		{"clone", "ckpt-x", "--count", "0"},
		{"clone", "ckpt-x", "--parallel", "99"},
		{"clone", "ckpt-x", "--parallel", "0"},
		{"migrate", "demo", "--to", "https://127.0.0.1:1", "--mode", "bogus"},
		{"migrate", "demo", "--to", "https://127.0.0.1:1", "--mode", "bogus", "--dry-run"},
		{"workload", "create", "demo", "--path", t.TempDir(), "--cpu", "-1", "--", "/bin/true"},
		{"workload", "logs", "demo", "--tail", "-5"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(arguments, &stdout, &stderr)
		if err == nil {
			t.Fatalf("%v silently succeeded", arguments)
		}
		if code := exitCodeOf(t, err); code != 2 {
			t.Fatalf("%v exited %d, want 2 (usage): %v", arguments, code, err)
		}
	}
}

// TestParseBytesRejectsFractionsAndOverflow: a resource size is a whole
// number of bytes inside int64 — `1.5B` and an overflowing product are typos
// to refuse, never garbage to forward as a workload limit.
func TestParseBytesRejectsFractionsAndOverflow(t *testing.T) {
	if _, err := parseBytes("1.5B"); err == nil {
		t.Fatal("fractional bytes must be refused")
	}
	if _, err := parseBytes("9999999TiB"); err == nil {
		t.Fatal("int64-overflowing sizes must be refused")
	}
	if value, err := parseBytes("1.5KiB"); err != nil || value != 1536 {
		t.Fatalf("1.5KiB = %d, %v; want 1536", value, err)
	}
}

// TestMultiOrgAccountsDefaultToTheFirstOrganization: docs/cli.md and the
// --org flag help promise the first organization as the default; a second
// organization must never turn every fleet command into a hard error.
func TestMultiOrgAccountsDefaultToTheFirstOrganization(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/organizations":
			_ = json.NewEncoder(writer).Encode([]map[string]any{
				{"id": "org-first", "name": "First", "role": "owner", "created_at": time.Now().UTC()},
				{"id": "org-second", "name": "Second", "role": "member", "created_at": time.Now().UTC()},
			})
		case "/v1/organizations/org-first/machines":
			_ = json.NewEncoder(writer).Encode([]map[string]any{{
				"id": "machine-record-1", "organization_id": "org-first", "machine_id": "workstation",
				"name": "Workstation", "agent_url": "https://workstation:8443", "capabilities": map[string]any{},
				"status": "online", "created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
			}})
		default:
			t.Errorf("unexpected request %s: a multi-org account must default to its first organization", request.URL.Path)
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()
	tokenStore := writeFakeSession(t, server.URL)

	var stdout, stderr bytes.Buffer
	err := run([]string{"--control-url", server.URL, "--token-store", tokenStore, "--json", "machines"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("fleet machines on a multi-org account failed: %v: %s", err, stderr.String())
	}
	var machines []struct {
		MachineID string `json:"machine_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &machines); err != nil {
		t.Fatalf("machines output was not JSON: %v: %s", err, stdout.String())
	}
	if len(machines) != 1 || machines[0].MachineID != "workstation" {
		t.Fatalf("unexpected machines from the default organization: %+v", machines)
	}
}

// TestCloneHelpDocumentsWaitReady: --wait-ready/--wait-timeout are shipped
// features; the help block must list them or they are undiscoverable.
func TestCloneHelpDocumentsWaitReady(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"help", "clone"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--wait-ready", "--wait-timeout"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help clone omits %s:\n%s", want, stdout.String())
		}
	}
}
