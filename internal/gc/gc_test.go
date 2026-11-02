package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/checkpoint"
	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

type gcFixture struct {
	root       string
	machine    *identity.Identity
	repository *checkpoint.Repository
	chunks     *chunkstore.Store
	collector  *Collector
}

func newGCFixture(t *testing.T) gcFixture {
	t.Helper()
	root := t.TempDir()
	machine, err := identity.Ensure(root + "/identity")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	repository, err := checkpoint.OpenRepository(root+"/checkpoints", keys)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(root+"/objects", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	collector, err := OpenOffline(root, repository, chunks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	return gcFixture{root: root, machine: machine, repository: repository, chunks: chunks, collector: collector}
}

func (f gcFixture) putAsset(t *testing.T, name string) model.AssetManifest {
	t.Helper()
	result, err := f.chunks.PutAsset(context.Background(), "workload", name, "application/octet-stream", bytes.NewReader([]byte(name)), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	return result.Asset
}

func (f gcFixture) saveManifest(t *testing.T, id string, assets ...model.AssetManifest) model.CheckpointManifest {
	t.Helper()
	manifest := model.CheckpointManifest{Format: model.StateFormatName, FormatVersion: model.StateFormatVersion, ID: id, Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(), Workload: model.WorkloadSpec{ID: "workload", Name: "workload", RootPath: f.root}, Assets: assets}
	if err := checkpoint.SignManifest(&manifest, f.machine); err != nil {
		t.Fatal(err)
	}
	if err := f.repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// seedOrphansForges persists first-seen observations as if an earlier
// maintenance run had already recorded them long ago.
func (f gcFixture) seedOrphans(t *testing.T, firstSeen time.Time, refs ...model.ChunkRef) {
	t.Helper()
	values := map[string]time.Time{}
	path := filepath.Join(f.root, "gc", "orphans.json")
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &values); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range refs {
		values[chunkKey(ref)] = firstSeen
	}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// backdateChunk rewrites a chunk file's mtime so it looks like old garbage.
// The plan must be (re)generated afterwards for the candidate's Modified
// field to match what DeleteRef verifies.
func (f gcFixture) backdateChunk(t *testing.T, ref model.ChunkRef, age time.Duration) {
	t.Helper()
	infos, err := f.chunks.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.Ref.Address == ref.Address && info.Ref.KeyVersion == ref.KeyVersion {
			when := time.Now().Add(-age)
			if err := os.Chtimes(info.Path, when, when); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("chunk %s not found on disk", ref.Address)
}

func (f gcFixture) chunkExists(t *testing.T, ref model.ChunkRef) bool {
	t.Helper()
	infos, err := f.chunks.Enumerate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.Ref.Address == ref.Address && info.Ref.KeyVersion == ref.KeyVersion {
			return true
		}
	}
	return false
}

func TestBuildPlanKeepsReferencedChunksAndSweepRechecksReferences(t *testing.T) {
	f := newGCFixture(t)
	kept := f.putAsset(t, "kept")
	orphan := f.putAsset(t, "orphan")
	f.saveManifest(t, "checkpoint-1", kept)
	// Age the orphan so the plan sees it as past the grace floor.
	f.backdateChunk(t, orphan.Chunks[0], 3*time.Hour)
	f.seedOrphans(t, time.Now().Add(-2*time.Hour), orphan.Chunks[0])
	plan, err := f.collector.Plan(context.Background(), 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Ref.Address != orphan.Chunks[0].Address {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Candidates[0].Status != "eligible" {
		t.Fatalf("candidate status = %q, want eligible", plan.Candidates[0].Status)
	}
	// Saving a new manifest after planning makes the candidate live. Sweep must
	// recheck and skip it instead of deleting a newly referenced chunk.
	f.saveManifest(t, "checkpoint-2", orphan)
	if _, err := f.collector.Sweep(context.Background(), plan.ID, false); err != nil {
		t.Fatal(err)
	}
	stored, err := f.collector.loadPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Candidates[0].Status != "skipped" {
		t.Fatalf("candidate status = %q, want skipped", stored.Candidates[0].Status)
	}
	if !f.chunkExists(t, orphan.Chunks[0]) {
		t.Fatal("a chunk that became referenced between plan and sweep must survive")
	}
}

// TestGCSweepHonorsGracePeriodBeforeDeleting is the two-phase grace
// regression: Plan labels a fresh orphan "waiting" with EligibleAt in the
// future, and Sweep must leave it alone — the old loop deleted every
// non-deleted/non-skipped candidate regardless of grace, so a single
// Plan+Sweep run destroyed chunks orphaned microseconds earlier, including
// chunks of a checkpoint whose manifest was still being written.
func TestGCSweepHonorsGracePeriodBeforeDeleting(t *testing.T) {
	f := newGCFixture(t)
	orphan := f.putAsset(t, "orphan")
	plan, err := f.collector.Plan(context.Background(), 2*time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Status != "waiting" {
		t.Fatalf("fresh orphan must be planned as waiting, got %#v", plan.Candidates)
	}
	if !plan.Candidates[0].EligibleAt.After(time.Now().UTC()) {
		t.Fatalf("waiting candidate must carry a future eligibility, got %v", plan.Candidates[0].EligibleAt)
	}
	if _, err := f.collector.Sweep(context.Background(), plan.ID, false); err != nil {
		t.Fatal(err)
	}
	stored, err := f.collector.loadPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Candidates[0].Status != "waiting" {
		t.Fatalf("sweep must not delete a grace-ineligible orphan, status = %q", stored.Candidates[0].Status)
	}
	if !f.chunkExists(t, orphan.Chunks[0]) {
		t.Fatal("BUG REGRESSED: sweep deleted a grace-ineligible (waiting) orphan")
	}
	// Only after the grace window has elapsed across a persisted observation
	// (and the content is past the mid-write floor) may deletion happen.
	f.backdateChunk(t, orphan.Chunks[0], 3*time.Hour)
	f.seedOrphans(t, time.Now().Add(-3*time.Hour), orphan.Chunks[0])
	plan2, err := f.collector.Plan(context.Background(), 2*time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.Candidates) != 1 || plan2.Candidates[0].Status != "eligible" {
		t.Fatalf("aged orphan must be planned as eligible, got %#v", plan2.Candidates)
	}
	if _, err := f.collector.Sweep(context.Background(), plan2.ID, false); err != nil {
		t.Fatal(err)
	}
	if f.chunkExists(t, orphan.Chunks[0]) {
		t.Fatal("an orphan past its grace period must be deleted")
	}
}

// TestSweepDoesNotDeleteChunksOfCheckpointBeingSavedMidWrite is the gcGuard
// race regression: chunk-write → manifest-install windows (policy
// checkpoints, migration stages, mirror imports) run in background writers
// that hold no write guard, so GC can observe their freshly written chunks as
// unreferenced while the capture is still running. Even with forged/stale
// eligibility bookkeeping the sweep must refuse content younger than the
// mid-write floor — the same floor clamps any caller-requested grace below
// one hour.
func TestSweepDoesNotDeleteChunksOfCheckpointBeingSavedMidWrite(t *testing.T) {
	f := newGCFixture(t)
	midWrite := f.putAsset(t, "mid-write")
	// The bookkeeping claims the chunk has been unreferenced for hours
	// (lost or forged orphans.json); the file itself is seconds old — it is
	// being written right now by a checkpoint whose manifest is not
	// installed yet.
	f.seedOrphans(t, time.Now().Add(-5*time.Hour), midWrite.Chunks[0])
	plan, err := f.collector.Plan(context.Background(), 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if plan.GracePeriod != minGrace {
		t.Fatalf("effective grace = %v, want the %v floor", plan.GracePeriod, minGrace)
	}
	if len(plan.Candidates) != 1 {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Candidates[0].Status != "eligible" {
		t.Fatalf("the forged bookkeeping must plan the candidate as eligible (only the content-age floor protects it), got %q", plan.Candidates[0].Status)
	}
	if _, err := f.collector.Sweep(context.Background(), plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if !f.chunkExists(t, midWrite.Chunks[0]) {
		t.Fatal("BUG REGRESSED: sweep deleted a chunk of a checkpoint being saved mid-write")
	}
	// Once the content itself is past the floor and the observation has
	// aged, deletion proceeds — the floor is an age gate, not a block.
	f.backdateChunk(t, midWrite.Chunks[0], 2*time.Hour)
	plan2, err := f.collector.Plan(context.Background(), 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan2.Candidates) != 1 || plan2.Candidates[0].Status != "eligible" {
		t.Fatalf("aged mid-write orphan must be eligible, got %#v", plan2.Candidates)
	}
	if _, err := f.collector.Sweep(context.Background(), plan2.ID, false); err != nil {
		t.Fatal(err)
	}
	if f.chunkExists(t, midWrite.Chunks[0]) {
		t.Fatal("content past the mid-write floor and the grace window must be reclaimed")
	}
}

func TestGCPlanEnforcesMinimumGraceFloor(t *testing.T) {
	f := newGCFixture(t)
	f.putAsset(t, "orphan")
	for _, requested := range []time.Duration{0, time.Second, time.Minute} {
		plan, err := f.collector.Plan(context.Background(), requested, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if plan.GracePeriod != minGrace {
			t.Fatalf("requested grace %v became %v, want floor %v", requested, plan.GracePeriod, minGrace)
		}
	}
}

// TestGCProtectsChunksOfUnindexedOnDiskManifests covers the Save/Delete
// crash windows: a manifest.enc.json on disk whose index entry is missing
// (crash between the manifest write and index.Put, or an interrupted Delete)
// still references chunks. The index alone would call them orphaned; the
// scan must treat them as referenced and surface the manifest instead.
func TestGCProtectsChunksOfUnindexedOnDiskManifests(t *testing.T) {
	f := newGCFixture(t)
	indexed := f.putAsset(t, "indexed")
	unindexed := f.putAsset(t, "unindexed")
	plain := f.putAsset(t, "plain")
	f.saveManifest(t, "checkpoint-indexed", indexed)
	saved := f.saveManifest(t, "checkpoint-crashed", unindexed)
	encoded, err := f.repository.ExportEncryptedManifest(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Replay the crash: drop the index entry and directory, then put the
	// manifest file back exactly where Save left it.
	if err := f.repository.Delete(saved.ID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, "checkpoints", saved.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.enc.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	f.backdateChunk(t, unindexed.Chunks[0], 3*time.Hour)
	f.backdateChunk(t, plain.Chunks[0], 3*time.Hour)
	f.seedOrphans(t, time.Now().Add(-2*time.Hour), plain.Chunks[0])
	plan, err := f.collector.Plan(context.Background(), 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range plan.Candidates {
		if candidate.Ref.Address == unindexed.Chunks[0].Address {
			t.Fatal("chunks of an unindexed on-disk manifest must not be planned as orphans")
		}
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Ref.Address != plain.Chunks[0].Address {
		t.Fatalf("only the genuinely unreferenced chunk may be planned, got %#v", plan.Candidates)
	}
	found := false
	for _, id := range plan.UnindexedManifests {
		if id == saved.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("unindexed manifest %s must be surfaced, got %v", saved.ID, plan.UnindexedManifests)
	}
	if _, err := f.collector.Sweep(context.Background(), plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if !f.chunkExists(t, unindexed.Chunks[0]) {
		t.Fatal("chunks of an unindexed on-disk manifest must survive the sweep")
	}
	if f.chunkExists(t, plain.Chunks[0]) {
		t.Fatal("a genuinely orphaned chunk must still be reclaimed")
	}
}

// TestGCFailsClosedOnUnreadableOnDiskManifest: an on-disk manifest that
// cannot be authenticated has an unknown chunk set; deleting anything while
// it exists risks destroying its recovery bytes, so planning fails closed.
func TestGCFailsClosedOnUnreadableOnDiskManifest(t *testing.T) {
	f := newGCFixture(t)
	orphan := f.putAsset(t, "orphan")
	f.backdateChunk(t, orphan.Chunks[0], 3*time.Hour)
	dir := filepath.Join(f.root, "checkpoints", "checkpoint-corrupt")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.enc.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.collector.Plan(context.Background(), 0, time.Now().UTC()); err == nil {
		t.Fatal("planning must fail closed when an on-disk manifest cannot be decoded")
	}
}

func TestGCSweepRejectsPlanIDTraversal(t *testing.T) {
	f := newGCFixture(t)
	for _, id := range []string{"", ".", "..", "../evil", "a/b", "gc/plans/../../objects", "/etc/passwd", ".hidden", "-leading"} {
		if _, err := f.collector.loadPlan(id); !errors.Is(err, ErrPlanNotFound) {
			t.Fatalf("loadPlan(%q) = %v, want ErrPlanNotFound", id, err)
		}
	}
	if err := f.collector.savePlan(Plan{ID: "../evil"}); err == nil {
		t.Fatal("savePlan must refuse a traversal plan id")
	}
	path := filepath.Join(f.root, "gc", "plans")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	// A plan file whose id field disagrees with its file name is refused.
	if err := os.WriteFile(filepath.Join(path, "mismatched.json"), []byte(`{"id":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.collector.loadPlan("mismatched"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("a plan file with a mismatched id must be refused, got %v", err)
	}
}

// TestSweepReclaimsStaleChunkTempFiles: crashed putChunk/writeJSON runs leak
// .chunk-* / .gc-* temp files that Enumerate never sees; the sweep reclaims
// the stale ones while leaving live staging alone.
func TestSweepReclaimsStaleChunkTempFiles(t *testing.T) {
	f := newGCFixture(t)
	chunkDir := filepath.Join(f.root, "objects", "chunks", "v1", "ab")
	if err := os.MkdirAll(chunkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(chunkDir, ".chunk-dead")
	live := filepath.Join(chunkDir, ".chunk-live")
	for _, path := range []string{stale, live} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	staleGC := filepath.Join(f.root, "gc", ".gc-dead")
	if err := os.WriteFile(staleGC, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staleGC, old, old); err != nil {
		t.Fatal(err)
	}
	plan, err := f.collector.Plan(context.Background(), 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.collector.Sweep(context.Background(), plan.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stale, staleGC} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale temp %s must be reclaimed", path)
		}
	}
	for _, path := range []string{live} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live staging temp %s must be kept: %v", path, err)
		}
	}
}

// TestGCConcurrentPlansPreserveOrphanObservations: overlapping maintenance
// runs (two POST /v1/gc handlers, or an API run racing the CLI) previously
// interleaved their load-modify-write cycles over gc/orphans.json and silently
// clobbered each other's first-seen observations. The per-state-directory
// exclusion must keep every observation.
func TestGCConcurrentPlansPreserveOrphanObservations(t *testing.T) {
	f := newGCFixture(t)
	var refs []model.ChunkRef
	for i := 0; i < 8; i++ {
		asset := f.putAsset(t, "orphan-"+string(rune('a'+i)))
		refs = append(refs, asset.Chunks[0])
	}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := f.collector.Plan(context.Background(), 0, time.Now().UTC()); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
	values, err := f.collector.loadOrphans()
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if _, ok := values[chunkKey(ref)]; !ok {
			t.Fatalf("first-seen observation of chunk %s was clobbered by a concurrent plan", ref.Address)
		}
	}
}
