package linux

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCRIUVersion(t *testing.T) {
	cases := []struct {
		banner string
		want   string
	}{
		{"Version: 4.2\n", "4.2"},
		{"Version: 3.17.1 (git)\n", "3.17.1"},
		{"v4.1", "4.1"},
		{"no version here", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseCRIUVersion(c.banner); got != c.want {
			t.Fatalf("parseCRIUVersion(%q) = %q, want %q", c.banner, got, c.want)
		}
	}
}

func TestTrustedExecutableRejectsWritableOrForeignBinaries(t *testing.T) {
	directory := t.TempDir()
	safe := filepath.Join(directory, "safe")
	if err := os.WriteFile(safe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !trustedExecutable(safe) {
		t.Fatal("a regular owner-executed binary was rejected")
	}
	writable := filepath.Join(directory, "writable")
	if err := os.WriteFile(writable, []byte("#!/bin/sh\nexit 0\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	if trustedExecutable(writable) {
		t.Fatal("a world-writable binary was trusted")
	}
	directoryTarget := filepath.Join(directory, "adir")
	if err := os.Mkdir(directoryTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if trustedExecutable(directoryTarget) {
		t.Fatal("a directory was trusted as an executable")
	}
	if trustedExecutable(filepath.Join(directory, "missing")) {
		t.Fatal("a missing path was trusted")
	}
}

// TestGNUTarDetection proves the gnu_tar feature is only set by an actual GNU
// tar: busybox-style banners must not satisfy the archive-tool gate.
func TestGNUTarDetection(t *testing.T) {
	directory := t.TempDir()
	plant := func(name, script string) {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
	plant("tar", "#!/bin/sh\necho 'tar (GNU tar) 1.35'\n")
	if !gnuTarAvailable() {
		t.Fatal("GNU tar banner did not satisfy the feature")
	}
	plant("tar", "#!/bin/sh\necho 'bsdtar 3.6.1'\n")
	if gnuTarAvailable() {
		t.Fatal("bsdtar satisfied the gnu_tar feature")
	}
	plant("tar", "#!/bin/sh\necho 'busybox tar'\n")
	if gnuTarAvailable() {
		t.Fatal("busybox tar satisfied the gnu_tar feature")
	}
	// A world-writable tar is never trusted, GNU or not.
	plant("tar", "#!/bin/sh\necho 'tar (GNU tar) 1.35'\n")
	if err := os.Chmod(filepath.Join(directory, "tar"), 0o777); err != nil {
		t.Fatal(err)
	}
	if gnuTarAvailable() {
		t.Fatal("an untrusted tar binary satisfied the feature")
	}
}
