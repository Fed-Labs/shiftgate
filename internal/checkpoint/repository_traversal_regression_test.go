package checkpoint

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

// traversalStubEngine is an engine that refuses to restore; the traversal
// fixtures never checkpoint or restore, they only exercise repository sinks.
type traversalStubEngine struct{}

func (traversalStubEngine) Check(context.Context) error                { return nil }
func (traversalStubEngine) Version(context.Context) (string, error)    { return "traversal-stub", nil }
func (traversalStubEngine) PreDump(context.Context, DumpOptions) error { return nil }
func (traversalStubEngine) Dump(context.Context, DumpOptions) error    { return nil }
func (traversalStubEngine) Restore(context.Context, RestoreOptions) (int, error) {
	return 0, errors.New("traversal stub engine")
}

// hostileCheckpointIDs are the shapes a peer-supplied manifest id must never
// take: every one of them is a path-traversal primitive once joined into a
// repository path.
var hostileCheckpointIDs = []string{
	"",
	".",
	"..",
	"...",
	"../..",
	"../escape",
	"../../var/lib/shift",
	"..\\..",
	"a/b",
	"a/../../b",
	"/etc",
	"/",
	"escape/../..",
	".hidden",
	" id",
	"id ",
	"id\x00evil",
	"index.enc.json",
	"trusted-sources.json",
	strings.Repeat("a", 129),
}

type repositoryFixture struct {
	stateRoot  string
	repository *Repository
	service    *Service
	identity   *identity.Identity
	chunks     *chunkstore.Store
}

