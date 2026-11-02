package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/persistence"
	"shift.dev/shift/internal/update"
)

// document.go: release document generation from real build outputs, digest
// binding of a signed release back to those outputs, and trusted-key status.
//
// A release document is never written by hand: `document` reads the binaries
// the build produced, records each one's size and SHA-256, and fills the
// protocol/config/state schema versions this build speaks. The signing step
// then covers exactly those digests, and `verify --artifacts/--tarball`
// re-derives them from the shipped bytes, so a green verify means the signed
// digests match the tarball every installer will download.

// artifactFlag collects a repeatable `component=path` flag.
type artifactFlag []struct{ component, path string }

func (values *artifactFlag) String() string { return fmt.Sprint(*values) }

func (values *artifactFlag) Set(value string) error {
	component, path, found := strings.Cut(value, "=")
	if !found || component == "" || path == "" {
		return fmt.Errorf("artifact must be COMPONENT=PATH, got %q", value)
	}
	*values = append(*values, struct{ component, path string }{component, path})
	return nil
}

// document generates a release document from build outputs.
func document(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("document", flag.ContinueOnError)
	version := flags.String("version", "", "release version (for example 0.2.0)")
	channel := flags.String("channel", string(update.ChannelStable), "release channel")
	outputPath := flags.String("out", "", "where to write the release document")
	baseURL := flags.String("base-url", "", "URL prefix each artifact is published under; artifacts get a file:// URL when omitted")
	notes := flags.String("notes", "", "release notes recorded in the document")
	minimumFrom := flags.String("minimum-from", "", "oldest version that may upgrade directly to this one")
	mandatory := flags.Bool("mandatory", false, "mark the release mandatory for the mandatory install policy")
	publishedAt := flags.String("published-at", "", "publication time (RFC3339; defaults to now)")
	operatingSystem := flags.String("os", runtime.GOOS, "operating system the artifacts were built for")
	architecture := flags.String("arch", runtime.GOARCH, "architecture the artifacts were built for")
	artifacts := artifactFlag{}
	flags.Var(&artifacts, "artifact", "artifact to record as COMPONENT=PATH; repeatable (components: agent, cli, desktop, control, gc, verify, relay)")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *version == "" || *outputPath == "" {
		return errors.New("document requires --version and --out")
	}
	if len(artifacts) == 0 {
		return errors.New("document requires at least one --artifact COMPONENT=PATH")
	}
	label, err := update.ParseVersion(*version)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	published := time.Now().UTC()
	if *publishedAt != "" {
		parsed, err := time.Parse(time.RFC3339, *publishedAt)
		if err != nil {
			return fmt.Errorf("published-at: %w", err)
		}
		published = parsed.UTC()
	}
	release := update.Release{
		Version:                *version,
		Channel:                update.Channel(*channel),
		PublishedAt:            published,
		Notes:                  *notes,
		MinimumFromVersion:     *minimumFrom,
		Mandatory:              *mandatory,
		ProtocolVersion:        model.ProtocolVersion,
		MinimumProtocolVersion: model.MinimumProtocolVersion,
		ConfigVersion:          model.ConfigSchemaVersion,
		StateVersion:           model.StateSchemaVersion,
	}
	for _, entry := range artifacts {
		artifact, err := artifactFromPath(update.Component(entry.component), entry.path, *baseURL, *operatingSystem, *architecture)
		if err != nil {
			return err
		}
		release.Artifacts = append(release.Artifacts, artifact)
	}
	// The document's --version is bound to what the artifacts themselves
	// report: a release that promises a version no shipped binary answers
	// with is refused here, at publication time, instead of at install time
	// on every machine in the fleet.
	if err := bindArtifactVersions(context.Background(), release.Artifacts, label, artifacts, output); err != nil {
		return err
	}
	if err := release.Validate(); err != nil {
		return fmt.Errorf("generated release is not publishable: %w", err)
	}
	if err := persistence.WriteJSON(*outputPath, release, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(output, "Release document for %s with %d artifact(s) written to %s.\n", release.Version, len(release.Artifacts), *outputPath)
	for _, artifact := range release.Artifacts {
		_, _ = fmt.Fprintf(output, "  %s %s/%s sha256=%s size=%d\n", artifact.Component, artifact.OS, artifact.Architecture, artifact.SHA256, artifact.SizeBytes)
	}
	return nil
}

// bindArtifactVersions executes every artifact's version self-report and
// refuses unless each binary answers with the release version the document
// promises. The tooling's own build revision is compared to the artifacts'
// too: the schema pins in the document come from this build tree, so a
// document generated from a different revision than the artifacts would state
// schema numbers the binaries never spoke.
func bindArtifactVersions(ctx context.Context, artifacts []update.Artifact, label update.Version, paths artifactFlag, output io.Writer) error {
	toolRevision := toolingRevision()
	byComponent := make(map[update.Component]string, len(paths))
	for _, entry := range paths {
		byComponent[update.Component(entry.component)] = entry.path
	}
	for _, artifact := range artifacts {
		path := byComponent[artifact.Component]
		reported, err := update.ProbeVersion(ctx, path, string(artifact.Component))
		if err != nil {
			return fmt.Errorf("artifact %s: %w\nthe release document's --version can only be issued for artifacts that answer --version with it", artifact.Component, err)
		}
		if !reported.SameRelease(label) {
			return fmt.Errorf("artifact %s reports version %s but the document promises %s: bump the binaries' version (or re-tag) so the signed document and the shipped bytes agree",
				artifact.Component, reported, label)
		}
		if toolRevision != "" {
			if artifactRevision := buildRevisionOf(path); artifactRevision != "" && artifactRevision != toolRevision {
				return fmt.Errorf("artifact %s was built from revision %s but this tool runs from revision %s: generate the document from the release revision so the schema pins match the binaries",
					artifact.Component, artifactRevision, toolRevision)
			}
		}
	}
	if toolRevision == "" {
		_, _ = fmt.Fprintln(output, "note   tooling build revision is unstamped; same-revision schema pins could not be cross-checked")
	}
	return nil
}

// toolingRevision is the git revision this shift-release binary was built
// from, when the Go toolchain stamped one.
func toolingRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

// buildRevisionOf reads the git revision one Go artifact was built from. A
// non-Go artifact (a test script, a packaged stub) carries none, which is
// reported as empty rather than guessed at.
func buildRevisionOf(path string) string {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

// artifactFromPath measures one build output and records it as a release
// artifact. The digests are computed here, from the bytes on disk, so the
// document can never claim a digest nobody checked. The platform label comes
// from the operator's flags (defaulting to the host's own) and is cross-checked
// against the binary's own format where one can be read: an artifact is never
// silently relabeled as some other platform.
func artifactFromPath(component update.Component, filePath, baseURL, operatingSystem, architecture string) (update.Artifact, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return update.Artifact{}, fmt.Errorf("artifact %s: %w", filePath, err)
	}
	if !info.Mode().IsRegular() {
		return update.Artifact{}, fmt.Errorf("artifact %s is not a regular file", filePath)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return update.Artifact{}, err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return update.Artifact{}, fmt.Errorf("artifact %s: %w", filePath, err)
	}
	sum := hex.EncodeToString(digest.Sum(nil))
	if detected, ok := elfArchitecture(filePath); ok && detected != strings.ToLower(strings.TrimSpace(architecture)) {
		return update.Artifact{}, fmt.Errorf("artifact %s is built for %s but was recorded as %s; pass --arch %s (or the matching --os/--arch) instead of relabeling it",
			filePath, detected, architecture, detected)
	}
	location := ""
	if baseURL != "" {
		location = strings.TrimRight(baseURL, "/") + "/" + filepath.Base(filePath)
	} else {
		absolute, err := filepath.Abs(filePath)
		if err != nil {
			return update.Artifact{}, err
		}
		location = (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
	}
	return update.Artifact{
		Component:    component,
		OS:           strings.ToLower(strings.TrimSpace(operatingSystem)),
		Architecture: strings.ToLower(strings.TrimSpace(architecture)),
		URL:          location,
		Format:       update.FormatBinary,
		SizeBytes:    info.Size(),
		SHA256:       sum,
		BinarySHA256: sum,
	}, nil
}

// elfArchitecture reads an ELF header's machine type and maps it to a Go
// architecture name. It reports false for anything that is not an ELF binary,
// which is where no format-level guess is possible and the operator's flags
// stand alone.
func elfArchitecture(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return "", false
	}
	if header[0] != 0x7f || header[1] != 'E' || header[2] != 'L' || header[3] != 'F' {
		return "", false
	}
	machine := binary.LittleEndian.Uint16(header[18:20])
	if header[5] == 2 {
		machine = binary.BigEndian.Uint16(header[18:20])
	}
	switch machine {
	case 3:
		return "386", true
	case 40:
		return "arm", true
	case 62:
		return "amd64", true
	case 183:
		return "arm64", true
	case 243:
		if header[4] == 2 {
			return "riscv64", true
		}
		return "riscv", true
	default:
		return fmt.Sprintf("em-%d", machine), true
	}
}

// keysStatus reports whether a trusted-keys file holds real trust anchors or
// only the checked-in placeholder. It exits successfully either way and prints
// one word; a malformed file is an error, so a pipeline can branch on the
// status without swallowing a broken keys file.
func keysStatus(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("keys-status", flag.ContinueOnError)
	keysPath := flags.String("keys", "", "path to the trusted-key JSON array")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *keysPath == "" {
		return errors.New("keys-status requires --keys")
	}
	keys, err := readTrustedKeys(*keysPath)
	if err != nil {
		return err
	}
	usable := 0
	for _, key := range keys {
		if !isPlaceholderKey(key) {
			usable++
		}
	}
	if usable == 0 {
		_, _ = fmt.Fprintln(output, "placeholder")
		return nil
	}
	_, _ = fmt.Fprintln(output, "configured")
	return nil
}

// placeholderComment marks the checked-in key entries that stand in for the
// real key ceremony. A comment carrying this marker is never a production
// trust anchor: no private key exists for it anywhere.
const placeholderComment = "PLACEHOLDER"

// isPlaceholderKey reports whether an entry is only ceremony scaffolding.
func isPlaceholderKey(key update.TrustedKey) bool {
	return strings.Contains(strings.ToUpper(key.Comment), placeholderComment)
}

// readTrustedKeys loads a trusted-keys file. The canonical shape is a JSON
// array of entries; a single JSON object is accepted too, because the keygen
// output is copied into files by hand and old copies float around.
func readTrustedKeys(path string) ([]update.TrustedKey, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(content))
	switch {
	case strings.HasPrefix(trimmed, "["):
		var keys []update.TrustedKey
		if err := unmarshalStrictJSON(content, &keys); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		return keys, nil
	case strings.HasPrefix(trimmed, "{"):
		var key update.TrustedKey
		if err := unmarshalStrictJSON(content, &key); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		return []update.TrustedKey{key}, nil
	default:
		return nil, fmt.Errorf("%s is neither a trusted-key array nor a trusted-key object", path)
	}
}

