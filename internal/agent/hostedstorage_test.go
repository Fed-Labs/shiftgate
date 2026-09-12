package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shift.dev/shift/internal/objectstore"
)

// hostedstorage_test.go: the credential loop against an in-process control
// plane. The fake answers the credential endpoint with rotating values, a
// quota refusal, or nothing at all, so the loop's fail-closed behavior is
// exercised exactly: issue → swap, rotate → SetCredentials, quota → distinct
// error, unreachable → credentials retained.

type fakeControlPlane struct {
	mu       sync.Mutex
	requests int
	mode     string // "issue", "quota", "not-configured", "unreachable", "invalid"
	issued   int
	endpoint string
}

func (fake *fakeControlPlane) setMode(mode string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.mode = mode
}

func (fake *fakeControlPlane) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.requests
}

func (fake *fakeControlPlane) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, "/storage/credentials") {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if request.Header.Get("Authorization") != "Bearer agent-key" {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	fake.mu.Lock()
	fake.requests++
	mode := fake.mode
	fake.issued++
	index := fake.issued
	endpoint := fake.endpoint
	fake.mu.Unlock()
	switch mode {
	case "quota":
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write([]byte(`{"code":"STORAGE_QUOTA_EXCEEDED","message":"over plan"}`))
		return
	case "not-configured":
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"code":"STORAGE_NOT_CONFIGURED","message":"self-hosted deployment"}`))
		return
	case "unreachable":
		// Simulate transport failure: close without a response.
		panic(http.ErrAbortHandler)
	case "invalid":
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"endpoint":"","region":""}`))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"endpoint":          endpoint,
		"region":            "test-1",
		"bucket":            "shift-checkpoints",
		"prefix":            "org/org_1",
		"access_key_id":     fmt.Sprintf("key-%d", index),
		"secret_access_key": fmt.Sprintf("secret-%d", index),
		"session_token":     fmt.Sprintf("token-%d", index),
		"expiration":        time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"force_path_style":  true,
	})
}

// newHostedLoop builds a loop pointed at the fake control plane, with a
// listing backend behind it so swapped credentials can be observed.
func newHostedLoop(t *testing.T, controlPlane *httptest.Server, fake *fakeControlPlane, storage *httptest.Server) (*hostedStorageLoop, *objectstore.Deferred) {
	t.Helper()
	fake.mu.Lock()
	fake.endpoint = storage.URL
	fake.mu.Unlock()
	deferred := objectstore.NewDeferred()
	loop := newHostedStorageLoop(hostedStorageConfig{
		controlURL:   controlPlane.URL,
		organization: "org_1",
		apiKey:       "agent-key",
		stateDir:     t.TempDir(),
	}, deferred, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return loop, deferred
}

// tinyFakeS3 is a minimal object store: it accepts puts, remembers each
// object's size and SHA-256 metadata, and answers HEAD so the client's
// verified-put round trip completes. It records the Authorization header of
// every request, which is how tests observe credential rotation.
type tinyFakeS3 struct {
	mu             sync.Mutex
	authorizations []string
	objects        map[string]int64
	hashes         map[string]string
}

func (fake *tinyFakeS3) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	fake.mu.Lock()
	fake.authorizations = append(fake.authorizations, request.Header.Get("Authorization"))
	if fake.objects == nil {
		fake.objects = map[string]int64{}
		fake.hashes = map[string]string{}
	}
	objects := fake.objects
	hashes := fake.hashes
	fake.mu.Unlock()
	if request.Method == http.MethodHead {
		fake.mu.Lock()
		size, ok := objects[request.URL.Path]
		sha := hashes[request.URL.Path]
		fake.mu.Unlock()
		if !ok {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		writer.Header().Set("X-Amz-Meta-Sha256", sha)
		writer.Header().Set("ETag", `"tiny"`)
		writer.WriteHeader(http.StatusOK)
		return
	}
	body, _ := io.ReadAll(request.Body)
	fake.mu.Lock()
	objects[request.URL.Path] = int64(len(body))
	hashes[request.URL.Path] = request.Header.Get("X-Amz-Meta-Sha256")
	fake.mu.Unlock()
	writer.WriteHeader(http.StatusOK)
}

func (fake *tinyFakeS3) lastAuthorization() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.authorizations) == 0 {
		return ""
	}
	return fake.authorizations[len(fake.authorizations)-1]
}

// fetchOnce performs one credential fetch through the loop under test.
func fetchOnce(ctx context.Context, loop *hostedStorageLoop, controlPlane *httptest.Server) (objectstore.STSCredentials, error) {
	return loop.fetch(ctx)
}

