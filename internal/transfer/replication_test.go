package transfer

// replication_test.go pins the standby half of the replication wire
// contract: a replication reserve ends in a held session that notifies the
// duty watcher, a migration session can never be held, hold and withdraw
// refuse to run without a watcher, and a withdrawal reaches the watcher with
// the authenticated source's identity.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	"shift.dev/shift/internal/securestore"
)

// recordingWatcher captures the duty callbacks a real standby supervisor
// would receive, so the tests can assert exactly what the server told it.
type recordingWatcher struct {
	mu        sync.Mutex
	held      []Session
	withdrawn []recordedWithdrawal
}

type recordedWithdrawal struct {
	source   string
	workload string
}

func (w *recordingWatcher) ReplicationHeld(session Session) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.held = append(w.held, session)
}

func (w *recordingWatcher) ReplicationWithdrawn(source, workload string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.withdrawn = append(w.withdrawn, recordedWithdrawal{source: source, workload: workload})
}

func (w *recordingWatcher) holds() []Session {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Session(nil), w.held...)
}

func (w *recordingWatcher) withdrawals() []recordedWithdrawal {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]recordedWithdrawal(nil), w.withdrawn...)
}

// replicationFixture is a destination-side peer server with a source-side
// checkpoint behind it, driven through a real HTTP client — the same wire
// dance a replicating agent speaks, so what is pinned is the contract.
type replicationFixture struct {
	watcher     *recordingWatcher
	server      *Server
	httpServer  *httptest.Server
	client      *Client
	repository  *checkpoint.Repository
	sessions    *Sessions
	manifest    model.CheckpointManifest
	chunks      *chunkstore.Store
	keys        *securestore.Manager
	machine     *identity.Identity
	keyVersion  uint32
	workloadKey []byte
}

