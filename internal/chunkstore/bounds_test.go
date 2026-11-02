package chunkstore

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	keys, err := securestore.Open(t.TempDir() + "/keys")
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(t.TempDir()+"/objects", 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// TestDecompressionBombRefused: a tiny crafted frame must never expand past
// the declared plain size or the absolute plain-size cap. The declared size is
// bounded before decoding, and the decoder itself is memory-capped, so a
// bomb is refused instead of materialized.
func TestDecompressionBombRefused(t *testing.T) {
	bomb := make([]byte, 256<<20)
	compressed, err := compress(bomb)
	if err != nil {
		t.Fatal(err)
	}
	if len(compressed) > 1<<20 {
		t.Fatalf("bomb frame should compress small, got %d bytes", len(compressed))
	}
	if _, _, err := decompress(compressed, maxPlainChunk+1); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("a declared size above the cap must be refused before decoding, got %v", err)
	}
	if _, _, err := decompress(compressed, 0); err == nil {
		t.Fatal("a zero declared size must be refused")
	}
	if _, _, err := decompress(compressed, 1<<20); err == nil {
		t.Fatal("a frame expanding beyond its declared size must be refused")
	}
}

func TestImportRejectsOversizedRefs(t *testing.T) {
	store := newTestStore(t)
	for name, ref := range map[string]model.ChunkRef{
		"zero plain size":     {Address: strings.Repeat("ab", 32), PlainSize: 0, StoredSize: 10},
		"plain size too big":  {Address: strings.Repeat("ab", 32), PlainSize: maxPlainChunk + 1, StoredSize: 10},
		"zero stored size":    {Address: strings.Repeat("ab", 32), PlainSize: 10, StoredSize: 0},
		"stored size too big": {Address: strings.Repeat("ab", 32), PlainSize: 10, StoredSize: maxEncodedChunk + 1},
	} {
		if _, err := store.ImportChunkResult(context.Background(), "w", ref, bytes.NewReader(nil)); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("%s must be refused before any read, got %v", name, err)
		}
	}
}

// TestImportAcceptsPlainSizesAboveTheLocalChunkSize: chunk size is a
// per-agent setting, and the import bound is the absolute memory-safe cap —
// a destination tuned to a smaller chunk size than its source must not refuse
// the source's chunks with "out of range". Such a ref passes the gate and is
// then judged by the upload itself.
func TestImportAcceptsPlainSizesAboveTheLocalChunkSize(t *testing.T) {
	store := newTestStore(t) // configured with 64 KiB chunks
	ref := model.ChunkRef{Address: strings.Repeat("ab", 32), PlainSize: 1 << 20, StoredSize: 10}
	_, err := store.ImportChunkResult(context.Background(), "w", ref, bytes.NewReader(nil))
	if err == nil || strings.Contains(err.Error(), "out of range") {
		t.Fatalf("a plain size above the local chunk size but within maxPlainChunk must pass the bound, got %v", err)
	}
}

func TestDecodeChunkRejectsHugePlainSize(t *testing.T) {
	encoded, err := encodeChunk(1, maxPlainChunk+1, bytes.Repeat([]byte{1}, 12), []byte("ciphertext"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := decodeChunk(encoded); err == nil || !strings.Contains(err.Error(), "plain size out of range") {
		t.Fatalf("a header declaring a huge plain size must be refused, got %v", err)
	}
}

func TestVerifyChunkRejectsUnboundedRefs(t *testing.T) {
	store := newTestStore(t)
	ref := model.ChunkRef{Address: strings.Repeat("ab", 32), PlainSize: maxPlainChunk + 1, StoredSize: 10}
	if _, err := store.readChunk("w", ref); err == nil || err.Error() != "chunk plain size out of range" {
		t.Fatalf("verify must refuse an out-of-range plain size, got %v", err)
	}
}
