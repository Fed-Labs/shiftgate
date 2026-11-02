package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/config"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/transfer"
	"shift.dev/shift/internal/update"
)

// agentVersionScript is a real executable that answers --version, which is what
// the installer's self-check runs; it stands in for the agent binary without
// replacing the running test binary.
func agentVersionScript(version string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"shift-agent " + version + "\"; exit 0; fi\nexit 64\n")
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// newUpdateSigner generates a release-signing key in memory; nothing is written
// to disk and no key material is printed.
func newUpdateSigner(t *testing.T) *update.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal signing key: %v", err)
	}
	signer, err := update.NewSigner(string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatalf("load signing key: %v", err)
	}
	return signer
}

// publishRelease writes a signed release for a version-script artifact and
// returns the feed document path and the trusted key for it.
func publishRelease(t *testing.T, signer *update.Signer, directory, version string) (feedPath string, trusted update.TrustedKey) {
	t.Helper()
	payload := agentVersionScript(version)
	artifactPath := filepath.Join(directory, "agent-"+version)
	writeFile(t, artifactPath, payload, 0o755)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	release := update.Release{
		Version: version, Channel: update.ChannelStable, PublishedAt: time.Now().UTC(),
		ProtocolVersion: model.ProtocolVersion, MinimumProtocolVersion: model.MinimumProtocolVersion,
		ConfigVersion: model.ConfigSchemaVersion, StateVersion: model.StateSchemaVersion,
		Artifacts: []update.Artifact{{
			Component: update.ComponentAgent, OS: runtime.GOOS, Architecture: runtime.GOARCH,
			URL: "file://" + artifactPath, Format: update.FormatBinary,
			SizeBytes: int64(len(payload)), SHA256: digest, BinarySHA256: digest,
		}},
	}
	signed, err := signer.SignRelease(release)
	if err != nil {
		t.Fatalf("sign release: %v", err)
	}
	feedPath = filepath.Join(directory, "feed.json")
	encoded, err := json.Marshal(update.Feed{
		Version: 1, Channel: update.ChannelStable, GeneratedAt: time.Now().UTC(),
		Releases: []update.SignedRelease{signed},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, feedPath, encoded, 0o644)
	publicPEM, err := signer.PublicKeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	return feedPath, update.TrustedKey{ID: signer.KeyID(), PublicKeyPEM: publicPEM, Comment: "test signer"}
}

// updateHarness is an update-configured agent plus the exact files the feed
// and artifact live in, so tests can tamper with what the installer verifies.
type updateHarness struct {
	service    *Service
	executable string
	artifact   string
	feedPath   string
	trusted    update.TrustedKey
}

// newUpdateHarness opens an agent whose updates are configured against a file
// feed with one published release signed by publish, trusting exactly the
// given keys (nil means the publisher's own). The executable is a real script
// that answers --version with the running version.
func newUpdateHarness(t *testing.T, version string, publish *update.Signer, trust []update.TrustedKey) *updateHarness {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "agent-bin")
	writeFile(t, executable, agentVersionScript(version), 0o755)
	feedPath, trusted := publishRelease(t, publish, root, "9.9.9")
	if trust == nil {
		trust = []update.TrustedKey{trusted}
	}

	configuration := config.DefaultAgent()
	configuration.StateDir = filepath.Join(root, "state")
	configuration.Listen = "unix://" + filepath.Join(root, "agent.sock")
	configuration.InsecureDevelopment = true
	updates := config.DefaultUpdates()
	updates.Enabled = true
	updates.FeedFile = feedPath
	updates.TrustedKeys = trust
	updates.ExecutablePath = executable
	configuration.Updates = updates

	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open agent: %v", err)
	}
	if service.updates == nil {
		t.Fatal("the update manager was not built from configuration")
	}
	return &updateHarness{service: service, executable: executable, artifact: filepath.Join(root, "agent-9.9.9"), feedPath: feedPath, trusted: trusted}
}

// assertExecutableUnchanged pins that no install ran: the agent binary is
// byte-for-byte what the machine booted with.
func assertExecutableUnchanged(t *testing.T, harness *updateHarness, version string) {
	t.Helper()
	content, err := os.ReadFile(harness.executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, agentVersionScript(version)) {
		t.Fatal("the agent binary changed despite the install being refused")
	}
}

