package update

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testNow is a fixed clock so rollout stages and history timestamps are
// reproducible.
var testNow = time.Date(2026, time.March, 4, 9, 0, 0, 0, time.UTC)

func fixedClock() func() time.Time {
	return func() time.Time { return testNow }
}

// newSigner generates a signing key in memory. Nothing is written to disk and no
// key material is printed.
func newSigner(t *testing.T) *Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatalf("marshal signing key: %v", err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	signer, err := NewSigner(string(encoded))
	if err != nil {
		t.Fatalf("load signing key: %v", err)
	}
	return signer
}

func trustedKeyFor(t *testing.T, signer *Signer) TrustedKey {
	t.Helper()
	publicPEM, err := signer.PublicKeyPEM()
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	return TrustedKey{ID: signer.KeyID(), PublicKeyPEM: publicPEM, Comment: "test signer"}
}

func keyRingFor(t *testing.T, threshold int, signers ...*Signer) *KeyRing {
	t.Helper()
	keys := make([]TrustedKey, 0, len(signers))
	for _, signer := range signers {
		keys = append(keys, trustedKeyFor(t, signer))
	}
	ring, err := NewKeyRing(keys, threshold, testNow)
	if err != nil {
		t.Fatalf("build key ring: %v", err)
	}
	return ring
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func gzipped(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("compress payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close compressor: %v", err)
	}
	return buffer.Bytes()
}

// versionScript is a real executable that answers --version, which is what the
// installer's self-check runs. Using a script rather than a compiled binary keeps
// the test fast while exercising the same execution path.
func versionScript(component, version string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"" + component + " " + version + "\"; exit 0; fi\nexit 64\n")
}

// brokenScript exits non-zero however it is called, standing in for a release
// that does not run on this machine.
func brokenScript() []byte {
	return []byte("#!/bin/sh\nexit 1\n")
}

