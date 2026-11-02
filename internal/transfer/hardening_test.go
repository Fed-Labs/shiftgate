package transfer

// hardening_test.go pins the peer-ingest hardening fixes: checkpoint IDs a
// peer supplies can never become filesystem paths, definitive server
// refusals are never masked as transport errors, concurrent uploads cannot
// overshoot a reservation, completion metering fires exactly once per
// transition, chunk verification concurrency is capped, and a dead transfer
// id is never re-reserved.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

func TestValidCheckpointIDRefusesPathShapes(t *testing.T) {
	for _, id := range []string{"", "..", "../escape", "a/b", "/etc/keys", "x/../y", ".", "a\\b", "cp\x00id", strings.Repeat("a", 65)} {
		if validCheckpointID(id) {
			t.Fatalf("checkpoint id %q must be refused", id)
		}
	}
	for _, id := range []string{"checkpoint-repl", "8b7a2f1e-4c3d-4e5f-8a9b-0c1d2e3f4a5b", "cp_0001", "A"} {
		if !validCheckpointID(id) {
			t.Fatalf("checkpoint id %q must be accepted", id)
		}
	}
}

// TestImportManifestRefusesUnsafeCheckpointIDs: a peer-supplied manifest ID
// is joined into repository paths by the agent running as root. An ID with
// separators or dot segments must be refused at the ingest boundary — before
// any save — so no peer can choose a path to write or later delete.
func TestImportManifestRefusesUnsafeCheckpointIDs(t *testing.T) {
	f := newReplicationFixture(t)
	ctx := context.Background()
	sessionID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.client.Reserve(ctx, ReserveRequest{
		ID: sessionID, SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
		EstimatedBytes: f.manifest.Metrics.StoredBytes, ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.client.ImportKey(ctx, session.ID, "workload-1", f.keyVersion, f.workloadKey); err != nil {
		t.Fatalf("import key: %v", err)
	}
	for _, id := range []string{"../escape", "..", "a/b", "/etc/keys", ".hidden/../../x"} {
		evil := f.manifest
		evil.ID = id
		if err := checkpoint.SignManifest(&evil, f.machine); err != nil {
			t.Fatal(err)
		}
		if _, err := f.client.ImportManifest(ctx, session.ID, evil); err == nil || !strings.Contains(err.Error(), "safe identifier") {
			t.Fatalf("manifest id %q must be refused at import, got %v", id, err)
		}
		if _, err := f.repository.Load(id); err == nil {
			t.Fatalf("manifest id %q must not have been written anywhere", id)
		}
	}
}

// TestUploadChunkSurfacesServerRefusalBeforeExportErrors: when the peer
// refuses the chunk before reading its body (quota, authorization, storage
// floor, state), the transport closes the export pipe. The caller must see
// the server's structured refusal, not "closed pipe" — otherwise terminal
// and transient failures become indistinguishable.
func TestUploadChunkSurfacesServerRefusalBeforeExportErrors(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := chunkstore.Open(root+"/objects", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	// Larger than net/http's 256 KiB unread-body drain limit, so the early
	// response really does abort the body stream mid-export.
	content := make([]byte, 300<<10)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	result, err := store.PutAsset(context.Background(), "w1", "asset", "application/octet-stream", bytes.NewReader(content), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInsufficientStorage)
		_, _ = writer.Write([]byte(`{"code":"TRANSFER_QUOTA_EXCEEDED","message":"incoming transfer exceeded its reserved size"}`))
	}))
	t.Cleanup(server.Close)
	client := &Client{baseURL: server.URL, http: server.Client()}
	err = client.UploadChunk(context.Background(), "session-1", result.Asset.Chunks[0], store)
	if err == nil {
		t.Fatal("a refused upload must return an error")
	}
	if !strings.Contains(err.Error(), "TRANSFER_QUOTA_EXCEEDED") {
		t.Fatalf("the server's refusal must reach the caller, got %q", err)
	}
}