// openUpdateService opens an agent whose updates are configured against a file
// feed with one published release, and whose executable is a real script that
// answers --version with the running version.
func openUpdateService(t *testing.T, version string) (*Service, update.TrustedKey) {
	t.Helper()
	harness := newUpdateHarness(t, version, newUpdateSigner(t), nil)
	return harness.service, harness.trusted
}

func updateRequest(t *testing.T, service *Service, method, path string, body any, uid uint32) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request = request.WithContext(context.WithValue(request.Context(), credentialsContextKey{}, peerCredentials{UID: uid, GID: uint32(os.Getegid())}))
	response := httptest.NewRecorder()
	service.localHandler().ServeHTTP(response, request)
	return response
}

func TestUpdateStatusReportsTheInstallation(t *testing.T) {
	service, _ := openUpdateService(t, config.Version)
	response := updateRequest(t, service, http.MethodGet, "/v1/updates", nil, uint32(os.Geteuid()))
	if response.Code != http.StatusOK {
		t.Fatalf("status returned %d: %s", response.Code, response.Body.String())
	}
	var status update.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.CurrentVersion != config.Version || status.Channel != update.ChannelStable {
		t.Fatalf("unexpected installation: %+v", status)
	}
	if status.RestartRequired {
		t.Fatal("a fresh machine has nothing pending a restart")
	}
}

func TestUpdateRoutesExplainWhenUpdatesAreOff(t *testing.T) {
	configuration := config.DefaultAgent()
	configuration.StateDir = t.TempDir()
	configuration.Listen = "unix://" + filepath.Join(configuration.StateDir, "agent.sock")
	configuration.InsecureDevelopment = true
	service, err := Open(configuration, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// The status route is a GET; the mutation routes are POSTs.
	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/updates"},
		{http.MethodPost, "/v1/updates/check"},
		{http.MethodPost, "/v1/updates/apply"},
	}
	for _, route := range routes {
		response := updateRequest(t, service, route.method, route.path, nil, uint32(os.Geteuid()))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d: %s", route.path, response.Code, response.Body.String())
		}
		if !bytes.Contains(response.Body.Bytes(), []byte("UPDATES_DISABLED")) {
			t.Fatalf("%s did not explain itself: %s", route.path, response.Body.String())
		}
	}
}

func TestUpdateMutationsRequirePrivilege(t *testing.T) {
	service, _ := openUpdateService(t, config.Version)
	other := uint32(os.Geteuid() + 1)
	for _, path := range []string{"/v1/updates/check", "/v1/updates/apply", "/v1/updates/rollback", "/v1/updates/block", "/v1/updates/unblock"} {
		response := updateRequest(t, service, http.MethodPost, path, struct{}{}, other)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s returned %d for another local user: %s", path, response.Code, response.Body.String())
		}
	}
	// Reading the update state stays available to any authenticated local caller.
	response := updateRequest(t, service, http.MethodGet, "/v1/updates", nil, other)
	if response.Code != http.StatusOK {
		t.Fatalf("status returned %d for another local user: %s", response.Code, response.Body.String())
	}
}

func TestUpdateApplyInstallsAndRollsBack(t *testing.T) {
	service, _ := openUpdateService(t, config.Version)
	response := updateRequest(t, service, http.MethodPost, "/v1/updates/apply", map[string]string{"version": "9.9.9"}, uint32(os.Geteuid()))
	if response.Code != http.StatusOK {
		t.Fatalf("apply returned %d: %s", response.Code, response.Body.String())
	}
	var installed update.Installed
	if err := json.Unmarshal(response.Body.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	if installed.Version != "9.9.9" || installed.PreviousVersion != config.Version {
		t.Fatalf("unexpected install: %+v", installed)
	}

	statusResponse := updateRequest(t, service, http.MethodGet, "/v1/updates", nil, uint32(os.Geteuid()))
	var status update.Status
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.PendingVersion != "9.9.9" || !status.RestartRequired {
		t.Fatalf("the staged release must be pending a restart: %+v", status)
	}

	rollback := updateRequest(t, service, http.MethodPost, "/v1/updates/rollback", map[string]string{}, uint32(os.Geteuid()))
	if rollback.Code != http.StatusOK {
		t.Fatalf("rollback returned %d: %s", rollback.Code, rollback.Body.String())
	}
	var undone update.Installed
	if err := json.Unmarshal(rollback.Body.Bytes(), &undone); err != nil {
		t.Fatal(err)
	}
	if undone.Version != config.Version {
		t.Fatalf("rollback did not restore the previous binary: %+v", undone)
	}
}

func TestUpdateApplyRefusesAnUnsignedVersion(t *testing.T) {
	service, _ := openUpdateService(t, config.Version)
	response := updateRequest(t, service, http.MethodPost, "/v1/updates/apply", map[string]string{"version": "8.8.8"}, uint32(os.Geteuid()))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an unknown version must be refused, got %d: %s", response.Code, response.Body.String())
	}
}

