package objectstore

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func newDeferredTestStore(t *testing.T) *S3 {
	t.Helper()
	serverState := newFakeS3()
	store, err := OpenS3(S3Config{
		Endpoint: "http://s3.test", Region: "test-1", Bucket: "shift-test", Prefix: "tenant-a",
		AccessKeyID: "access", SecretAccessKey: "secret", StateDir: t.TempDir(), ForcePathStyle: true,
		HTTPClient: &http.Client{Transport: handlerTransport{handler: serverState}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.clock = func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
	return store
}

func TestDeferredFailsClosedBeforeSwap(t *testing.T) {
	deferred := NewDeferred()
	if deferred.Available() {
		t.Fatal("empty deferred store reported a backend")
	}
	if _, err := deferred.Put(context.Background(), "checkpoints/cp/chunk", bytes.NewReader([]byte("x")), 1, ""); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("Put before swap: %v, want ErrCredentialsUnavailable", err)
	}
	if _, err := deferred.Head(context.Background(), "checkpoints/cp/chunk"); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("Head before swap: %v, want ErrCredentialsUnavailable", err)
	}
	if _, err := deferred.ListMultipart(context.Background(), "chunks"); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("ListMultipart before swap: %v, want ErrCredentialsUnavailable", err)
	}
	if err := deferred.SetCredentials("a", "b", "c"); !errors.Is(err, ErrCredentialsUnavailable) {
		t.Fatalf("SetCredentials before swap: %v, want ErrCredentialsUnavailable", err)
	}
}

func TestDeferredDelegatesAfterSwap(t *testing.T) {
	deferred := NewDeferred()
	store := newDeferredTestStore(t)
	deferred.Swap(store)
	if !deferred.Available() {
		t.Fatal("swapped deferred store reports no backend")
	}
	content := []byte("ciphertext")
	digest := sha256Hex(content)
	if _, err := deferred.Put(context.Background(), "checkpoints/cp/manifest", bytes.NewReader(content), int64(len(content)), digest); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := deferred.Get(context.Background(), "checkpoints/cp/manifest", &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), content) {
		t.Fatal("deferred store content did not round trip")
	}
	if err := deferred.SetCredentials("rotated", "rotated-secret", "rotated-token"); err != nil {
		t.Fatal(err)
	}
}
