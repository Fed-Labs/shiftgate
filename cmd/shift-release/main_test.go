package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/persistence"
	"shift.dev/shift/internal/update"
)

// releaseDocument builds a minimal valid release for one platform.
func releaseDocument(version string, url string) update.Release {
	return update.Release{
		Version: version, Channel: update.ChannelStable, PublishedAt: time.Now().UTC(),
		ProtocolVersion: 1, MinimumProtocolVersion: 1, ConfigVersion: 1, StateVersion: 1,
		Artifacts: []update.Artifact{{
			Component: update.ComponentAgent, OS: "linux", Architecture: "amd64",
			URL: url, Format: update.FormatBinary, SizeBytes: 4,
			SHA256:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BinarySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

// publish signs a release with a freshly generated key pair and returns the
// paths of the signed release and the trusted-keys file.
func publish(t *testing.T, directory, version string) (signedPath, keysPath string) {
	t.Helper()
	privatePath := filepath.Join(directory, "signing-key.pem")
	publicPath := filepath.Join(directory, "signing-key.pub.pem")
	var output bytes.Buffer
	if err := run([]string{"keygen", "--private-key", privatePath, "--public-key", publicPath, "--comment", "publisher"}, &output); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	// The trusted-key entry the tool prints is the one a fleet configuration
	// carries; parse it back out of stdout.
	start := strings.Index(output.String(), "{")
	var entry update.TrustedKey
	if err := json.Unmarshal([]byte(output.String()[start:]), &entry); err != nil {
		t.Fatalf("parse trusted-key entry: %v", err)
	}
	releasePath := filepath.Join(directory, "release-"+version+".json")
	writeJSONFile(t, releasePath, releaseDocument(version, "https://releases.example/agent-"+version))
	signedPath = filepath.Join(directory, "signed-"+version+".json")
	if err := run([]string{"sign", "--key", privatePath, "--release", releasePath, "--out", signedPath}, &output); err != nil {
		t.Fatalf("sign: %v", err)
	}
	keysPath = filepath.Join(directory, "keys.json")
	// Every publish generates its own key pair, and the ring must contain
	// every key that signed the feed — entries accumulate across calls.
	var keys []update.TrustedKey
	if _, statErr := os.Stat(keysPath); statErr == nil {
		if err := persistence.ReadJSON(keysPath, &keys); err != nil {
			t.Fatalf("read accumulated trusted keys: %v", err)
		}
	}
	writeJSONFile(t, keysPath, append(keys, entry))
	return signedPath, keysPath
}

func TestKeygenProducesALoadableSigningKey(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "key.pem")
	publicPath := filepath.Join(directory, "key.pub.pem")
	var output bytes.Buffer
	if err := run([]string{"keygen", "--private-key", privatePath, "--public-key", publicPath}, &output); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	info, err := os.Stat(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the private key must be owner-only, got %o", info.Mode().Perm())
	}
	privatePEM, err := os.ReadFile(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := update.NewSigner(string(privatePEM))
	if err != nil {
		t.Fatalf("the generated key must load as a release signer: %v", err)
	}
	publicPEM, err := os.ReadFile(publicPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := update.TrustedKeyID(string(publicPEM))
	if err != nil {
		t.Fatal(err)
	}
	if id != signer.KeyID() {
		t.Fatalf("the public key id %s does not match the signer id %s", id, signer.KeyID())
	}
}

func TestFeedAndVerifyRoundTrip(t *testing.T) {
	directory := t.TempDir()
	signedOne, keysPath := publish(t, directory, "1.0.0")
	signedTwo, _ := publish(t, directory, "1.1.0")
	feedPath := filepath.Join(directory, "feed.json")
	var output bytes.Buffer
	// Flags come before the positional release paths: the standard library's
	// flag parsing stops at the first non-flag argument.
	if err := run([]string{"feed", "--channel", "stable", "--out", feedPath, "--revoke", "1.0.0", signedOne, signedTwo}, &output); err != nil {
		t.Fatalf("feed: %v", err)
	}
	var document update.Feed
	if err := persistence.ReadJSON(feedPath, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Releases) != 2 || len(document.Revoked) != 1 {
		t.Fatalf("unexpected feed contents: %d releases, %d revoked", len(document.Releases), len(document.Revoked))
	}
	if err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath}, &output); err != nil {
		t.Fatalf("verify must accept the feed the tool itself built: %v", err)
	}
	if !strings.Contains(output.String(), "All 2 release(s)") {
		t.Fatalf("unexpected verify output: %s", output.String())
	}
}

func TestVerifyRefusesATamperedRelease(t *testing.T) {
	directory := t.TempDir()
	signedPath, keysPath := publish(t, directory, "2.0.0")
	var signed update.SignedRelease
	if err := persistence.ReadJSON(signedPath, &signed); err != nil {
		t.Fatal(err)
	}
	// A release edited after signing must not verify, which is the whole point
	// of the signature: the bytes are what was signed or nothing.
	signed.Release.Notes = "quietly changed"
	tamperedPath := filepath.Join(directory, "tampered.json")
	writeJSONFile(t, tamperedPath, signed)
	feedPath := filepath.Join(directory, "feed.json")
	var output bytes.Buffer
	if err := run([]string{"feed", "--channel", "stable", "--out", feedPath, tamperedPath}, &output); err != nil {
		t.Fatalf("feed: %v", err)
	}
	if err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath}, &output); err == nil {
		t.Fatal("verify must refuse a feed whose release was edited after signing")
	}
}

func TestFeedRefusesAReleaseFromAnotherChannel(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "key.pem")
	publicPath := filepath.Join(directory, "key.pub.pem")
	var output bytes.Buffer
	if err := run([]string{"keygen", "--private-key", privatePath, "--public-key", publicPath}, &output); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	release := releaseDocument("3.0.0", "https://releases.example/agent-3.0.0")
	release.Channel = update.ChannelBeta
	releasePath := filepath.Join(directory, "release.json")
	writeJSONFile(t, releasePath, release)
	signedPath := filepath.Join(directory, "signed.json")
	if err := run([]string{"sign", "--key", privatePath, "--release", releasePath, "--out", signedPath}, &output); err != nil {
		t.Fatalf("sign: %v", err)
	}
	feedPath := filepath.Join(directory, "feed.json")
	if err := run([]string{"feed", "--channel", "stable", "--out", feedPath, signedPath}, &output); err == nil {
		t.Fatal("a stable feed must refuse to carry a beta release")
	}
}

func TestUsageIsReportedWithoutArguments(t *testing.T) {
	var output bytes.Buffer
	if err := run(nil, &output); err == nil {
		t.Fatal("running with no command must fail")
	}
	if err := run([]string{"nonsense"}, &output); err == nil {
		t.Fatal("an unknown command must fail")
	}
}