func TestUpdateApplyDefersWhileAnIncomingTransferIsRunning(t *testing.T) {
	service, _ := openUpdateService(t, config.Version)
	if err := service.inFlightWork().busy(); err != nil {
		t.Fatalf("a fresh machine must be ready for updates: %v", err)
	}
	if _, err := service.sessions.Reserve(transfer.Session{
		ID: "session-update-1", SourceMachineID: "machine-remote", WorkloadID: "workload-remote",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.inFlightWork().busy(); err == nil {
		t.Fatal("an incoming transfer must stop an update")
	}
	response := updateRequest(t, service, http.MethodPost, "/v1/updates/apply", map[string]string{"version": "9.9.9"}, uint32(os.Geteuid()))
	if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte("UPDATE_DEFERRED")) {
		t.Fatalf("the apply must be deferred, got %d: %s", response.Code, response.Body.String())
	}
}

func TestUpdateGateBlocksEachKindOfInFlightWork(t *testing.T) {
	cases := []struct {
		name string
		work inFlightWork
		want string
	}{
		{
			name: "migration in transfer",
			work: inFlightWork{
				migrations: func() []model.Migration {
					return []model.Migration{{ID: "m1", WorkloadID: "w1", Stage: model.MigrationTransfer}}
				},
				restores: func() []checkpoint.RestoreRecord { return nil },
				forks:    func() []checkpoint.ForkRecord { return nil },
				sessions: func() []transfer.Session { return nil },
			},
			want: "migration m1",
		},
		{
			name: "restore preparing",
			work: inFlightWork{
				migrations: func() []model.Migration { return nil },
				restores: func() []checkpoint.RestoreRecord {
					return []checkpoint.RestoreRecord{{ID: "r1", WorkloadID: "w1", State: checkpoint.RestorePreparing}}
				},
				forks:    func() []checkpoint.ForkRecord { return nil },
				sessions: func() []transfer.Session { return nil },
			},
			want: "restore r1",
		},
		{
			name: "fork materializing",
			work: inFlightWork{
				migrations: func() []model.Migration { return nil },
				restores:   func() []checkpoint.RestoreRecord { return nil },
				forks: func() []checkpoint.ForkRecord {
					return []checkpoint.ForkRecord{{ID: "f1", SourceWorkloadID: "w1", State: checkpoint.ForkMaterialized}}
				},
				sessions: func() []transfer.Session { return nil },
			},
			want: "fork f1",
		},
		{
			name: "session reserved",
			work: inFlightWork{
				migrations: func() []model.Migration { return nil },
				restores:   func() []checkpoint.RestoreRecord { return nil },
				forks:      func() []checkpoint.ForkRecord { return nil },
				sessions: func() []transfer.Session {
					return []transfer.Session{{ID: "s1", WorkloadID: "w1", State: transfer.SessionReserved}}
				},
			},
			want: "incoming transfer s1",
		},
		{
			name: "held replication session is parked state, not work",
			work: inFlightWork{
				migrations: func() []model.Migration { return nil },
				restores:   func() []checkpoint.RestoreRecord { return nil },
				forks:      func() []checkpoint.ForkRecord { return nil },
				sessions: func() []transfer.Session {
					return []transfer.Session{{ID: "s1", WorkloadID: "w1", State: transfer.SessionHeld}}
				},
			},
			want: "",
		},
		{
			name: "terminal records are history",
			work: inFlightWork{
				migrations: func() []model.Migration {
					return []model.Migration{{ID: "m1", WorkloadID: "w1", Stage: model.MigrationCompleted}}
				},
				restores: func() []checkpoint.RestoreRecord {
					return []checkpoint.RestoreRecord{{ID: "r1", WorkloadID: "w1", State: checkpoint.RestoreCommitted}}
				},
				forks: func() []checkpoint.ForkRecord {
					return []checkpoint.ForkRecord{{ID: "f1", SourceWorkloadID: "w1", State: checkpoint.ForkFailed}}
				},
				sessions: func() []transfer.Session {
					return []transfer.Session{{ID: "s1", WorkloadID: "w1", State: transfer.SessionRolledBack}}
				},
			},
			want: "",
		},
	}
	for _, testCase := range cases {
		err := testCase.work.busy()
		if testCase.want == "" {
			if err != nil {
				t.Fatalf("%s: terminal records must not block, got %v", testCase.name, err)
			}
			continue
		}
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(testCase.want)) {
			t.Fatalf("%s: expected %q, got %v", testCase.name, testCase.want, err)
		}
	}
}

