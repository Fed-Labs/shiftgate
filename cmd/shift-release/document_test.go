package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shift.dev/shift/internal/update"
)

// writeBuildOutput drops one fake build output into a directory. The fake is a
// runnable script that answers --version the way the real binaries do —
// "name version" on stdout — so the document's version binding exercises the
// real execution path. The content marker rides in a comment so distinct fakes
// still have distinct digests.
func writeBuildOutput(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	script := "#!/bin/sh\n# " + content + "\necho \"" + name + " 0.2.0\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeVersionOutput drops a fake binary whose self-reported version is chosen
// by the test, and which answers only the argument it is given.
func writeVersionOutput(t *testing.T, directory, name, selfReported string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ] || [ \"$1\" = \"version\" ]; then echo \"" + name + " " + selfReported + "\"; exit 0; fi\nexit 64\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeTarball packs the named files of a directory into a gzipped tar and
// writes the installer-style .sha256 sidecar next to it.
func writeTarball(t *testing.T, sourceDir, tarball string, names ...string) {
	t.Helper()
	file, err := os.Create(tarball)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	writer := tar.NewWriter(gz)
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	digest, err := fileDigest(tarball)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tarball+".sha256", []byte(digest+"  "+filepath.Base(tarball)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// signAndFeed signs a generated release document and assembles the feed, the
// pipeline's document→sign→feed sequence in miniature.
func signAndFeed(t *testing.T, directory, releasePath string) (feedPath, privatePath, publicPath string) {
	t.Helper()
	privatePath = filepath.Join(directory, "release.key")
	publicPath = filepath.Join(directory, "release.pub")
	var output bytes.Buffer
	if err := run([]string{"keygen", "--private-key", privatePath, "--public-key", publicPath, "--comment", "release"}, &output); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	signedPath := filepath.Join(directory, "signed.json")
	if err := run([]string{"sign", "--key", privatePath, "--release", releasePath, "--out", signedPath}, &output); err != nil {
		t.Fatalf("sign: %v", err)
	}
	feedPath = filepath.Join(directory, "feed.json")
	if err := run([]string{"feed", "--channel", "stable", "--out", feedPath, signedPath}, &output); err != nil {
		t.Fatalf("feed: %v", err)
	}
	return feedPath, privatePath, publicPath
}

// keysArray renders one public key as a one-element trusted-keys array.
func keysArray(t *testing.T, publicPath, comment string) string {
	t.Helper()
	var output bytes.Buffer
	if err := run([]string{"trusted-key", "--public-key", publicPath, "--comment", comment}, &output); err != nil {
		t.Fatalf("trusted-key: %v", err)
	}
	keysPath := filepath.Join(filepath.Dir(publicPath), "keys-"+comment+".json")
	if err := os.WriteFile(keysPath, []byte(output.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return keysPath
}

func TestDocumentMeasuresBuildOutputs(t *testing.T) {
	directory := t.TempDir()
	agentPath := writeBuildOutput(t, directory, "shift-agent", "agent-bytes")
	cliPath := writeBuildOutput(t, directory, "shiftgate", "cli-bytes")
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	err := run([]string{
		"document", "--version", "v0.2.0", "--channel", "stable", "--out", releasePath,
		"--base-url", "https://releases.example/v0.2.0",
		"--artifact", "agent=" + agentPath,
		"--artifact", "cli=" + cliPath,
	}, &output)
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	var release update.Release
	if err := json.Unmarshal(mustRead(t, releasePath), &release); err != nil {
		t.Fatal(err)
	}
	if release.Version != "0.2.0" {
		t.Fatalf("version was not normalized: %q", release.Version)
	}
	if len(release.Artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(release.Artifacts))
	}
	for _, artifact := range release.Artifacts {
		agentDiagnostics, err := fileDigest(agentPath)
		if err != nil {
			t.Fatal(err)
		}
		expected := agentDiagnostics
		if artifact.Component == update.ComponentCLI {
			if expected, err = fileDigest(cliPath); err != nil {
				t.Fatal(err)
			}
		}
		if artifact.SHA256 != expected || artifact.BinarySHA256 != expected {
			t.Fatalf("artifact %s digests %s/%s do not match the built bytes %s", artifact.Component, artifact.SHA256, artifact.BinarySHA256, expected)
		}
		if artifact.SizeBytes <= 0 {
			t.Fatalf("artifact %s recorded no size", artifact.Component)
		}
		if !strings.HasPrefix(artifact.URL, "https://releases.example/v0.2.0/") {
			t.Fatalf("artifact url %q does not sit under the base url", artifact.URL)
		}
	}
	if release.ProtocolVersion <= 0 || release.ConfigVersion <= 0 || release.StateVersion <= 0 {
		t.Fatalf("schema versions were not filled from the build: %+v", release)
	}
}

func TestVerifyBindsDigestsToBuildOutputsAndTarball(t *testing.T) {
	directory := t.TempDir()
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBuildOutput(t, binDir, "shift-agent", "agent-bytes-v2")
	writeBuildOutput(t, binDir, "shiftgate", "cli-bytes-v2")
	writeBuildOutput(t, binDir, "shift-control", "control-bytes-v2")
	distDir := filepath.Join(directory, "dist")
	if err := os.Mkdir(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(distDir, "shiftgate-linux-amd64.tar.gz")
	writeTarball(t, binDir, tarball, "shiftgate", "shift-agent", "shift-control")

	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	if err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--base-url", "https://releases.example/v0.2.0",
		"--artifact", "agent=" + filepath.Join(binDir, "shift-agent"),
		"--artifact", "cli=" + filepath.Join(binDir, "shiftgate"),
	}, &output); err != nil {
		t.Fatalf("document: %v", err)
	}
	feedPath, _, publicPath := signAndFeed(t, directory, releasePath)
	keysPath := keysArray(t, publicPath, "ceremony")

	if err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath, "--artifacts", binDir, "--tarball", tarball}, &output); err != nil {
		t.Fatalf("verify must bind signed digests to the build outputs and tarball: %v", err)
	}
	if !strings.Contains(output.String(), "digests") {
		t.Fatalf("verify must report digest binding: %s", output.String())
	}

	// A tarball rebuilt from different bytes is a different release: the
	// digest binding must refuse it even though every signature still holds.
	writeBuildOutput(t, binDir, "shift-agent", "substituted-agent-bytes")
	writeTarball(t, binDir, tarball, "shiftgate", "shift-agent", "shift-control")
	if err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath, "--tarball", tarball}, &output); err == nil {
		t.Fatal("verify must refuse a tarball whose bytes do not match the signed digests")
	}
}