func newRepositoryFixture(t *testing.T) repositoryFixture {
	t.Helper()
	stateRoot := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(filepath.Join(stateRoot, "runtime"), false, logger)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := identity.Ensure(filepath.Join(stateRoot, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(filepath.Join(stateRoot, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(filepath.Join(stateRoot, "objects"), 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(filepath.Join(stateRoot, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, traversalStubEngine{}, logger)
	return repositoryFixture{stateRoot: stateRoot, repository: repository, service: service, identity: machine, chunks: chunks}
}

func signedManifestWithID(t *testing.T, signer *identity.Identity, checkpointID, workloadID string) model.CheckpointManifest {
	t.Helper()
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: checkpointID, Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{ID: workloadID, Name: "traversal-probe", RootPath: "/srv/probe"},
	}
	if err := SignManifest(&manifest, signer); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestRegressionCheckpointIDTraversalRejected is the CRITICAL regression: a
// hostile checkpoint id must never reach a filesystem path. Save must refuse
// it before writing anything, Delete must refuse it before removing anything,
// and both must leave a planted victim directory outside the repository root
// completely untouched. This is the automatic-recursive-delete hole: an
// indexed id like ".." turned the retention pruner's repository.Delete into
// os.RemoveAll(root/..).
func TestRegressionCheckpointIDTraversalRejected(t *testing.T) {
	fixture := newRepositoryFixture(t)
	victim := filepath.Join(filepath.Dir(filepath.Join(fixture.stateRoot, "checkpoints")), "victim")
	if err := os.MkdirAll(filepath.Join(victim, "precious"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "precious", "keep.txt"), []byte("must survive"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, hostile := range hostileCheckpointIDs {
		manifest := signedManifestWithID(t, fixture.identity, hostile, "workload-1")
		if err := fixture.repository.Save(manifest); err == nil {
			t.Fatalf("BYPASS REGRESSED: repository.Save accepted the traversal id %q", hostile)
		}
		if err := fixture.repository.Delete(hostile); err == nil {
			t.Fatalf("BYPASS REGRESSED: repository.Delete accepted the traversal id %q", hostile)
		}
		if _, err := fixture.repository.Load(hostile); err == nil {
			t.Fatalf("BYPASS REGRESSED: repository.Load accepted the traversal id %q", hostile)
		}
		if _, err := fixture.repository.ExportEncryptedManifest(hostile); err == nil {
			t.Fatalf("BYPASS REGRESSED: repository.ExportEncryptedManifest accepted the traversal id %q", hostile)
		}
	}

	// Workload ids are validated at the same boundary.
	for _, hostile := range []string{"..", "a/b", "", "/etc"} {
		manifest := signedManifestWithID(t, fixture.identity, "checkpoint-ok", hostile)
		if err := fixture.repository.Save(manifest); err == nil {
			t.Fatalf("BYPASS REGRESSED: repository.Save accepted the traversal workload id %q", hostile)
		}
	}

	// The ingress paths that fed Save unvalidated must refuse too: Import is
	// the non-peer ingest door, VerifyManifest the signature check every
	// peer manifest passes through.
	hostile := signedManifestWithID(t, fixture.identity, "../escape", "workload-1")
	if err := VerifyManifest(hostile); err == nil {
		t.Fatal("BYPASS REGRESSED: VerifyManifest admitted a traversal checkpoint id")
	}
	if err := fixture.service.Import(hostile); err == nil {
		t.Fatal("BYPASS REGRESSED: Service.Import admitted a traversal checkpoint id")
	}

	// Nothing outside the repository root may have been touched.
	content, err := os.ReadFile(filepath.Join(victim, "precious", "keep.txt"))
	if err != nil || string(content) != "must survive" {
		t.Fatalf("the planted victim was disturbed: %q %v", content, err)
	}
	entries, err := os.ReadDir(filepath.Join(fixture.stateRoot, "checkpoints"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "..") {
			t.Fatalf("repository root holds a suspicious entry %q", entry.Name())
		}
	}
}

// TestRegressionRepositoryDeleteRemovesOnlyTheNamedCheckpoint covers the
// positive side of the same boundary: a well-formed id still deletes exactly
// its own directory, nothing else.
func TestRegressionRepositoryDeleteRemovesOnlyTheNamedCheckpoint(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := signedManifestWithID(t, fixture.identity, "checkpoint-keep-1", "workload-1")
	if err := fixture.repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	other := signedManifestWithID(t, fixture.identity, "checkpoint-keep-2", "workload-1")
	if err := fixture.repository.Save(other); err != nil {
		t.Fatal(err)
	}
	if err := fixture.repository.Delete(manifest.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.Load(manifest.ID); err == nil {
		t.Fatal("the deleted checkpoint must be gone")
	}
	if _, err := fixture.repository.Load(other.ID); err != nil {
		t.Fatalf("the surviving checkpoint must still load: %v", err)
	}
}

// TestRegressionCorruptOrIncompleteCheckpointRejected proves the repository
// refuses a corrupt checkpoint rather than handing it downstream: a tampered
// envelope, a truncated one, and a manifest whose index and contents disagree
// all fail closed at Load.
func TestRegressionCorruptOrIncompleteCheckpointRejected(t *testing.T) {
	fixture := newRepositoryFixture(t)
	manifest := signedManifestWithID(t, fixture.identity, "checkpoint-corrupt-1", "workload-1")
	if err := fixture.repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.stateRoot, "checkpoints", manifest.ID, "manifest.enc.json")

	corrupted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), corrupted...)

	corrupted[len(corrupted)/2] ^= 0xff
	if err := os.WriteFile(path, corrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.Load(manifest.ID); err == nil {
		t.Fatal("a tampered encrypted manifest must not load")
	}

	if err := os.WriteFile(path, original[:len(original)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.Load(manifest.ID); err == nil {
		t.Fatal("a truncated encrypted manifest must not load")
	}

	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.Load(manifest.ID); err == nil {
		t.Fatal("garbage in place of an encrypted manifest must not load")
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.repository.Load(manifest.ID); err != nil {
		t.Fatalf("the restored original must load again: %v", err)
	}
}

// TestRegressionProcessImageAssetsRejectsIncompleteChain proves the restore
// side refuses an incomplete process-image chain: a gapped pre-copy sequence
// can never be resolved by CRIU and must be rejected up front.
func TestRegressionProcessImageAssetsRejectsIncompleteChain(t *testing.T) {
	assets := func(names ...string) []model.AssetManifest {
		result := make([]model.AssetManifest, 0, len(names))
		for _, name := range names {
			result = append(result, model.AssetManifest{Name: name})
		}
		return result
	}
	if _, _, err := processImageAssets(model.CheckpointManifest{ID: "c", Assets: assets("filesystem-root")}); err == nil {
		t.Fatal("a checkpoint without a process-state asset must be refused")
	}
	if _, _, err := processImageAssets(model.CheckpointManifest{ID: "c", Assets: assets("process-state", "process-state-precopy-2")}); err == nil {
		t.Fatal("a gapped pre-copy chain must be refused")
	}
	if _, passes, err := processImageAssets(model.CheckpointManifest{ID: "c", Assets: assets("process-state", "process-state-precopy-1", "process-state-precopy-2")}); err != nil || len(passes) != 2 {
		t.Fatalf("a contiguous chain must pass: passes=%d err=%v", len(passes), err)
	}
}

// TestRegressionSaveRejectsReservedNames keeps checkpoint ids from colliding
// with the repository's own files.
func TestRegressionSaveRejectsReservedNames(t *testing.T) {
	fixture := newRepositoryFixture(t)
	for _, reserved := range []string{"index.enc.json", "trusted-sources.json"} {
		manifest := signedManifestWithID(t, fixture.identity, reserved, "workload-1")
		if err := fixture.repository.Save(manifest); err == nil {
			t.Fatalf("the reserved repository name %q must not be usable as a checkpoint id", reserved)
		}
	}
	if _, err := fixture.repository.Load("index.enc.json"); err == nil {
		t.Fatal("reserved names must be refused at Load too")
	}
}