// TestUpdateGateAllowsInstallWhileAReplicationIsHeld: a held replication
// session is parked, complete, verified state — the duty record is the
// operational truth — and it sits in HELD for its full reservation TTL. If
// the gate treated it as in-flight, a warm standby receiving pushes more
// often than the TTL could never self-update at all. An active push still
// defers the install; the parked one does not.
func TestUpdateGateAllowsInstallWhileAReplicationIsHeld(t *testing.T) {
	service, _ := openUpdateService(t, config.Version)
	if _, err := service.sessions.Reserve(transfer.Session{
		ID: "session-held-1", SourceMachineID: "machine-remote", WorkloadID: "workload-remote",
		Purpose: transfer.PurposeReplication,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.inFlightWork().busy(); err == nil {
		t.Fatal("an incoming replication push in flight must defer an update")
	}
	if _, err := service.sessions.UpdateCleanup("session-held-1", "machine-remote", func(value *transfer.Session) error {
		value.State = transfer.SessionHeld
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.inFlightWork().busy(); err != nil {
		t.Fatalf("a held replication session must not defer updates: %v", err)
	}
	response := updateRequest(t, service, http.MethodPost, "/v1/updates/apply", map[string]string{"version": "9.9.9"}, uint32(os.Geteuid()))
	if response.Code != http.StatusOK {
		t.Fatalf("self-update must apply on a warm standby, got %d: %s", response.Code, response.Body.String())
	}
}

// TestUpdateApplyRefusesATamperedArtifact: the signed release promises one
// digest and the downloaded bytes must match it. A binary tampered with after
// signing is refused even when it still runs and reports the promised
// version — the signature's digest, not the binary's behavior, is the truth.
func TestUpdateApplyRefusesATamperedArtifact(t *testing.T) {
	harness := newUpdateHarness(t, config.Version, newUpdateSigner(t), nil)
	writeFile(t, harness.artifact, append(agentVersionScript("9.9.9"), []byte("\n# tampered after signing\n")...), 0o755)
	response := updateRequest(t, harness.service, http.MethodPost, "/v1/updates/apply", map[string]string{"version": "9.9.9"}, uint32(os.Geteuid()))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a tampered artifact must be refused, got %d: %s", response.Code, response.Body.String())
	}
	assertExecutableUnchanged(t, harness, config.Version)
}

// TestUpdateApplyRefusesAnUntrustedSignature: a release signed by a key the
// machine does not trust is never installed — the feed is untrusted input
// and only signatures the key ring holds vouch for a binary.
func TestUpdateApplyRefusesAnUntrustedSignature(t *testing.T) {
	publisher := newUpdateSigner(t)
	_, trustedFromOtherSigner := publishRelease(t, newUpdateSigner(t), t.TempDir(), "9.9.9")
	harness := newUpdateHarness(t, config.Version, publisher, []update.TrustedKey{trustedFromOtherSigner})
	response := updateRequest(t, harness.service, http.MethodPost, "/v1/updates/apply", map[string]string{"version": "9.9.9"}, uint32(os.Geteuid()))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an untrusted signature must be refused, got %d: %s", response.Code, response.Body.String())
	}
	assertExecutableUnchanged(t, harness, config.Version)
}