func TestVerifyRefusesTamperedBuildOutput(t *testing.T) {
	directory := t.TempDir()
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentPath := writeBuildOutput(t, binDir, "shift-agent", "agent-bytes")
	cliPath := writeBuildOutput(t, binDir, "shiftgate", "cli-bytes")
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	if err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--artifact", "agent=" + agentPath,
		"--artifact", "cli=" + cliPath,
	}, &output); err != nil {
		t.Fatalf("document: %v", err)
	}
	feedPath, _, publicPath := signAndFeed(t, directory, releasePath)
	keysPath := keysArray(t, publicPath, "ceremony")

	writeBuildOutput(t, binDir, "shift-agent", "rebuilt-differently")
	if err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath, "--artifacts", binDir}, &output); err == nil {
		t.Fatal("verify must refuse build outputs whose digests differ from the signed ones")
	}
}

func TestVerifyChecksTheTarballSidecar(t *testing.T) {
	directory := t.TempDir()
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBuildOutput(t, binDir, "shift-agent", "agent-bytes")
	writeBuildOutput(t, binDir, "shiftgate", "cli-bytes")
	distDir := filepath.Join(directory, "dist")
	if err := os.Mkdir(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(distDir, "shiftgate-linux-amd64.tar.gz")
	writeTarball(t, binDir, tarball, "shiftgate", "shift-agent")
	// Poison the sidecar the installer would check.
	if err := os.WriteFile(tarball+".sha256", []byte(strings.Repeat("a", 64)+"  shiftgate-linux-amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	if err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--artifact", "agent=" + filepath.Join(binDir, "shift-agent"),
		"--artifact", "cli=" + filepath.Join(binDir, "shiftgate"),
	}, &output); err != nil {
		t.Fatalf("document: %v", err)
	}
	feedPath, _, publicPath := signAndFeed(t, directory, releasePath)
	keysPath := keysArray(t, publicPath, "ceremony")
	if err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath, "--tarball", tarball}, &output); err == nil {
		t.Fatal("verify must refuse a tarball whose published .sha256 sidecar does not match it")
	}
}

func TestDocumentRefusesAVersionTheBinariesDoNotReport(t *testing.T) {
	directory := t.TempDir()
	agentPath := writeVersionOutput(t, directory, "shift-agent", "9.9.9")
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--artifact", "agent=" + agentPath,
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "reports version 9.9.9") {
		t.Fatalf("document must refuse a --version no artifact reports, got %v", err)
	}
	if _, statErr := os.Stat(releasePath); statErr == nil {
		t.Fatal("no release document may be written for a mislabeled release")
	}
}

