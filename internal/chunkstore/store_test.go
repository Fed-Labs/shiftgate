package chunkstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

func TestAssetRoundTripAndDeduplication(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(root+"/store", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("portable-state\n"), 20000)
	first, err := store.PutAsset(context.Background(), "w1", "fs", "application/x-tar", bytes.NewReader(content), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PutAsset(context.Background(), "w1", "fs-copy", "application/x-tar", bytes.NewReader(content), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if second.DeduplicatedBytes != int64(len(content)) {
		t.Fatalf("deduplicated %d bytes, want %d", second.DeduplicatedBytes, len(content))
	}
	var restored bytes.Buffer
	if err := store.RestoreAsset(context.Background(), "w1", first.Asset, &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, restored.Bytes()) {
		t.Fatal("restored asset differs")
	}
}

func TestAssetTamperingFailsClosed(t *testing.T) {
	root := t.TempDir()
	keys, _ := securestore.Open(root + "/keys")
	store, _ := Open(root+"/store", 64<<10, keys)
	result, err := store.PutAsset(context.Background(), "w1", "memory", "application/octet-stream", bytes.NewReader([]byte("memory")), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.chunkPath(result.Asset.Chunks[0])
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := os.ReadFile(path)
	encoded[len(encoded)-1] ^= 0xff
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateAsset(context.Background(), "w1", result.Asset); err == nil {
		t.Fatal("tampered chunk validated")
	}
}

func TestConcurrentContentAddressedWritesVerifyWinner(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(root+"/store", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("same state"), 1000)
	const workers = 8
	results := make(chan PutResult, workers)
	errorsChannel := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.PutAsset(context.Background(), "workload", "asset", "application/octet-stream", bytes.NewReader(content), true, 1)
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Fatal(err)
	}
	for result := range results {
		if err := store.ValidateAsset(context.Background(), "workload", result.Asset); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImportChunkRetryIsIdempotent(t *testing.T) {
	sourceRoot := t.TempDir()
	sourceKeys, err := securestore.Open(sourceRoot + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	source, err := Open(sourceRoot+"/store", 64<<10, sourceKeys)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("transfer me"), 1000)
	asset, err := source.PutAsset(context.Background(), "workload", "asset", "application/octet-stream", bytes.NewReader(content), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	keyVersion := asset.Asset.Chunks[0].KeyVersion
	key, err := sourceKeys.ExportWorkloadKey("workload", keyVersion)
	if err != nil {
		t.Fatal(err)
	}
	destinationRoot := t.TempDir()
	destinationKeys, err := securestore.Open(destinationRoot + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	if err := destinationKeys.ImportWorkloadKey("workload", keyVersion, key); err != nil {
		t.Fatal(err)
	}
	destination, err := Open(destinationRoot+"/store", 64<<10, destinationKeys)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := source.ExportChunk(asset.Asset.Chunks[0], &encoded); err != nil {
		t.Fatal(err)
	}
	inserted, err := destination.ImportChunkResult(context.Background(), "workload", asset.Asset.Chunks[0], bytes.NewReader(encoded.Bytes()))
	if err != nil || !inserted {
		t.Fatalf("first import: inserted=%v err=%v", inserted, err)
	}
	inserted, err = destination.ImportChunkResult(context.Background(), "workload", asset.Asset.Chunks[0], bytes.NewReader(encoded.Bytes()))
	if err != nil || inserted {
		t.Fatalf("retry import: inserted=%v err=%v", inserted, err)
	}
	if err := destination.ValidateAsset(context.Background(), "workload", asset.Asset); err != nil {
		t.Fatal(err)
	}
}

// legacyGzipCompress reproduces the codec every chunk written before the
// zstd switch used, so tests can fabricate chunks exactly as an older agent
// binary stored them and prove they still restore.
func legacyGzipCompress(plaintext []byte) ([]byte, error) {
	var output bytes.Buffer
	writer, err := gzip.NewWriterLevel(&output, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(plaintext); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// storeLegacyGzipChunk writes one chunk through the pre-zstd pipeline
// (gzip → encrypt → encode) directly to disk, bypassing putChunk, and
// returns the manifest ref an older binary would have recorded for it.
func storeLegacyGzipChunk(t *testing.T, store *Store, keys *securestore.Manager, workloadID string, sequence int, plaintext []byte) model.ChunkRef {
	t.Helper()
	keyVersion, dataKey, err := keys.GetOrCreateWorkloadKey(workloadID)
	if err != nil {
		t.Fatal(err)
	}
	address, err := securestore.Address(dataKey, workloadID, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := legacyGzipCompress(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	aad := chunkAAD(workloadID, address, keyVersion, int64(len(plaintext)))
	nonce, ciphertext, err := securestore.Encrypt(dataKey, workloadID, "checkpoint-chunk", compressed, aad)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeChunk(keyVersion, int64(len(plaintext)), nonce, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	plainDigest := sha256.Sum256(plaintext)
	cipherDigest := sha256.Sum256(encoded)
	ref := model.ChunkRef{
		Address:      address,
		KeyVersion:   keyVersion,
		PlainSize:    int64(len(plaintext)),
		StoredSize:   int64(len(encoded)),
		CipherSHA256: hex.EncodeToString(cipherDigest[:]),
		PlainSHA256:  hex.EncodeToString(plainDigest[:]),
		Sequence:     sequence,
		Compression:  "gzip",
		Encryption:   "AES-256-GCM",
	}
	path, err := store.chunkPath(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestNewChunksCompressWithZstd: every chunk the store writes now carries
// the zstd label, compressible state genuinely shrinks, the round trip is
// byte-exact, and a body with neither codec's frame magic is refused rather
// than guessed at.
func TestNewChunksCompressWithZstd(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(root+"/store", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("portable zstd state\n"), 20000)
	result, err := store.PutAsset(context.Background(), "w1", "fs", "application/x-tar", bytes.NewReader(content), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Asset.Chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(result.Asset.Chunks))
	}
	for _, ref := range result.Asset.Chunks {
		if ref.Compression != "zstd" {
			t.Fatalf("chunk %d labeled %q, want zstd", ref.Sequence, ref.Compression)
		}
	}
	if result.Asset.StoredSize >= result.Asset.PlainSize {
		t.Fatalf("compressible state did not shrink: plain %d stored %d", result.Asset.PlainSize, result.Asset.StoredSize)
	}
	var restored bytes.Buffer
	if err := store.RestoreAsset(context.Background(), "w1", result.Asset, &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, restored.Bytes()) {
		t.Fatal("restored asset differs")
	}
	if _, codec, err := decompress(compress(content[:4096]), 4096); err != nil || codec != "zstd" {
		t.Fatalf("direct codec round trip: codec=%q err=%v", codec, err)
	}
	if _, _, err := decompress([]byte("not a compressed frame at all"), 8); err == nil {
		t.Fatal("unknown frame magic was decompressed instead of refused")
	}
}

// TestGzipChunksFromOlderBinariesStillRestore: a chunk written by a pre-zstd
// agent — gzip-compressed, gzip-labeled — restores byte for byte under the
// current store, including inside one manifest that mixes it with a new
// zstd chunk: the shape a workload's asset list takes right after an
// upgrade.
func TestGzipChunksFromOlderBinariesStillRestore(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(root+"/store", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	partA := bytes.Repeat([]byte("written by the old binary\n"), 900)
	partB := bytes.Repeat([]byte("written by the new binary\n"), 900)
	legacy := storeLegacyGzipChunk(t, store, keys, "w2", 0, partA)
	keyVersion, dataKey, err := keys.GetOrCreateWorkloadKey("w2")
	if err != nil {
		t.Fatal(err)
	}
	modern, existed, err := store.putChunk("w2", keyVersion, dataKey, 1, partB)
	if err != nil {
		t.Fatal(err)
	}
	if existed {
		t.Fatal("a fresh chunk reported deduplication")
	}
	if modern.Compression != "zstd" {
		t.Fatalf("new chunk labeled %q, want zstd", modern.Compression)
	}
	whole := sha256.New()
	_, _ = whole.Write(partA)
	_, _ = whole.Write(partB)
	asset := model.AssetManifest{
		Name: "fs", MediaType: "application/x-tar", Required: true, RestoreOrder: 1,
		Chunks:     []model.ChunkRef{legacy, modern},
		PlainSize:  int64(len(partA) + len(partB)),
		StoredSize: legacy.StoredSize + modern.StoredSize,
		SHA256:     hex.EncodeToString(whole.Sum(nil)),
	}
	if err := store.ValidateAsset(context.Background(), "w2", asset); err != nil {
		t.Fatalf("mixed gzip+zstd asset did not validate: %v", err)
	}
	var restored bytes.Buffer
	if err := store.RestoreAsset(context.Background(), "w2", asset, &restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(append([]byte{}, partA...), partB...), restored.Bytes()) {
		t.Fatal("restored asset differs from the original plaintext")
	}
}

// TestDedupAgainstLegacyGzipChunkLabelsTheRealCodec: re-putting plaintext an
// older binary already stored deduplicates against the existing chunk, and
// the manifest ref it returns describes the bytes actually on disk — their
// gzip codec, their size, their digest — not the write that lost the race.
func TestDedupAgainstLegacyGzipChunkLabelsTheRealCodec(t *testing.T) {
	root := t.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(root+"/store", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte("already stored as gzip\n"), 1000)
	legacy := storeLegacyGzipChunk(t, store, keys, "w3", 0, content)
	result, err := store.PutAsset(context.Background(), "w3", "fs-copy", "application/x-tar", bytes.NewReader(content), true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.DeduplicatedBytes != int64(len(content)) {
		t.Fatalf("deduplicated %d bytes, want %d — the legacy chunk was not reused", result.DeduplicatedBytes, len(content))
	}
	ref := result.Asset.Chunks[0]
	if ref.Address != legacy.Address || ref.Compression != "gzip" {
		t.Fatalf("dedup ref = address %s codec %q, want the legacy address labeled gzip", ref.Address, ref.Compression)
	}
	if ref.StoredSize != legacy.StoredSize || ref.CipherSHA256 != legacy.CipherSHA256 {
		t.Fatal("dedup ref does not describe the bytes actually on disk")
	}
	if err := store.ValidateAsset(context.Background(), "w3", result.Asset); err != nil {
		t.Fatal(err)
	}
}