// bindDigests re-derives every artifact digest of a signed release from the
// bytes that actually ship: files in a directory (the build output) and the
// members of the release tarball. It is what makes a verified signature mean
// the shipped bytes are the signed bytes. The shipped executables are also
// run: each must answer --version with the release it is published under, so
// a green verify binds the signed version label to the binaries' own
// self-report and not just to their bytes.
func bindDigests(releases []update.SignedRelease, artifactsDir, tarball string) error {
	var files map[string]string
	if artifactsDir != "" {
		measured, err := measureDirectory(artifactsDir)
		if err != nil {
			return err
		}
		files = measured
	}
	var members map[string]string
	if tarball != "" {
		measured, err := measureTarball(tarball)
		if err != nil {
			return err
		}
		members = measured
		if err := checkTarballSidecar(tarball); err != nil {
			return err
		}
	}
	for _, signed := range releases {
		expected, err := signed.Release.SemanticVersion()
		if err != nil {
			return fmt.Errorf("release %s: %w", signed.Release.Version, err)
		}
		for _, artifact := range signed.Release.Artifacts {
			name := artifactName(artifact)
			bound := false
			if files != nil {
				digest, known := files[name]
				if !known {
					return fmt.Errorf("release %s: no file %q in %s to bind artifact %s/%s/%s to",
						signed.Release.Version, name, artifactsDir, artifact.Component, artifact.OS, artifact.Architecture)
				}
				if digest != artifact.SHA256 {
					return fmt.Errorf("release %s: %s digest %s does not match the signed digest %s",
						signed.Release.Version, name, digest, artifact.SHA256)
				}
				reported, probeErr := update.ProbeVersion(context.Background(), filepath.Join(artifactsDir, name), string(artifact.Component))
				if probeErr != nil {
					return fmt.Errorf("release %s: %s: %w", signed.Release.Version, name, probeErr)
				}
				if !reported.SameRelease(expected) {
					return fmt.Errorf("release %s: %s reports version %s, the signed release promises %s",
						signed.Release.Version, name, reported, expected)
				}
				bound = true
			}
			if members != nil {
				digest, known := members[name]
				if !known {
					return fmt.Errorf("release %s: no member %q in %s to bind artifact %s/%s/%s to",
						signed.Release.Version, name, tarball, artifact.Component, artifact.OS, artifact.Architecture)
				}
				if digest != artifact.SHA256 {
					return fmt.Errorf("release %s: tarball member %s digest %s does not match the signed digest %s",
						signed.Release.Version, name, digest, artifact.SHA256)
				}
				bound = true
			}
			if !bound {
				return errors.New("digest binding requires --artifacts, --tarball, or both")
			}
		}
	}
	return nil
}