// The first issuance constructs a usable store: the deferred store answers a
// Put after apply where before it failed closed.
func TestHostedStorageFirstIssuanceSwapsStore(t *testing.T) {
	fake := &fakeControlPlane{mode: "issue"}
	controlPlane := httptest.NewServer(fake)
	defer controlPlane.Close()
	s3State := &tinyFakeS3{}
	storage := httptest.NewServer(s3State)
	defer storage.Close()
	loop, deferred := newHostedLoop(t, controlPlane, fake, storage)

	ctx := context.Background()
	if _, err := deferred.Put(ctx, "checkpoints/cp/manifest", strings.NewReader("x"), 1, ""); !errors.Is(err, objectstore.ErrCredentialsUnavailable) {
		t.Fatalf("Put before issuance: %v, want ErrCredentialsUnavailable", err)
	}
	credentials, err := fetchOnce(ctx, loop, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessKeyID != "key-1" {
		t.Fatalf("first credentials: %+v", credentials)
	}
	if err := loop.apply(credentials); err != nil {
		t.Fatal(err)
	}
	if !deferred.Available() {
		t.Fatal("deferred store still unavailable after apply")
	}
	if _, err := deferred.Put(ctx, "checkpoints/cp/manifest", strings.NewReader("x"), 1, ""); err != nil {
		t.Fatalf("Put after first issuance: %v", err)
	}
}

// A later issuance rotates credentials on the installed client instead of
// rebuilding it: the next object put carries the new access key.
func TestHostedStorageRotationUpdatesSigning(t *testing.T) {
	fake := &fakeControlPlane{mode: "issue"}
	controlPlane := httptest.NewServer(fake)
	defer controlPlane.Close()
	s3State := &tinyFakeS3{}
	storage := httptest.NewServer(s3State)
	defer storage.Close()
	loop, deferred := newHostedLoop(t, controlPlane, fake, storage)
	ctx := context.Background()

	first, err := fetchOnce(ctx, loop, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.apply(first); err != nil {
		t.Fatal(err)
	}
	second, err := fetchOnce(ctx, loop, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessKeyID == first.AccessKeyID {
		t.Fatalf("fake control plane did not rotate credentials: %s", second.AccessKeyID)
	}
	if err := loop.apply(second); err != nil {
		t.Fatal(err)
	}
	if _, err := deferred.Put(ctx, "checkpoints/cp/manifest", strings.NewReader("x"), 1, ""); err != nil {
		t.Fatal(err)
	}
	// The store is unchanged in identity, so this is the same client; only its
	// signing credentials moved. The S3 request must now carry key-2.
	authorization := s3State.lastAuthorization()
	if !strings.Contains(authorization, "Credential=key-2/") {
		t.Fatalf("rotated credentials were not used for signing: %q", authorization)
	}
}

// A quota refusal is a distinct, non-fatal error: nothing is swapped, the
// previously installed credentials remain, and the loop records the state.
func TestHostedStorageQuotaIsDistinct(t *testing.T) {
	fake := &fakeControlPlane{mode: "issue"}
	controlPlane := httptest.NewServer(fake)
	defer controlPlane.Close()
	s3State := &tinyFakeS3{}
	storage := httptest.NewServer(s3State)
	defer storage.Close()
	loop, deferred := newHostedLoop(t, controlPlane, fake, storage)
	ctx := context.Background()

	if _, err := fetchOnce(ctx, loop, controlPlane); err != nil {
		t.Fatal(err)
	}
	if err := loop.apply(mustCredentials(t, loop, controlPlane)); err != nil {
		t.Fatal(err)
	}
	fake.setMode("quota")
	if _, err := fetchOnce(ctx, loop, controlPlane); !errors.Is(err, ErrStorageQuotaExceeded) {
		t.Fatalf("quota refusal: %v, want ErrStorageQuotaExceeded", err)
	}
	// Existing credentials still serve; the quota suspended issuance, not the
	// installed store.
	if !deferred.Available() {
		t.Fatal("quota refusal uninstalled the working store")
	}
	fake.setMode("not-configured")
	if _, err := fetchOnce(ctx, loop, controlPlane); !errors.Is(err, ErrStorageNotConfigured) {
		t.Fatalf("not-configured refusal: %v, want ErrStorageNotConfigured", err)
	}
}

// fetchAndApply with an unreachable control plane keeps the last working
// credentials — the mirror degrades only when they actually expire.
func TestHostedStorageUnreachableRetainsCredentials(t *testing.T) {
	fake := &fakeControlPlane{mode: "issue"}
	controlPlane := httptest.NewServer(fake)
	defer controlPlane.Close()
	s3State := &tinyFakeS3{}
	storage := httptest.NewServer(s3State)
	defer storage.Close()
	loop, deferred := newHostedLoop(t, controlPlane, fake, storage)
	ctx := context.Background()

	credentials, err := fetchOnce(ctx, loop, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.apply(credentials); err != nil {
		t.Fatal(err)
	}
	fake.setMode("unreachable")
	loop.fetchAndApply(ctx)
	if !deferred.Available() {
		t.Fatal("unreachable control plane dropped working credentials")
	}
	if _, err := deferred.Put(ctx, "checkpoints/cp/manifest", strings.NewReader("x"), 1, ""); err != nil {
		t.Fatalf("Put during outage: %v", err)
	}
	// An invalid response body is rejected rather than trusted.
	fake.setMode("invalid")
	if _, err := fetchOnce(ctx, loop, controlPlane); err == nil {
		t.Fatal("malformed credential response accepted")
	}
}

func mustCredentials(t *testing.T, loop *hostedStorageLoop, controlPlane *httptest.Server) objectstore.STSCredentials {
	t.Helper()
	credentials, err := fetchOnce(context.Background(), loop, controlPlane)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}