func TestDocumentBindsVersionToTheCLIVersionSubcommand(t *testing.T) {
	directory := t.TempDir()
	cliPath := filepath.Join(directory, "shiftgate")
	// shiftgate spells its self-report `shiftgate version`, not `--version`.
	script := "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo \"SHIFT 0.2.0\"; exit 0; fi\nexit 2\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--artifact", "cli=" + cliPath,
	}, &output)
	if err != nil {
		t.Fatalf("document must read the cli's version subcommand: %v", err)
	}
}

func TestVerifyRefusesASignedReleaseWhoseBinariesReportAnotherVersion(t *testing.T) {
	directory := t.TempDir()
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentPath := writeVersionOutput(t, binDir, "shift-agent", "9.9.9")
	// Hand-write the release document the way a pre-binding shift-release
	// would have: digests match the shipped bytes, but the version label is
	// not what the binary reports. verify must refuse it.
	digest, err := fileDigest(agentPath)
	if err != nil {
		t.Fatal(err)
	}
	release := releaseDocument("0.2.0", "https://releases.example/shift-agent")
	release.Artifacts = []update.Artifact{{
		Component: update.ComponentAgent, OS: "linux", Architecture: "amd64",
		URL: "https://releases.example/shift-agent", Format: update.FormatBinary,
		SizeBytes: int64(len(mustRead(t, agentPath))), SHA256: digest, BinarySHA256: digest,
	}}
	releasePath := filepath.Join(directory, "release.json")
	writeJSONFile(t, releasePath, release)
	feedPath, _, publicPath := signAndFeed(t, directory, releasePath)
	keysPath := keysArray(t, publicPath, "ceremony")

	var output bytes.Buffer
	err = run([]string{"verify", "--feed", feedPath, "--keys", keysPath, "--artifacts", binDir}, &output)
	if err == nil || !strings.Contains(err.Error(), "reports version 9.9.9") {
		t.Fatalf("verify must bind the signed version to the binary's self-report, got %v", err)
	}
}

func TestVerifyFailsLoudlyWhenTheTarballSidecarIsMissing(t *testing.T) {
	directory := t.TempDir()
	binDir := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBuildOutput(t, binDir, "shift-agent", "agent-bytes")
	writeBuildOutput(t, binDir, "shiftgate", "cli-bytes")
	distDir := filepath.Join(directory, "dist")
	if err := os.Mkdir(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := filepath.Join(distDir, "shiftgate-linux-amd64.tar.gz")
	writeTarball(t, binDir, tarball, "shiftgate", "shift-agent")
	if err := os.Remove(tarball + ".sha256"); err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	if err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--artifact", "agent=" + filepath.Join(binDir, "shift-agent"),
		"--artifact", "cli=" + filepath.Join(binDir, "shiftgate"),
	}, &output); err != nil {
		t.Fatalf("document: %v", err)
	}
	feedPath, _, publicPath := signAndFeed(t, directory, releasePath)
	keysPath := keysArray(t, publicPath, "ceremony")
	err := run([]string{"verify", "--feed", feedPath, "--keys", keysPath, "--tarball", tarball}, &output)
	if err == nil || !strings.Contains(err.Error(), ".sha256") {
		t.Fatalf("verify --tarball must fail loudly when the sidecar is missing, got %v", err)
	}
}