// artifactName is the file name an artifact is published and bound under.
func artifactName(artifact update.Artifact) string {
	parsed, err := url.Parse(artifact.URL)
	if err != nil {
		return path.Base(artifact.URL)
	}
	return path.Base(parsed.Path)
}

// measureDirectory maps each regular file's base name to its SHA-256.
func measureDirectory(directory string) (map[string]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		digest, err := fileDigest(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		digests[entry.Name()] = digest
	}
	return digests, nil
}

// measureTarball maps each tar member's base name to the SHA-256 of its bytes.
func measureTarball(tarball string) (map[string]string, error) {
	file, err := os.Open(tarball)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", tarball, err)
	}
	defer func() { _ = gz.Close() }()
	digests := make(map[string]string)
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", tarball, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		digest := sha256.New()
		if _, err := io.Copy(digest, reader); err != nil {
			return nil, fmt.Errorf("read %s: %w", tarball, err)
		}
		name := path.Base(filepath.ToSlash(header.Name))
		digests[name] = hex.EncodeToString(digest.Sum(nil))
	}
	return digests, nil
}

// checkTarballSidecar verifies the tarball against its published .sha256
// sidecar — the same file the one-command installer checks. The sidecar is
// mandatory whenever a tarball is bound: without it, nothing the signature
// pins covers the tarball as a whole, and extra or shadowing members would
// ride along with a green verify.
func checkTarballSidecar(tarball string) error {
	sidecar := tarball + ".sha256"
	content, err := os.ReadFile(sidecar)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s is missing: a tarball can only be verified against its published .sha256 sidecar (run make dist, which writes it)", sidecar)
	}
	if err != nil {
		return err
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 {
		return fmt.Errorf("%s is malformed", sidecar)
	}
	actual, err := fileDigest(tarball)
	if err != nil {
		return err
	}
	if actual != strings.ToLower(fields[0]) {
		return fmt.Errorf("%s records digest %s but the tarball is %s", sidecar, fields[0], actual)
	}
	return nil
}

// fileDigest returns the SHA-256 of one file's bytes.
func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