// writeExecutable installs a payload as an executable file, the way a previous
// version of SHIFT would already be present on a machine.
func writeExecutable(t *testing.T, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, payload, 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// stubFetcher serves artifact payloads from memory, keyed by URL. It enforces the
// same size contract the HTTP fetcher does so tests exercise real verification.
type stubFetcher struct {
	payloads map[string][]byte
	calls    int
	failWith error
}

func newStubFetcher() *stubFetcher {
	return &stubFetcher{payloads: make(map[string][]byte)}
}

func (fetcher *stubFetcher) add(url string, payload []byte) {
	fetcher.payloads[url] = payload
}

func (fetcher *stubFetcher) Fetch(ctx context.Context, artifact Artifact, into io.Writer) error {
	fetcher.calls++
	if fetcher.failWith != nil {
		return fetcher.failWith
	}
	payload, found := fetcher.payloads[artifact.URL]
	if !found {
		return fmt.Errorf("no payload registered for %s", artifact.URL)
	}
	if int64(len(payload)) != artifact.SizeBytes {
		return fmt.Errorf("artifact is %d bytes, the release records %d", len(payload), artifact.SizeBytes)
	}
	if _, err := into.Write(payload); err != nil {
		return err
	}
	return ctx.Err()
}

// feedFunc adapts a function to the FeedSource interface.
type feedFunc func(ctx context.Context) (Feed, error)

func (source feedFunc) Feed(ctx context.Context) (Feed, error) {
	return source(ctx)
}

// binaryArtifact describes an uncompressed executable payload.
func binaryArtifact(component Component, url string, payload []byte) Artifact {
	digest := digestOf(payload)
	return Artifact{
		Component:    component,
		OS:           "linux",
		Architecture: "amd64",
		URL:          url,
		Format:       FormatBinary,
		SizeBytes:    int64(len(payload)),
		SHA256:       digest,
		BinarySHA256: digest,
	}
}

// gzipArtifact describes a gzip-compressed executable payload.
func gzipArtifact(t *testing.T, component Component, url string, payload []byte) (Artifact, []byte) {
	t.Helper()
	compressed := gzipped(t, payload)
	return Artifact{
		Component:    component,
		OS:           "linux",
		Architecture: "amd64",
		URL:          url,
		Format:       FormatGzip,
		SizeBytes:    int64(len(compressed)),
		SHA256:       digestOf(compressed),
		BinarySHA256: digestOf(payload),
	}, compressed
}

// releaseFor builds a valid release around the supplied artifacts.
func releaseFor(version string, artifacts ...Artifact) Release {
	return Release{
		Version:                version,
		Channel:                ChannelStable,
		PublishedAt:            testNow.Add(-time.Hour),
		Notes:                  "test release " + version,
		ProtocolVersion:        1,
		MinimumProtocolVersion: 1,
		ConfigVersion:          1,
		StateVersion:           1,
		Artifacts:              artifacts,
	}
}

func signedFeed(t *testing.T, signer *Signer, releases ...Release) Feed {
	t.Helper()
	feed := Feed{Version: feedSchemaVersion, Channel: ChannelStable, GeneratedAt: testNow}
	for _, release := range releases {
		signed, err := signer.SignRelease(release)
		if err != nil {
			t.Fatalf("sign release %s: %v", release.Version, err)
		}
		feed.Releases = append(feed.Releases, signed)
	}
	return feed
}

// installationFor describes a machine running the given version of a component.
func installationFor(component Component, version, executablePath string) Installation {
	return Installation{
		Component:       component,
		Version:         version,
		Channel:         ChannelStable,
		OS:              "linux",
		Architecture:    "amd64",
		MachineID:       "machine-under-test",
		ExecutablePath:  executablePath,
		ProtocolVersion: 1,
		ConfigVersion:   1,
		StateVersion:    1,
	}
}

// decodeJSON and encodeJSON keep the state-file assertions in the tests free of
// their own JSON plumbing.
func decodeJSON(content []byte, into any) error {
	return json.Unmarshal(content, into)
}

func encodeJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

// TestFileFetcherDeliversSignedBytes covers the air-gapped artifact path: the
// bytes come from disk, but the size contract and the digest checks downstream
// are the same ones the networked path meets.
func TestFileFetcherDeliversSignedBytes(t *testing.T) {
	directory := t.TempDir()
	payload := versionScript("shift-agent", "1.2.3")
	artifactPath := filepath.Join(directory, "agent")
	writeExecutable(t, artifactPath, payload)
	artifact := Artifact{
		Component: ComponentAgent, OS: "linux", Architecture: "amd64",
		URL: "file://" + artifactPath, Format: FormatBinary,
		SizeBytes: int64(len(payload)), SHA256: digestOf(payload), BinarySHA256: digestOf(payload),
	}
	var delivered bytes.Buffer
	if err := NewFileFetcher().Fetch(context.Background(), artifact, &delivered); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(delivered.Bytes(), payload) {
		t.Fatal("the fetched bytes differ from the artifact on disk")
	}

	artifact.SizeBytes++
	if err := NewFileFetcher().Fetch(context.Background(), artifact, &delivered); err == nil {
		t.Fatal("a size that disagrees with the signed release must be refused")
	}

	artifact.URL = "https://releases.example/agent"
	if err := NewFileFetcher().Fetch(context.Background(), artifact, io.Discard); err == nil {
		t.Fatal("an https url must not be read from disk")
	}

	artifact.URL = "file://" + filepath.Join(directory, "absent")
	if err := NewFileFetcher().Fetch(context.Background(), artifact, io.Discard); err == nil {
		t.Fatal("a missing artifact must be refused")
	}
}

// TestArtifactValidationAcceptsFileURLs pins the artifact-url rule: https for a
// networked feed, an absolute file url for an air-gapped one, nothing else.
func TestArtifactValidationAcceptsFileURLs(t *testing.T) {
	payload := versionScript("shift-agent", "1.2.3")
	base := Artifact{
		Component: ComponentAgent, OS: "linux", Architecture: "amd64", Format: FormatBinary,
		SizeBytes: int64(len(payload)), SHA256: digestOf(payload), BinarySHA256: digestOf(payload),
	}
	for name, url := range map[string]string{
		"file url":       "file:///srv/shift/artifacts/agent",
		"https url":      "https://releases.example/agent",
		"file with host": "file://host/artifacts/agent",
		"relative file":  "file:artifacts/agent",
		"bare path":      "/srv/shift/artifacts/agent",
		"http url":       "http://releases.example/agent",
	} {
		artifact := base
		artifact.URL = url
		err := artifact.Validate()
		accepted := name == "file url" || name == "https url"
		if accepted && err != nil {
			t.Fatalf("%s must be accepted: %v", name, err)
		}
		if !accepted && err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}
