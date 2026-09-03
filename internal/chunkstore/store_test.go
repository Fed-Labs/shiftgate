package chunkstore

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"

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
