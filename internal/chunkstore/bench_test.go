package chunkstore

import (
	"bytes"
	"crypto/rand"
	"testing"

	"shift.dev/shift/internal/securestore"
)

// benchChunkSize is the agent's default chunk size, so every payload here is
// the exact body a production chunk carries.
const benchChunkSize = 4 << 20

// benchPayloadNames enumerates the payload shapes a checkpoint actually
// stores: text streams (logs, config, source), a real heap's mix of
// compressible regions and binary spans, untouched zero pages, and
// incompressible random bytes as the worst case.
var benchPayloadNames = []string{"text", "heap-mix", "zeros", "random"}

// benchmarkPayload builds one 4 MiB body of the shape named.
func benchmarkPayload(name string) []byte {
	buffer := make([]byte, benchChunkSize)
	switch name {
	case "text":
		line := []byte("2026-09-11T12:34:56Z level=info component=checkpoint workload=demo message=state captured\n")
		for offset := 0; offset+len(line) <= benchChunkSize; offset += len(line) {
			copy(buffer[offset:], line)
		}
	case "heap-mix":
		block := 64 << 10
		line := []byte("heap region with repeated allocation patterns and string data\n")
		for offset := 0; offset+block <= benchChunkSize; offset += block {
			if (offset/block)%4 == 3 {
				fillRandom(buffer[offset : offset+block])
			} else {
				for inner := offset; inner+len(line) <= offset+block; inner += len(line) {
					copy(buffer[inner:], line)
				}
			}
		}
	case "zeros":
	case "random":
		fillRandom(buffer)
	}
	return buffer
}

// fillRandom fills the buffer with incompressible bytes.
func fillRandom(buffer []byte) {
	if _, err := rand.Read(buffer); err != nil {
		panic("chunkstore benchmark: random fill: " + err.Error())
	}
}

// TestCodecRatiosAndOverhead measures and pins the compression behavior the
// benchmark documentation cites: how far each payload shape shrinks under
// the current zstd codec compared with the gzip codec it replaced, that
// incompressible data is stored with framing overhead only instead of
// expanding, and that every shape round-trips byte for byte.
func TestCodecRatiosAndOverhead(t *testing.T) {
	for _, name := range benchPayloadNames {
		payload := benchmarkPayload(name)
		zstdBody := compress(payload)
		gzipBody, err := legacyGzipCompress(payload)
		if err != nil {
			t.Fatalf("%s: legacy gzip: %v", name, err)
		}
		t.Logf("%-8s plain %d  zstd %d (%.1f%%)  gzip-bestspeed %d (%.1f%%)",
			name, len(payload), len(zstdBody), 100*float64(len(zstdBody))/float64(len(payload)),
			len(gzipBody), 100*float64(len(gzipBody))/float64(len(payload)))
		if name == "text" || name == "heap-mix" {
			if len(zstdBody) >= len(gzipBody) {
				t.Errorf("%s: zstd stored %d bytes, worse than gzip's %d", name, len(zstdBody), len(gzipBody))
			}
		}
		if float64(len(zstdBody)) > 1.001*float64(len(payload)) {
			t.Errorf("%s: incompressible data expanded to %.3f× its size", name, float64(len(zstdBody))/float64(len(payload)))
		}
		plaintext, codec, err := decompress(zstdBody, int64(len(payload)))
		if err != nil || codec != "zstd" || !bytes.Equal(plaintext, payload) {
			t.Fatalf("%s: round trip failed: codec=%q err=%v", name, codec, err)
		}
	}
}

// BenchmarkChunkCodec measures compression and decompression throughput of
// the current zstd codec and the gzip codec it replaced, in isolation (no
// encryption, no disk), per payload shape. Run with:
//
//	go test ./internal/chunkstore/ -bench BenchmarkChunkCodec -benchtime 2s
func BenchmarkChunkCodec(b *testing.B) {
	for _, name := range benchPayloadNames {
		payload := benchmarkPayload(name)
		zstdBody := compress(payload)
		b.Run("compress-zstd/"+name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = compress(payload)
			}
		})
		b.Run("compress-gzip/"+name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _ = legacyGzipCompress(payload)
			}
		})
		b.Run("decompress-zstd/"+name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_, _, _ = decompress(zstdBody, int64(len(payload)))
			}
		})
	}
}

// openBenchStore opens a store over a throwaway directory with the benchmark
// chunk size, exactly as the agent configures it.
func openBenchStore(b *testing.B) (*Store, *securestore.Manager) {
	b.Helper()
	root := b.TempDir()
	keys, err := securestore.Open(root + "/keys")
	if err != nil {
		b.Fatal(err)
	}
	store, err := Open(root+"/store", benchChunkSize, keys)
	if err != nil {
		b.Fatal(err)
	}
	return store, keys
}

// BenchmarkChunkStore measures the whole chunk path the agent pays per
// checkpoint: a fresh chunk write (compress + encrypt + fsync + link), the
// dedup verification of re-storing identical state, and the read path a
// restore runs (read + digest + decrypt + decompress + content check).
// Fresh writes vary one 8-byte region per iteration so every iteration
// stores new content rather than deduplicating. Run with:
//
//	go test ./internal/chunkstore/ -bench BenchmarkChunkStore -benchtime 2s
func BenchmarkChunkStore(b *testing.B) {
	store, keys := openBenchStore(b)
	payload := benchmarkPayload("heap-mix")
	keyVersion, dataKey, err := keys.GetOrCreateWorkloadKey("bench")
	if err != nil {
		b.Fatal(err)
	}
	b.Run("put-fresh", func(b *testing.B) {
		b.SetBytes(int64(len(payload)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			variation := append([]byte{}, payload...)
			variation[64] = byte(i)
			variation[65] = byte(i >> 8)
			if _, _, err := store.putChunk("bench", keyVersion, dataKey, i, variation); err != nil {
				b.Fatal(err)
			}
		}
	})
	ref, existed, err := store.putChunk("bench", keyVersion, dataKey, 0, payload)
	if err != nil || existed {
		b.Fatalf("seed put: existed=%v err=%v", existed, err)
	}
	b.Run("put-deduplicated", func(b *testing.B) {
		b.SetBytes(int64(len(payload)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := store.putChunk("bench", keyVersion, dataKey, 0, payload); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("restore", func(b *testing.B) {
		b.SetBytes(int64(len(payload)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := store.readChunk("bench", ref); err != nil {
				b.Fatal(err)
			}
		}
	})
}
