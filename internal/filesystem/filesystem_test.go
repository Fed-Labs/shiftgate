package filesystem

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeTreeEntry(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestProbeReportsTheKernelFilesystem(t *testing.T) {
	directory := t.TempDir()
	probed, err := Probe(directory)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probed.Name == "" {
		t.Fatal("probe must name the filesystem")
	}
}

func TestProbeRejectsMissingPath(t *testing.T) {
	if _, err := Probe(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("probing a missing path must fail")
	}
}

func TestDetectSnapshotProviderWithoutBtrfs(t *testing.T) {
	// ext4 and friends have no snapshot mechanism SHIFT can drive; the probe
	// result must say so rather than hand back a provider that cannot work.
	if provider := DetectSnapshotProvider(Filesystem{Name: "ext4"}); provider != nil {
		t.Fatalf("ext4 must not get a snapshot provider, got %v", provider)
	}
}

func TestIndexTreeStampsEveryFile(t *testing.T) {
	root := t.TempDir()
	writeTreeEntry(t, filepath.Join(root, "a.txt"), "alpha", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTreeEntry(t, filepath.Join(root, "sub", "b.txt"), "beta", 0o600)
	index, err := IndexTree(root, nil)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if len(index) != 3 {
		t.Fatalf("expected root, a.txt, and sub/b.txt, got %v", index)
	}
	if _, ok := index["a.txt"]; !ok {
		t.Fatal("index must key files by path relative to the root")
	}
	stamp := index["sub/b.txt"]
	if stamp.Mode&uint32(os.FileMode(0o600)) == 0 || stamp.Size != 4 {
		t.Fatalf("unexpected stamp for sub/b.txt: %+v", stamp)
	}
}

func TestIndexTreeHonorsExclusions(t *testing.T) {
	root := t.TempDir()
	writeTreeEntry(t, filepath.Join(root, "keep.txt"), "kept", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "cache", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTreeEntry(t, filepath.Join(root, "cache", "deep", "drop.txt"), "dropped", 0o644)
	writeTreeEntry(t, filepath.Join(root, "scratch.log"), "dropped", 0o644)
	index, err := IndexTree(root, []string{"cache", "*.log"})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	for path := range index {
		if path != "keep.txt" {
			t.Fatalf("exclusion was not applied: %q remained", path)
		}
	}
}

func TestDiffReportsChangedAddedAndDeleted(t *testing.T) {
	root := t.TempDir()
	writeTreeEntry(t, filepath.Join(root, "same.txt"), "same", 0o644)
	writeTreeEntry(t, filepath.Join(root, "gone.txt"), "old", 0o644)
	writeTreeEntry(t, filepath.Join(root, "edited.txt"), "before", 0o644)
	before, err := IndexTree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	writeTreeEntry(t, filepath.Join(root, "edited.txt"), "after — longer", 0o644)
	writeTreeEntry(t, filepath.Join(root, "new.txt"), "new", 0o644)
	after, err := IndexTree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	diff := Diff(before, after)
	if len(diff.Deleted) != 1 || diff.Deleted[0] != "gone.txt" {
		t.Fatalf("unexpected deletions: %v", diff.Deleted)
	}
	if len(diff.Added) != 1 || diff.Added[0] != "new.txt" {
		t.Fatalf("unexpected additions: %v", diff.Added)
	}
	if len(diff.Changed) != 1 || diff.Changed[0] != "edited.txt" {
		t.Fatalf("unexpected changes: %v", diff.Changed)
	}
	if diff.IsZero() {
		t.Fatal("a non-empty diff must not report zero")
	}
	if again := Diff(before, before); !again.IsZero() {
		t.Fatalf("diffing an index against itself must be empty: %+v", again)
	}
}

func TestIndexPersistenceRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeTreeEntry(t, filepath.Join(root, "a.txt"), "alpha", 0o644)
	index, err := IndexTree(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "index.json")
	if err := SaveIndex(path, index); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := LoadIndex(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != len(index) {
		t.Fatalf("round trip lost entries: %d then %d", len(index), len(loaded))
	}
	for path, stamp := range index {
		if loaded[path] != stamp {
			t.Fatalf("stamp for %s changed across the round trip", path)
		}
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the staged index file must not survive the rename")
	}
	missing, err := LoadIndex(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || missing != nil {
		t.Fatalf("a missing index must read as nil without error: %v %v", missing, err)
	}
}

func TestCloneTreeReproducesFilesLinksAndModes(t *testing.T) {
	source := t.TempDir()
	destination := filepath.Join(t.TempDir(), "clone")
	writeTreeEntry(t, filepath.Join(source, "plain.txt"), "content", 0o644)
	writeTreeEntry(t, filepath.Join(source, "secret.txt"), "content", 0o600)
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTreeEntry(t, filepath.Join(source, "nested", "inner.txt"), "inner", 0o640)
	if err := os.Symlink("plain.txt", filepath.Join(source, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(source, "plain.txt"), filepath.Join(source, "hard.txt")); err != nil {
		t.Fatal(err)
	}
	stats, err := CloneTree(source, destination)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "plain.txt"))
	if err != nil || string(content) != "content" {
		t.Fatalf("cloned file content mismatch: %q %v", content, err)
	}
	info, err := os.Lstat(filepath.Join(destination, "secret.txt"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions were not reproduced: %v %v", info.Mode(), err)
	}
	link, err := os.Readlink(filepath.Join(destination, "link.txt"))
	if err != nil || link != "plain.txt" {
		t.Fatalf("symlink was not reproduced: %q %v", link, err)
	}
	sourceInfo, _ := os.Lstat(filepath.Join(destination, "plain.txt"))
	clonedInfo, _ := os.Lstat(filepath.Join(destination, "hard.txt"))
	if !os.SameFile(sourceInfo, clonedInfo) {
		t.Fatal("hard link group was not preserved")
	}
	if stats.Files != 3 {
		t.Fatalf("expected 3 cloned regular files (plain, secret, inner), got %+v", stats)
	}
	if stats.ClonedFiles > stats.Files {
		t.Fatalf("cloned files cannot exceed copied files: %+v", stats)
	}
	if stats.Symlinks != 1 || stats.HardLinks != 1 || stats.Directories != 2 {
		t.Fatalf("unexpected clone stats: %+v", stats)
	}
	if _, err := CloneTree(source, destination); err == nil {
		t.Fatal("cloning onto an existing destination must fail")
	}
}

func TestCloneTreeSkipsUnixSockets(t *testing.T) {
	source := t.TempDir()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", filepath.Join(source, "app.sock"))
	if err != nil {
		t.Skipf("cannot create a unix socket here: %v", err)
	}
	defer listener.Close()
	destination := filepath.Join(t.TempDir(), "clone")
	stats, err := CloneTree(source, destination)
	if err != nil {
		t.Fatalf("clone with a socket present: %v", err)
	}
	if stats.SkippedSockets != 1 {
		t.Fatalf("the socket must be counted as skipped: %+v", stats)
	}
	if _, err := os.Lstat(filepath.Join(destination, "app.sock")); !os.IsNotExist(err) {
		t.Fatal("a dead socket endpoint must not be cloned")
	}
}

func TestDiscoverDependenciesOfAnInterpreterCommand(t *testing.T) {
	// /bin/sh exists everywhere Linux; the discovery must at minimum find the
	// binary and its ELF dependencies outside the workload root.
	discovery, err := DiscoverDependencies([]string{"/bin/sh", "-c", "true"}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(discovery.Dependencies) < 2 {
		t.Fatalf("a dynamic binary must report at least itself and its interpreter or libraries: %+v", discovery.Dependencies)
	}
	foundBinary := false
	for _, dependency := range discovery.Dependencies {
		if dependency.Path == "/bin/sh" && dependency.Kind == DependencyBinary && !dependency.InsideRoot {
			foundBinary = true
		}
	}
	if !foundBinary {
		t.Fatalf("the command binary itself must be a dependency: %+v", discovery.Dependencies)
	}
}

func TestDiscoverDependenciesMarksRootContents(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	discovery, err := DiscoverDependencies([]string{script}, root, nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(discovery.Dependencies) < 2 {
		t.Fatalf("a script must report itself and its interpreter: %+v", discovery.Dependencies)
	}
	scriptInside := false
	interpreterOutside := false
	for _, dependency := range discovery.Dependencies {
		switch dependency.Path {
		case script:
			scriptInside = dependency.InsideRoot
		case "/bin/sh":
			interpreterOutside = !dependency.InsideRoot
		}
	}
	if !scriptInside || !interpreterOutside {
		t.Fatalf("script must be inside the root and its interpreter outside: %+v", discovery.Dependencies)
	}
}

func TestDiscoverDependenciesReportsUnresolvedCommands(t *testing.T) {
	discovery, err := DiscoverDependencies([]string{"shift-definitely-not-a-command-xyz"}, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("discovery of an unresolvable command must not fail the caller: %v", err)
	}
	if len(discovery.Unresolved) != 1 {
		t.Fatalf("the missing command must be reported as unresolved: %+v", discovery.Unresolved)
	}
	if len(discovery.Dependencies) != 0 {
		t.Fatalf("nothing should have been resolved: %+v", discovery.Dependencies)
	}
}

// TestCopyXattrsRoundTrip exercises the raw llistxattr/lgetxattr/lsetxattr
// wrappers on a real filesystem: an attribute set on the source must appear
// with the same value on the destination after copyXattrs.
func TestCopyXattrsRoundTrip(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	writeTreeEntry(t, source, "state", 0o600)
	value := []byte("checkpointed")
	if err := lsetxattr(source, "user.shift.test", value, 0); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			// tmpfs without user xattrs, or a filesystem with no xattr
			// support at all — the wrappers cannot be proven here.
			t.Skipf("filesystem under %s has no xattr support: %v", directory, err)
		}
		t.Fatalf("set test xattr: %v", err)
	}
	destination := filepath.Join(directory, "destination")
	writeTreeEntry(t, destination, "state", 0o600)
	if err := copyXattrs(source, destination); err != nil {
		t.Fatalf("copyXattrs: %v", err)
	}
	size, err := lgetxattr(destination, "user.shift.test", nil)
	if err != nil {
		t.Fatalf("copied xattr missing: %v", err)
	}
	copied := make([]byte, size)
	if _, err := lgetxattr(destination, "user.shift.test", copied); err != nil {
		t.Fatalf("read copied xattr: %v", err)
	}
	if !bytes.Equal(copied, value) {
		t.Fatalf("copied xattr = %q, want %q", copied, value)
	}
}

// TestCloneFileFallbackFailureLeavesNoPartialFile: when the byte-copy
// fallback fails mid-way, the partially written destination must not be
// left behind — callers assume the destination is either absent or a
// complete copy. /proc/self/mem is a regular file whose reads fail with EIO,
// which drives the fallback's copy error deterministically.
func TestCloneFileFallbackFailureLeavesNoPartialFile(t *testing.T) {
	if _, err := os.Open("/proc/self/mem"); err != nil {
		t.Skipf("cannot open /proc/self/mem: %v", err)
	}
	target := filepath.Join(t.TempDir(), "copy")
	if _, err := CloneFile("/proc/self/mem", target); err == nil {
		t.Fatal("cloning an unreadable file must fail")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("a failed fallback copy must remove its partial destination")
	}
}

// TestCloneFileCreatesACompleteCopy: the success paths (reflink clone or
// byte fallback) must leave a byte-exact, complete destination and no
// intermediates behind.
func TestCloneFileCreatesACompleteCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	content := bytes.Repeat([]byte("durable\n"), 1000)
	if err := os.WriteFile(source, content, 0o640); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if _, err := CloneFile(source, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("cloned file differs from the source")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("clone left unexpected files behind: %v", entries)
	}
}

// TestSnapshotCreateRefusesUnsafeIDs: the snapshot id is joined into a
// filesystem path, so ids carrying separators or dot segments must be
// refused before any directory is created — a caller-supplied id can never
// steer the snapshot wrapper outside the workload's parent.
func TestSnapshotCreateRefusesUnsafeIDs(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "root")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	provider := btrfsProvider{}
	for _, id := range []string{"../escape", "a/b", "/abs", "..", ".", "x..y", "a\\b", "x y"} {
		if _, err := provider.Create(context.Background(), source, id); err == nil || !strings.Contains(err.Error(), "plain identifier") {
			t.Fatalf("snapshot id %q must be refused, got %v", id, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "escape")); !os.IsNotExist(err) {
		t.Fatal("a refused id must not create anything outside the parent")
	}
}