func TestKeysStatusDistinguishesPlaceholderFromConfigured(t *testing.T) {
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "key.pem")
	publicPath := filepath.Join(directory, "key.pub.pem")
	var output bytes.Buffer
	if err := run([]string{"keygen", "--private-key", privatePath, "--public-key", publicPath, "--comment", "PLACEHOLDER — not a real anchor"}, &output); err != nil {
		t.Fatalf("keygen: %v", err)
	}
	placeholderPath := keysArray(t, publicPath, "PLACEHOLDER-anchor")
	configuredPath := keysArray(t, publicPath, "release-2026-10")

	output.Reset()
	if err := run([]string{"keys-status", "--keys", placeholderPath}, &output); err != nil {
		t.Fatalf("keys-status must read a placeholder file without erroring: %v", err)
	}
	if strings.TrimSpace(output.String()) != "placeholder" {
		t.Fatalf("expected placeholder status, got %q", output.String())
	}
	output.Reset()
	if err := run([]string{"keys-status", "--keys", configuredPath}, &output); err != nil {
		t.Fatalf("keys-status: %v", err)
	}
	if strings.TrimSpace(output.String()) != "configured" {
		t.Fatalf("expected configured status, got %q", output.String())
	}

	// A placeholder-only ring never verifies anything: fail closed with a
	// ceremony hint instead of a bare signature mismatch.
	releasePath := filepath.Join(directory, "release.json")
	writeJSONFile(t, releasePath, releaseDocument("1.2.3", "https://releases.example/agent"))
	signedPath := filepath.Join(directory, "signed.json")
	if err := run([]string{"sign", "--key", privatePath, "--release", releasePath, "--out", signedPath}, &output); err != nil {
		t.Fatalf("sign: %v", err)
	}
	feedPath := filepath.Join(directory, "feed.json")
	if err := run([]string{"feed", "--channel", "stable", "--out", feedPath, signedPath}, &output); err != nil {
		t.Fatalf("feed: %v", err)
	}
	err := run([]string{"verify", "--feed", feedPath, "--keys", placeholderPath}, &output)
	if err == nil || !strings.Contains(err.Error(), "key ceremony") {
		t.Fatalf("verify must fail closed on a placeholder-only key file with a ceremony hint, got %v", err)
	}
	if err := run([]string{"verify", "--feed", feedPath, "--keys", configuredPath}, &output); err != nil {
		t.Fatalf("verify must accept the configured key ring: %v", err)
	}
}

func TestVerifyAcceptsASingleTrustedKeyObject(t *testing.T) {
	directory := t.TempDir()
	signedPath, _ := publish(t, directory, "2.5.0")
	// The documented keys shape is an array, but a single object is what
	// older ceremonies wrote; it must still load.
	var output bytes.Buffer
	if err := run([]string{"keygen", "--private-key", filepath.Join(directory, "k.pem"), "--public-key", filepath.Join(directory, "k.pub"), "--comment", "x"}, &output); err != nil {
		t.Fatal(err)
	}
	entries := strings.Index(output.String(), "[")
	var keys []update.TrustedKey
	if err := json.Unmarshal([]byte(output.String()[entries:]), &keys); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(directory, "keys-object.json")
	encoded, err := json.Marshal(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	// This ring does not hold the publishing key, so verification fails on
	// the threshold — but the file itself must parse.
	feedPath := filepath.Join(directory, "feed.json")
	if err := run([]string{"feed", "--channel", "stable", "--out", feedPath, signedPath}, &output); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"verify", "--feed", feedPath, "--keys", objectPath}, &output)
	if err == nil {
		t.Fatal("verify must not trust a release signed by a key outside the ring")
	}
	if strings.Contains(err.Error(), "trusted-key") {
		t.Fatalf("the single-object keys file failed to parse: %v", err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// TestDocumentRefusesToRelabelAnArtifactForAnotherArchitecture: the platform
// label is not a guess — an artifact whose own header names another
// architecture than the recorded one is refused with the flags to fix it.
func TestDocumentRefusesToRelabelAnArtifactForAnotherArchitecture(t *testing.T) {
	directory := t.TempDir()
	elf := make([]byte, 64)
	elf[0], elf[1], elf[2], elf[3] = 0x7f, 'E', 'L', 'F'
	elf[4] = 2                // 64-bit
	elf[5] = 1                // little-endian
	elf[18], elf[19] = 183, 0 // e_machine = EM_AARCH64
	agentPath := filepath.Join(directory, "shift-agent")
	if err := os.WriteFile(agentPath, elf, 0o755); err != nil {
		t.Fatal(err)
	}
	releasePath := filepath.Join(directory, "release.json")
	var output bytes.Buffer
	err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--arch", "amd64",
		"--artifact", "agent=" + agentPath,
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Fatalf("document must refuse to relabel an arm64 artifact as amd64, got %v", err)
	}

	// With the matching flags the same file is accepted.
	output.Reset()
	if err := run([]string{
		"document", "--version", "0.2.0", "--out", releasePath,
		"--os", "linux", "--arch", "arm64",
		"--artifact", "agent=" + agentPath,
	}, &output); err == nil {
		// The version probe fails on a header-only stub; that later failure
		// (not the platform one) is the expected outcome here.
		t.Fatal("a header-only stub must not pass the version binding")
	} else if strings.Contains(err.Error(), "relabel") || strings.Contains(err.Error(), "recorded as") {
		t.Fatalf("the matching --arch must clear the platform check, got %v", err)
	}
}