// TestConcurrentChunkUploadsDoNotOvershootTheReservation: the receive-byte
// quota must be reserved in the same serialized gate that checks it, so
// parallel uploads can never all pass the check and all land.
func TestConcurrentChunkUploadsDoNotOvershootTheReservation(t *testing.T) {
	f := newReplicationFixture(t)
	ctx := context.Background()
	bulk := make([]byte, 400<<10)
	if _, err := rand.Read(bulk); err != nil {
		t.Fatal(err)
	}
	extra, err := f.chunks.PutAsset(ctx, "workload-1", "bulk", "application/octet-stream", bytes.NewReader(bulk), true, 2)
	if err != nil {
		t.Fatal(err)
	}
	manifest := f.manifest
	manifest.Assets = append(append([]model.AssetManifest{}, f.manifest.Assets...), extra.Asset)
	if err := checkpoint.SignManifest(&manifest, f.machine); err != nil {
		t.Fatal(err)
	}
	sessionID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	// Room for about two chunks; more will be refused as the quota fills.
	var total int64
	for _, ref := range extra.Asset.Chunks {
		total += ref.StoredSize
	}
	session, err := f.client.Reserve(ctx, ReserveRequest{
		ID: sessionID, SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
		EstimatedBytes: total / 2, ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.client.ImportKey(ctx, session.ID, "workload-1", f.keyVersion, f.workloadKey); err != nil {
		t.Fatalf("import key: %v", err)
	}
	missing, err := f.client.ImportManifest(ctx, session.ID, manifest)
	if err != nil {
		t.Fatalf("import manifest: %v", err)
	}
	var group sync.WaitGroup
	start := make(chan struct{})
	for _, ref := range missing.Missing {
		group.Add(1)
		go func(ref model.ChunkRef) {
			defer group.Done()
			<-start
			_ = f.client.UploadChunk(ctx, session.ID, ref, f.chunks)
		}(ref)
	}
	close(start)
	group.Wait()
	final, err := f.sessions.Get(session.ID, f.machine.Machine.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.ReceivedBytes > quotaLimit(final.EstimatedBytes) {
		t.Fatalf("concurrent uploads overshot the reservation: %d > %d", final.ReceivedBytes, quotaLimit(final.EstimatedBytes))
	}
	if final.ReceivedBytes <= 0 {
		t.Fatal("no chunk landed; the test proves nothing")
	}
}

type countingMeter struct {
	mu    sync.Mutex
	calls int
	bytes int64
}

func (m *countingMeter) RecordTransferBytes(_ context.Context, _, _ string, bytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.bytes += bytes
	return nil
}

func (m *countingMeter) counts() (int, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.bytes
}

// TestConcurrentHoldsMeterCompletionOnce: completion metering must fire
// exactly once per session — the request that actually performs the
// terminal transition — no matter how many duplicate holds race.
func TestConcurrentHoldsMeterCompletionOnce(t *testing.T) {
	f := newReplicationFixture(t)
	meter := &countingMeter{}
	f.server.SetTransferMeter("org-1", meter)
	verified := f.pushToVerified(t, PurposeReplication)
	var group sync.WaitGroup
	start := make(chan struct{})
	for index := 0; index < 4; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, _ = f.client.Hold(context.Background(), verified.ID, 3, "https://source.example:9443")
		}()
	}
	close(start)
	group.Wait()
	if _, err := f.client.Hold(context.Background(), verified.ID, 3, "https://source.example:9443"); err != nil {
		t.Fatalf("re-hold: %v", err)
	}
	calls, _ := meter.counts()
	if calls != 1 {
		t.Fatalf("completion metered %d times, want exactly 1", calls)
	}
}

// TestChunkIngestRefusesWhenVerificationSlotsAreHeld: chunk verification is
// capped; a PUT arriving while every slot is held and whose request dies
// while waiting is answered with a clear refusal instead of verifying an
// unbounded number of full chunks at once.
func TestChunkIngestRefusesWhenVerificationSlotsAreHeld(t *testing.T) {
	f := newReplicationFixture(t)
	ctx := context.Background()
	sessionID, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	session, err := f.client.Reserve(ctx, ReserveRequest{
		ID: sessionID, SourceMachineID: f.machine.Machine.ID, WorkloadID: "workload-1",
		EstimatedBytes: f.manifest.Metrics.StoredBytes, ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := f.client.ImportKey(ctx, session.ID, "workload-1", f.keyVersion, f.workloadKey); err != nil {
		t.Fatalf("import key: %v", err)
	}
	for index := 0; index < maxConcurrentChunkImports; index++ {
		f.server.importSlots <- struct{}{}
	}
	ref := f.manifest.Assets[0].Chunks[0]
	reference, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "/v1/peer/transfers/" + session.ID + "/chunks/" + strconv.FormatUint(uint64(ref.KeyVersion), 10) + "/" + ref.Address
	request := httptest.NewRequest(http.MethodPut, endpoint, bytes.NewReader(nil))
	request.Header.Set("X-Shift-Chunk-Ref", base64.RawURLEncoding.EncodeToString(reference))
	waiting, cancel := context.WithCancel(context.Background())
	cancel()
	request = request.WithContext(waiting)
	recorder := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "CHUNK_IMPORT_BUSY") {
		t.Fatalf("slot-starved ingest must answer CHUNK_IMPORT_BUSY, got %d %s", recorder.Code, recorder.Body.String())
	}
	for index := 0; index < maxConcurrentChunkImports; index++ {
		<-f.server.importSlots
	}
}

// TestReserveRefusesExpiredAndTerminalIDs: reusing a transfer id whose
// session is dead must be a clear refusal, not a 201 carrying a session that
// can never ingest. A live session keeps its idempotent-retry behavior.
func TestReserveRefusesExpiredAndTerminalIDs(t *testing.T) {
	sessions := newSessionStore(t)
	source := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	live, err := sessions.Reserve(Session{ID: "live", SourceMachineID: source, WorkloadID: "workload-1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := sessions.Reserve(Session{ID: "live", SourceMachineID: source, WorkloadID: "workload-1"})
	if err != nil || again.State != SessionReserved {
		t.Fatalf("a live session must stay idempotent on re-reserve: %v %+v", err, again)
	}
	if _, err := sessions.UpdateCleanup(live.ID, source, func(value *Session) error {
		value.State = SessionFailed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Reserve(Session{ID: "live", SourceMachineID: source, WorkloadID: "workload-1"}); err == nil {
		t.Fatal("a failed session's id must not be re-reservable")
	}
	if _, err := sessions.Reserve(Session{
		ID: "dead", SourceMachineID: source, WorkloadID: "workload-1", ExpiresAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	sessions.ReapExpired()
	if _, err := sessions.Reserve(Session{ID: "dead", SourceMachineID: source, WorkloadID: "workload-1"}); err == nil {
		t.Fatal("an expired session's id must not be re-reservable")
	}
}

var _ TransferMeter = (*countingMeter)(nil)