func newReplicationFixture(t *testing.T) *replicationFixture {
	t.Helper()
	root := t.TempDir()
	sourceKeys, err := securestore.Open(filepath.Join(root, "source", "keys"))
	if err != nil {
		t.Fatal(err)
	}
	sourceChunks, err := chunkstore.Open(filepath.Join(root, "source", "objects"), 64<<10, sourceKeys)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("replicated checkpoint payload\n"), 200)
	asset, err := sourceChunks.PutAsset(context.Background(), "workload-1", "memory", "application/octet-stream", bytes.NewReader(plaintext), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	keyVersion := asset.Asset.Chunks[0].KeyVersion
	workloadKey, err := sourceKeys.ExportWorkloadKey("workload-1", keyVersion)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := identity.Ensure(filepath.Join(root, "source", "identity"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-repl", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload:       model.WorkloadSpec{ID: "workload-1", Name: "replicated", RootPath: "/srv/replicated"},
		Assets:         []model.AssetManifest{asset.Asset},
		Metrics:        model.CheckpointMetrics{PlainBytes: asset.Asset.PlainSize, StoredBytes: asset.Asset.StoredSize, ChunkCount: len(asset.Asset.Chunks)},
		Security:       model.SecurityEnvelope{KeyVersion: keyVersion},
		SourceIdentity: machine.Machine,
	}
	if err := checkpoint.SignManifest(&manifest, machine); err != nil {
		t.Fatal(err)
	}
	// Destination side: the sessions, chunk store, and manifest repository a
	// standby agent owns, plus the inventory its reserve check reads. The
	// restorer is never reached on the hold/withdraw paths and stays nil.
	destKeys, err := securestore.Open(filepath.Join(root, "destination", "keys"))
	if err != nil {
		t.Fatal(err)
	}
	destChunks, err := chunkstore.Open(filepath.Join(root, "destination", "objects"), 64<<10, destKeys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := checkpoint.OpenRepository(filepath.Join(root, "destination", "checkpoints"), destKeys)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := OpenSessions(filepath.Join(root, "destination"), destKeys)
	if err != nil {
		t.Fatal(err)
	}
	watcher := &recordingWatcher{}
	peer := NewServer(sessions, destKeys, destChunks, repository, nil, linuxplatform.NewInventory("standby-machine"),
		func(*http.Request) (string, error) { return machine.Machine.ID, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	peer.SetReplicationWatcher(watcher)
	httpServer := httptest.NewTLSServer(peer.Handler())
	t.Cleanup(httpServer.Close)
	return &replicationFixture{
		watcher: watcher, server: peer, httpServer: httpServer,
		client:     &Client{baseURL: httpServer.URL, http: httpServer.Client()},
		repository: repository, sessions: sessions,
		manifest: manifest, chunks: sourceChunks, keys: sourceKeys,
		machine: machine, keyVersion: keyVersion, workloadKey: workloadKey,
	}
}

// reserveKeyAndManifest walks a push through reserve, key import, manifest
// import, and chunk upload, leaving the session in MANIFEST_READY with every
// chunk on the destination.
func (f *replicationFixture) reserveKeyAndManifest(t *testing.T, purpose string) Session {
	t.Helper()
	ctx := context.Background()
	sessionID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.client.Reserve(ctx, ReserveRequest{
		ID: sessionID, SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
		EstimatedBytes: f.manifest.Metrics.StoredBytes,
		ExpiresAt:      time.Now().Add(30 * time.Minute),
		Purpose:        purpose,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.client.ImportKey(ctx, session.ID, "workload-1", f.keyVersion, f.workloadKey); err != nil {
		t.Fatalf("import key: %v", err)
	}
	missing, err := f.client.ImportManifest(ctx, session.ID, f.manifest)
	if err != nil {
		t.Fatalf("import manifest: %v", err)
	}
	for _, ref := range missing.Missing {
		if err := f.client.UploadChunk(ctx, session.ID, ref, f.chunks); err != nil {
			t.Fatalf("upload chunk %s: %v", ref.Address, err)
		}
	}
	return session
}

// pushToVerified continues the walk through verify, the last step before a
// migration would restore or a replication would hold.
func (f *replicationFixture) pushToVerified(t *testing.T, purpose string) Session {
	t.Helper()
	session := f.reserveKeyAndManifest(t, purpose)
	verified, err := f.client.Verify(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	return verified
}

// TestReplicationPushHoldsSessionAndNotifiesDuty is the whole contract in one
// walk: a replication push ends HELD with the duty facts on the session, the
// watcher is told, the checkpoint is loadable from the standby's own
// repository, and a retried hold returns the same session rather than error.
func TestReplicationPushHoldsSessionAndNotifiesDuty(t *testing.T) {
	f := newReplicationFixture(t)
	verified := f.pushToVerified(t, PurposeReplication)
	held, err := f.client.Hold(context.Background(), verified.ID, 3, "https://source.example:9443")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if held.State != SessionHeld || held.Purpose != PurposeReplication {
		t.Fatalf("session after hold: state %s purpose %s", held.State, held.Purpose)
	}
	if held.KeepLast != 3 || held.SourceAgentURL != "https://source.example:9443" {
		t.Fatalf("duty facts not carried on the held session: keep_last %d source %q", held.KeepLast, held.SourceAgentURL)
	}
	if held.CheckpointID != f.manifest.ID {
		t.Fatalf("held session names checkpoint %s, want %s", held.CheckpointID, f.manifest.ID)
	}
	notifications := f.watcher.holds()
	if len(notifications) != 1 {
		t.Fatalf("watcher notified %d times, want 1", len(notifications))
	}
	if notifications[0].CheckpointID != f.manifest.ID || notifications[0].WorkloadID != "workload-1" {
		t.Fatalf("watcher told about %+v", notifications[0])
	}
	// The standby restores from its own store, not from the session: the
	// held checkpoint must be loadable through the destination repository.
	if _, err := f.repository.Load(f.manifest.ID); err != nil {
		t.Fatalf("held checkpoint not loadable on the standby: %v", err)
	}
	// A hold retried after a lost response is idempotent — and re-notifies,
	// because the retry exists precisely because the source never saw the
	// first answer.
	again, err := f.client.Hold(context.Background(), verified.ID, 3, "https://source.example:9443")
	if err != nil {
		t.Fatalf("re-hold: %v", err)
	}
	if again.State != SessionHeld {
		t.Fatalf("re-hold returned state %s", again.State)
	}
	if notifications := f.watcher.holds(); len(notifications) != 2 {
		t.Fatalf("re-hold notified %d times, want 2", len(notifications))
	}
}

// TestHoldRefusesAMigrationSession pins the purpose split at the point it
// matters: a migration transfer — the only kind that may restore — can never
// be parked as a standby duty, and a replication transfer can never restore.
func TestHoldRefusesAMigrationSession(t *testing.T) {
	f := newReplicationFixture(t)
	verified := f.pushToVerified(t, "")
	if _, err := f.client.Hold(context.Background(), verified.ID, 1, "https://source.example:9443"); err == nil {
		t.Fatal("a migration session must not be holdable")
	} else if !strings.Contains(err.Error(), "only a replication transfer can be held") {
		t.Fatalf("hold refusal must name the purpose rule, got %q", err)
	}
	if holds := f.watcher.holds(); len(holds) != 0 {
		t.Fatalf("watcher was notified %d times for a refused hold", len(holds))
	}
}

// TestHoldRequiresAVerifiedSession: a session whose chunks have not been
// verified is not a duty — the standby must not record state it cannot
// vouch for.
func TestHoldRequiresAVerifiedSession(t *testing.T) {
	f := newReplicationFixture(t)
	session := f.reserveKeyAndManifest(t, PurposeReplication)
	_, err := f.client.Hold(context.Background(), session.ID, 1, "https://source.example:9443")
	if err == nil {
		t.Fatal("an unverified session must not be holdable")
	}
	if !strings.Contains(err.Error(), string(SessionManifestReady)) {
		t.Fatalf("refusal must name the session's actual state, got %q", err)
	}
}

// TestHoldValidatesTheDutyFacts: a negative retention and a non-https source
// URL are refused before any state changes.
func TestHoldValidatesTheDutyFacts(t *testing.T) {
	f := newReplicationFixture(t)
	verified := f.pushToVerified(t, PurposeReplication)
	for _, input := range []struct {
		keepLast int
		url      string
	}{{-1, "https://source.example:9443"}, {1, "http://source.example:9443"}, {1, "https://host/?query=1"}} {
		if _, err := f.client.Hold(context.Background(), verified.ID, input.keepLast, input.url); err == nil {
			t.Fatalf("hold with keep_last %d url %q must be refused", input.keepLast, input.url)
		} else if !strings.Contains(err.Error(), "HOLD_INVALID") {
			t.Fatalf("refusal must carry HOLD_INVALID, got %q", err)
		}
	}
	if _, err := f.sessions.Get(verified.ID, f.machine.Machine.ID); err != nil {
		t.Fatalf("session must survive refused holds: %v", err)
	}
}

// TestHoldAndWithdrawNeedAWatcher: without a duty recorder attached, hold and
// withdraw answer 503 rather than silently accepting a duty no one records.
func TestHoldAndWithdrawNeedAWatcher(t *testing.T) {
	f := newReplicationFixture(t)
	f.server.replication = nil
	verified := f.pushToVerified(t, PurposeReplication)
	if _, err := f.client.Hold(context.Background(), verified.ID, 1, "https://source.example:9443"); err == nil {
		t.Fatal("hold without a watcher must be refused")
	} else if !strings.Contains(err.Error(), "STANDBY_NOT_AVAILABLE") {
		t.Fatalf("refusal must carry STANDBY_NOT_AVAILABLE, got %q", err)
	}
	if err := f.client.Withdraw(context.Background(), "workload-1"); err == nil {
		t.Fatal("withdraw without a watcher must be refused")
	}
}

// TestReserveBindsPurpose: a session id reserved for replication can never be
// replayed as a migration (or the reverse), an unknown purpose is refused
// outright, and a reserve that names a source other than the authenticated
// machine is not the machine's session at all.
func TestReserveBindsPurpose(t *testing.T) {
	f := newReplicationFixture(t)
	ctx := context.Background()
	session, err := f.client.Reserve(ctx, ReserveRequest{
		ID: "purpose-bound", SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
		Purpose: PurposeReplication,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := f.client.Reserve(ctx, ReserveRequest{
		ID: "purpose-bound", SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
	}); err == nil || !strings.Contains(err.Error(), "bound to another purpose") {
		t.Fatalf("replay with another purpose must be refused, got %v", err)
	}
	if _, err := f.client.Reserve(ctx, ReserveRequest{
		ID: "bogus-purpose", SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
		Purpose: "backup",
	}); err == nil || !strings.Contains(err.Error(), "PURPOSE_INVALID") {
		t.Fatalf("unknown purpose must be refused with PURPOSE_INVALID, got %v", err)
	}
	if _, err := f.client.Reserve(ctx, ReserveRequest{
		ID: "wrong-source", SourceMachineID: "machine-imposter", WorkloadID: "workload-1",
		Purpose: PurposeReplication,
	}); err == nil || !strings.Contains(err.Error(), "SOURCE_IDENTITY_MISMATCH") {
		t.Fatalf("a reserve naming another source must be refused, got %v", err)
	}
	// The refused replays left the original session untouched.
	stored, err := f.sessions.Get(session.ID, f.machine.Machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Purpose != PurposeReplication || stored.State != SessionReserved {
		t.Fatalf("session disturbed by refused replays: %+v", stored)
	}
}

// TestWithdrawNotifiesTheWatcher: a withdrawal reaches the duty recorder with
// the authenticated source's identity — the watcher, not the request body,
// decides which duty that names — and an empty workload id is refused.
func TestWithdrawNotifiesTheWatcher(t *testing.T) {
	f := newReplicationFixture(t)
	verified := f.pushToVerified(t, PurposeReplication)
	if _, err := f.client.Hold(context.Background(), verified.ID, 2, ""); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := f.client.Withdraw(context.Background(), "workload-1"); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	withdrawals := f.watcher.withdrawals()
	if len(withdrawals) != 1 || withdrawals[0].source != f.machine.Machine.ID || withdrawals[0].workload != "workload-1" {
		t.Fatalf("watcher withdrawals: %+v", withdrawals)
	}
	if err := f.client.Withdraw(context.Background(), ""); err == nil {
		t.Fatal("an empty workload id must be refused")
	}
	if withdrawals := f.watcher.withdrawals(); len(withdrawals) != 1 {
		t.Fatalf("refused withdrawal must not notify, saw %d", len(withdrawals))
	}
}
