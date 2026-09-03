package objectstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLocalPutGetIntegrityAndIdempotency(t *testing.T) {
	store, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	content := bytes.Repeat([]byte("encrypted-checkpoint-chunk"), 1024)
	digest := sha256Hex(content)
	info, err := store.Put(ctx, "organizations/org/checkpoints/cp/chunk-1", bytes.NewReader(content), int64(len(content)), digest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(content)) || info.SHA256 != digest {
		t.Fatalf("unexpected object info: %#v", info)
	}
	idempotent, err := store.Put(ctx, info.Key, bytes.NewReader(content), int64(len(content)), digest)
	if err != nil {
		t.Fatal(err)
	}
	if idempotent.SHA256 != digest {
		t.Fatalf("idempotent put returned wrong digest: %#v", idempotent)
	}
	if _, err := store.Put(ctx, info.Key, bytes.NewReader([]byte("different")), 9, sha256Hex([]byte("different"))); !errors.Is(err, ErrConflict) {
		t.Fatalf("different content did not conflict: %v", err)
	}
	var restored bytes.Buffer
	got, err := store.Get(ctx, info.Key, &restored)
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != digest || !bytes.Equal(restored.Bytes(), content) {
		t.Fatal("restored object did not match input")
	}
	if err := store.Delete(ctx, info.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, info.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted object remained visible: %v", err)
	}
}

func TestLocalRejectsUnsafeKeysAndBadDigests(t *testing.T) {
	store, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"", "/absolute", "../escape", "safe/../../escape", "double//slash", `windows\\escape`} {
		if _, err := store.Put(ctx, key, bytes.NewReader(nil), 0, ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("unsafe key %q returned %v", key, err)
		}
	}
	if _, err := store.Put(ctx, "valid/key", bytes.NewReader([]byte("value")), 5, "not-a-hash"); err == nil {
		t.Fatal("invalid expected digest was accepted")
	}
	if _, err := store.Put(ctx, "valid/key", bytes.NewReader([]byte("value")), 5, sha256Hex([]byte("other"))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("mismatched expected digest returned %v", err)
	}
}

func TestLocalDetectsObjectTampering(t *testing.T) {
	root := t.TempDir()
	store, err := OpenLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("ciphertext")
	key := "chunks/v1/abc"
	if _, err := store.Put(context.Background(), key, bytes.NewReader(content), int64(len(content)), sha256Hex(content)); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "objects", filepath.FromSlash(key))
	if err := os.WriteFile(path, []byte("tampered!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := store.Get(context.Background(), key, &output); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered object returned %v", err)
	}
}

func TestLocalMultipartResumeCompleteAndCleanup(t *testing.T) {
	root := t.TempDir()
	store, err := OpenLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first := bytes.Repeat([]byte("a"), 256<<10)
	second := bytes.Repeat([]byte("b"), 128<<10)
	whole := append(append([]byte(nil), first...), second...)
	upload, err := store.InitiateMultipart(ctx, "checkpoints/cp-1/state", int64(len(whole)), sha256Hex(whole))
	if err != nil {
		t.Fatal(err)
	}
	partOne, err := store.UploadPart(ctx, upload.UploadID, 1, bytes.NewReader(first), int64(len(first)), sha256Hex(first))
	if err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := reopened.ListParts(ctx, upload.UploadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0] != partOne {
		t.Fatalf("multipart state did not survive reopen: %#v", parts)
	}
	if duplicate, err := reopened.UploadPart(ctx, upload.UploadID, 1, bytes.NewReader(first), int64(len(first)), sha256Hex(first)); err != nil || duplicate != partOne {
		t.Fatalf("idempotent multipart retry failed: %#v %v", duplicate, err)
	}
	if _, err := reopened.UploadPart(ctx, upload.UploadID, 1, bytes.NewReader(second), int64(len(second)), sha256Hex(second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting multipart retry returned %v", err)
	}
	partTwo, err := reopened.UploadPart(ctx, upload.UploadID, 2, bytes.NewReader(second), int64(len(second)), sha256Hex(second))
	if err != nil {
		t.Fatal(err)
	}
	info, err := reopened.CompleteMultipart(ctx, upload.UploadID, []PartInfo{partOne, partTwo})
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(whole)) || info.SHA256 != sha256Hex(whole) {
		t.Fatalf("unexpected completed multipart object: %#v", info)
	}
	var restored bytes.Buffer
	if _, err := reopened.Get(ctx, upload.Key, &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), whole) {
		t.Fatal("completed multipart object was corrupt")
	}

	stale, err := reopened.InitiateMultipart(ctx, "stale/object", 0, sha256Hex(nil))
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, "multipart", stale.UploadID, "meta.json")
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339Nano)
	metadata = bytes.Replace(metadata, []byte(stale.InitiatedAt.Format(time.RFC3339Nano)), []byte(old), 1)
	if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := reopened.CleanupMultipart(ctx, "stale/", time.Now().UTC().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d stale uploads, want 1", removed)
	}
	if _, err := reopened.ListParts(ctx, stale.UploadID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale multipart upload remained: %v", err)
	}
}

func TestLocalConcurrentIdenticalPut(t *testing.T) {
	store, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("same-content"), 8192)
	digest := sha256Hex(content)
	const workers = 8
	var wait sync.WaitGroup
	errorsChannel := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Put(context.Background(), "concurrent/object", bytes.NewReader(content), int64(len(content)), digest)
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
