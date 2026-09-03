package checkpoint

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/objectstore"
	"shift.dev/shift/internal/securestore"
)

func TestMirrorEncryptedCheckpointRoundTripAndRetry(t *testing.T) {
	fixture := newMirrorFixture(t)
	ctx := context.Background()

	first, err := fixture.sourceMirror.Upload(ctx, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	wantObjects := len(uniqueChunkRefs(fixture.manifest.Assets)) + 1
	if first.Objects != wantObjects {
		t.Fatalf("uploaded %d objects, want %d", first.Objects, wantObjects)
	}
	second, err := fixture.sourceMirror.Upload(ctx, fixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent retry changed result: first=%+v second=%+v", first, second)
	}

	for _, ref := range uniqueChunkRefs(fixture.manifest.Assets) {
		var remote bytes.Buffer
		if _, err := fixture.remote.Get(ctx, chunkObjectKey(ref), &remote); err != nil {
			t.Fatal(err)
		}
		var local bytes.Buffer
		if err := fixture.sourceChunks.ExportChunk(ref, &local); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(remote.Bytes(), local.Bytes()) {
			t.Fatal("mirrored chunk differs from encrypted local ciphertext")
		}
		if bytes.Contains(remote.Bytes(), fixture.plaintext[:64]) {
			t.Fatal("mirrored chunk exposed checkpoint plaintext")
		}
	}
	var remoteManifest bytes.Buffer
	if _, err := fixture.remote.Get(ctx, checkpointManifestKey(fixture.manifest.ID), &remoteManifest); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(remoteManifest.Bytes(), []byte("private-workload-secret")) || bytes.Contains(remoteManifest.Bytes(), []byte("sensitive-workload-name")) {
		t.Fatal("mirrored manifest exposed plaintext workload metadata")
	}
	if _, err := os.Stat(filepath.Join(fixture.remoteRoot, "keys")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object store contains an unexpected key directory: %v", err)
	}

	destinationMirror, destinationChunks, destinationRepository := fixture.destination(t)
	downloaded, err := destinationMirror.Download(ctx, fixture.manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if downloaded.ManifestSHA256 != fixture.manifest.ManifestSHA256 {
		t.Fatal("downloaded manifest changed")
	}
	if persisted, err := destinationRepository.Load(fixture.manifest.ID); err != nil || persisted.ManifestSHA256 != fixture.manifest.ManifestSHA256 {
		t.Fatalf("downloaded manifest was not persisted: manifest=%+v error=%v", persisted, err)
	}
	var restored bytes.Buffer
	if err := destinationChunks.RestoreAsset(ctx, fixture.manifest.Workload.ID, fixture.manifest.Assets[0], &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), fixture.plaintext) {
		t.Fatal("downloaded checkpoint plaintext did not round trip")
	}
	if _, err := destinationMirror.Download(ctx, fixture.manifest.ID); err != nil {
		t.Fatalf("idempotent download failed: %v", err)
	}
}

func TestMirrorDownloadFailsForMissingOrTamperedObject(t *testing.T) {
	fixture := newMirrorFixture(t)
	ctx := context.Background()
	if _, err := fixture.sourceMirror.Upload(ctx, fixture.manifest); err != nil {
		t.Fatal(err)
	}
	ref := uniqueChunkRefs(fixture.manifest.Assets)[0]
	key := chunkObjectKey(ref)
	if err := fixture.remote.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	destinationMirror, _, _ := fixture.destination(t)
	if _, err := destinationMirror.Download(ctx, fixture.manifest.ID); err == nil {
		t.Fatal("download succeeded with a missing chunk")
	}
	if _, err := fixture.sourceMirror.Upload(ctx, fixture.manifest); err != nil {
		t.Fatalf("retry did not restore missing object: %v", err)
	}

	objectPath := filepath.Join(fixture.remoteRoot, "objects", filepath.FromSlash(key))
	encoded, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if err := os.WriteFile(objectPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	destinationMirror, _, _ = fixture.destination(t)
	if _, err := destinationMirror.Download(ctx, fixture.manifest.ID); err == nil {
		t.Fatal("download succeeded with a tampered chunk")
	}
}

func TestServiceMirrorCheckpointRetriesPersistedCheckpoint(t *testing.T) {
	fixture := newMirrorFixture(t)
	service := &Service{repository: fixture.sourceRepo, mirror: fixture.sourceMirror}

	result, err := service.MirrorCheckpoint(context.Background(), fixture.manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.CheckpointID != fixture.manifest.ID || result.Objects != len(uniqueChunkRefs(fixture.manifest.Assets))+1 {
		t.Fatalf("unexpected mirror retry result: %+v", result)
	}
	if _, err := fixture.remote.Head(context.Background(), checkpointManifestKey(fixture.manifest.ID)); err != nil {
		t.Fatal(err)
	}

	disabled := &Service{repository: fixture.sourceRepo}
	if _, err := disabled.MirrorCheckpoint(context.Background(), fixture.manifest.ID); err == nil {
		t.Fatal("mirror retry succeeded while object-store mirroring was disabled")
	}
}

func TestMirrorResumesLargeMultipartChunkAfterPartFailure(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(filepath.Join(root, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(filepath.Join(root, "objects"), 64<<20, keys)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 9<<20)
	if _, err := cryptorand.Read(payload); err != nil {
		t.Fatal(err)
	}
	asset, err := chunks.PutAsset(context.Background(), "large-workload", "memory", "application/octet-stream", bytes.NewReader(payload), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(asset.Asset.Chunks) != 1 || asset.Asset.Chunks[0].StoredSize < multipartThresholdBytes {
		t.Fatalf("fixture did not produce a multipart chunk: %+v", asset.Asset.Chunks)
	}
	machine, err := identity.Ensure(filepath.Join(root, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "large-checkpoint", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{ID: "large-workload", Name: "large", RootPath: "/srv/large"},
		Assets:   []model.AssetManifest{asset.Asset},
		Metrics:  model.CheckpointMetrics{PlainBytes: asset.Asset.PlainSize, StoredBytes: asset.Asset.StoredSize, ChunkCount: len(asset.Asset.Chunks)},
		Security: model.SecurityEnvelope{KeyVersion: asset.Asset.Chunks[0].KeyVersion},
	}
	if err := SignManifest(&manifest, machine); err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(filepath.Join(root, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	remote, err := objectstore.OpenLocal(filepath.Join(root, "remote"))
	if err != nil {
		t.Fatal(err)
	}
	flaky := &failAfterPartStore{Store: remote, fail: true}
	mirror := NewMirror(flaky, chunks, repository)
	if _, err := mirror.Upload(context.Background(), manifest); err == nil {
		t.Fatal("first multipart upload unexpectedly succeeded")
	}
	partial, err := remote.ListMultipart(context.Background(), "chunks/")
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 1 {
		t.Fatalf("expected one resumable upload, got %d", len(partial))
	}
	result, err := mirror.Upload(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if result.Objects != 2 || result.Bytes != manifest.Metrics.StoredBytes+int64(len(mustExportManifest(t, repository, manifest.ID))) {
		t.Fatalf("unexpected resumed mirror result: %+v", result)
	}
	if remaining, err := remote.ListMultipart(context.Background(), "chunks/"); err != nil || len(remaining) != 0 {
		t.Fatalf("multipart upload was not completed: remaining=%d error=%v", len(remaining), err)
	}
	ref := asset.Asset.Chunks[0]
	if info, err := remote.Head(context.Background(), chunkObjectKey(ref)); err != nil {
		t.Fatal(err)
	} else if info.Size != ref.StoredSize || info.SHA256 != ref.CipherSHA256 {
		t.Fatalf("resumed chunk metadata mismatch: %+v", info)
	}
}

func TestMirrorCleansStaleChunkMultipartUploads(t *testing.T) {
	fixture := newMirrorFixture(t)
	upload, err := fixture.remote.InitiateMultipart(context.Background(), "chunks/v1/stale", 0, sha256Bytes(nil))
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(fixture.remoteRoot, "multipart", upload.UploadID, "meta.json")
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	metadata = bytes.Replace(metadata, []byte(upload.InitiatedAt.Format(time.RFC3339Nano)), []byte(old), 1)
	if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := fixture.sourceMirror.CleanupUploads(context.Background(), time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d stale uploads, want 1", removed)
	}
	if _, err := fixture.remote.ListParts(context.Background(), upload.UploadID); !errors.Is(err, objectstore.ErrNotFound) {
		t.Fatalf("stale chunk upload remained: %v", err)
	}
}

type failAfterPartStore struct {
	objectstore.Store
	fail bool
}

func (store *failAfterPartStore) UploadPart(ctx context.Context, uploadID string, number int, reader io.Reader, size int64, expectedSHA256 string) (objectstore.PartInfo, error) {
	part, err := store.Store.UploadPart(ctx, uploadID, number, reader, size, expectedSHA256)
	if err == nil && store.fail {
		store.fail = false
		return part, errors.New("simulated multipart interruption")
	}
	return part, err
}

func mustExportManifest(t *testing.T, repository *Repository, id string) []byte {
	t.Helper()
	encoded, err := repository.ExportEncryptedManifest(id)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type mirrorFixture struct {
	manifest     model.CheckpointManifest
	plaintext    []byte
	workloadKey  []byte
	keyVersion   uint32
	remoteRoot   string
	remote       *objectstore.Local
	sourceChunks *chunkstore.Store
	sourceRepo   *Repository
	sourceMirror *Mirror
}

func newMirrorFixture(t *testing.T) mirrorFixture {
	t.Helper()
	sourceRoot := t.TempDir()
	keys, err := securestore.Open(filepath.Join(sourceRoot, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(filepath.Join(sourceRoot, "objects"), 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("private checkpoint payload\n"), 8000)
	asset, err := chunks.PutAsset(context.Background(), "workload-1", "memory", "application/octet-stream", bytes.NewReader(plaintext), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	keyVersion := asset.Asset.Chunks[0].KeyVersion
	workloadKey, err := keys.ExportWorkloadKey("workload-1", keyVersion)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := identity.Ensure(filepath.Join(sourceRoot, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: "checkpoint-1", Kind: model.CheckpointFull, CreatedAt: time.Now().UTC(),
		Workload: model.WorkloadSpec{ID: "workload-1", Name: "sensitive-workload-name", RootPath: "/srv/private", Environment: map[string]string{"SECRET": "private-workload-secret"}},
		Assets:   []model.AssetManifest{asset.Asset},
		Metrics:  model.CheckpointMetrics{PlainBytes: asset.Asset.PlainSize, StoredBytes: asset.Asset.StoredSize, ChunkCount: len(asset.Asset.Chunks)},
		Security: model.SecurityEnvelope{KeyVersion: keyVersion},
	}
	if err := SignManifest(&manifest, machine); err != nil {
		t.Fatal(err)
	}
	remoteRoot := t.TempDir()
	remote, err := objectstore.OpenLocal(remoteRoot)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(filepath.Join(sourceRoot, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Save(manifest); err != nil {
		t.Fatal(err)
	}
	return mirrorFixture{
		manifest: manifest, plaintext: plaintext, workloadKey: workloadKey, keyVersion: keyVersion,
		remoteRoot: remoteRoot, remote: remote, sourceChunks: chunks, sourceRepo: repository, sourceMirror: NewMirror(remote, chunks, repository),
	}
}

func (fixture mirrorFixture) destination(t *testing.T) (*Mirror, *chunkstore.Store, *Repository) {
	t.Helper()
	root := t.TempDir()
	keys, err := securestore.Open(filepath.Join(root, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.ImportWorkloadKey(fixture.manifest.Workload.ID, fixture.keyVersion, fixture.workloadKey); err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(filepath.Join(root, "objects"), 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(filepath.Join(root, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	return NewMirror(fixture.remote, chunks, repository), chunks, repository
}
