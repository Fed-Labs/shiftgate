package checkpoint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// capabilityV2 is a valid VFS_CAP_REVISION_2 security.capability payload
// carrying cap_setuid in the effective/permitted set — the exact primitive a
// hostile checkpoint plants to escalate the restored workload to host root.
var capabilityV2 = []byte{
	0x01, 0x00, 0x00, 0x02, // magic_etc = VFS_CAP_REVISION_2 | VFS_CAP_FLAGS_EFFECTIVE
	0x80, 0x00, 0x00, 0x00, // permitted[0]: CAP_SETUID
	0x00, 0x00, 0x00, 0x00, // permitted[1]
	0x00, 0x00, 0x00, 0x00, // inheritable[0]
	0x00, 0x00, 0x00, 0x00, // inheritable[1]
}

// plantXattr sets one extended attribute and reports whether the environment
// permitted it. security.* and trusted.* need privileges the test user may
// not hold; those assertions then skip rather than silently pass.
func plantXattr(t *testing.T, path, name string, value []byte) bool {
	t.Helper()
	err := unix.Setxattr(path, name, value, 0)
	if err == nil {
		return true
	}
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
		t.Logf("cannot plant %s in this environment (%v); its assertions will skip", name, err)
		return false
	}
	t.Fatalf("plant %s: %v", name, err)
	return false
}

func listXattrs(t *testing.T, path string) []string {
	t.Helper()
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		t.Fatalf("list xattrs of %s: %v", path, err)
	}
	if size <= 0 {
		return nil
	}
	buffer := make([]byte, size)
	read, err := unix.Llistxattr(path, buffer)
	if err != nil {
		t.Fatalf("list xattrs of %s: %v", path, err)
	}
	names := []string{}
	for _, name := range strings.Split(string(buffer[:read]), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func hasXattr(t *testing.T, path, name string) bool {
	t.Helper()
	for _, existing := range listXattrs(t, path) {
		if existing == name {
			return true
		}
	}
	return false
}

// TestRegressionSanitizeStripsPrivilegedXattrs is the CRITICAL regression for
// the file-capability escalation: security.capability (and every other
// security.* / trusted.* label) must not survive archive sanitization, while
// ordinary user metadata keeps its place. A planted cap_setuid=ep binary is
// the modern setuid escape the sanitize pass promises to neutralize.
func TestRegressionSanitizeStripsPrivilegedXattrs(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "bin-evil")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nid\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	capability := plantXattr(t, binary, "security.capability", capabilityV2)
	trusted := plantXattr(t, binary, "trusted.overlay.opaque", []byte("y"))
	plantXattr(t, binary, "user.marker", []byte("keep me"))

	if err := sanitizeExtractedTree(root, RestorePolicy{}); err != nil {
		t.Fatal(err)
	}
	if capability && hasXattr(t, binary, "security.capability") {
		t.Fatal("BYPASS REGRESSED: security.capability survived archive sanitization")
	}
	if trusted && hasXattr(t, binary, "trusted.overlay.opaque") {
		t.Fatal("BYPASS REGRESSED: trusted.* survived archive sanitization")
	}
	if !hasXattr(t, binary, "user.marker") {
		t.Fatal("ordinary user xattrs are not privilege-bearing and must survive")
	}

	// The policy flag is the only way capabilities may be kept — the same
	// treatment setuid gets.
	if capability {
		if err := unix.Setxattr(binary, "security.capability", capabilityV2, 0); err != nil {
			t.Fatalf("replant capability: %v", err)
		}
		if err := sanitizeExtractedTree(root, RestorePolicy{AllowFileCaps: true}); err != nil {
			t.Fatal(err)
		}
		if !hasXattr(t, binary, "security.capability") {
			t.Fatal("an explicit allow_file_caps policy must keep capabilities")
		}
		if err := unix.Lremovexattr(binary, "security.capability"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRegressionSanitizePreservesOrdinaryPermissionAndOwnership guards the
// other half of restored-file metadata handling: sanitization strips
// privilege, never fidelity. Ordinary modes, ownership, and content must come
// through the round trip unchanged.
func TestRegressionSanitizePreservesOrdinaryPermissionAndOwnership(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "data.conf")
	if err := os.WriteFile(plain, []byte("state"), 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(plain)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode().Perm() != 0o640 {
		t.Fatalf("ordinary permissions must be preserved, got %v", after.Mode())
	}
	if after.Size() != info.Size() {
		t.Fatal("sanitization must not touch file contents")
	}
	if afterStat, ok := after.Sys().(*syscall.Stat_t); ok {
		if beforeStat, ok := info.Sys().(*syscall.Stat_t); ok && afterStat.Uid != beforeStat.Uid {
			t.Fatal("sanitization must not change file ownership")
		}
	}
}

// TestRegressionSanitizeRefusesFIFOs covers the special-file half of the
// privilege refusal: a planted FIFO blocks any later open forever, so it is
// refused exactly like a device node unless explicitly allowed.
func TestRegressionSanitizeRefusesFIFOs(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "hang.fifo")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	err := sanitizeExtractedTree(root, RestorePolicy{})
	if err == nil || !strings.Contains(err.Error(), "FIFO") {
		t.Fatalf("a FIFO must be refused unless explicitly allowed, got %v", err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{AllowDeviceNodes: true}); err != nil {
		t.Fatalf("the explicit special-file allowance must keep it: %v", err)
	}
}

// TestRegressionCaptureExtractRoundTripStripsFileCapabilities is the full
// CRITICAL-2 regression over the real pipeline: a capability planted on a
// source file round-trips through capture and extraction, and the restored
// tree must come out with the privilege gone — the archive layer alone must
// never be a privilege-escalation delivery vehicle.
func TestRegressionCaptureExtractRoundTripStripsFileCapabilities(t *testing.T) {
	fixture := newRepositoryFixture(t)
	source := t.TempDir()
	binary := filepath.Join(source, "tool")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho restored\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	capability := plantXattr(t, binary, "security.capability", capabilityV2)

	ctx := context.Background()
	result, err := captureDirectory(ctx, fixture.chunks, "workload-1", "filesystem-root", filepath.Dir(source), []string{filepath.Base(source)}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := extractDirectory(ctx, fixture.chunks, "workload-1", result.Asset, destination, RestorePolicy{}); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(destination, filepath.Base(source), "tool")
	if _, err := os.Stat(restored); err != nil {
		t.Fatal(err)
	}
	if capability && hasXattr(t, restored, "security.capability") {
		t.Fatal("BYPASS REGRESSED: a file capability survived the capture/extract round trip")
	}
	for _, name := range listXattrs(t, restored) {
		if strings.HasPrefix(name, "security.") || strings.HasPrefix(name, "trusted.") {
			t.Fatalf("BYPASS REGRESSED: privileged xattr %s survived the round trip", name)
		}
	}
	info, err := os.Stat(restored)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("restored file mode must be preserved, got %v", info.Mode())
	}
}

// escapeMountInfoPath encodes a path the way /proc/self/mountinfo does:
// backslash, space, tab, and newline become \134, \040, \011, \012.
func escapeMountInfoPath(value string) string {
	value = strings.ReplaceAll(value, `\`, `\134`)
	value = strings.ReplaceAll(value, " ", `\040`)
	value = strings.ReplaceAll(value, "\t", `\011`)
	value = strings.ReplaceAll(value, "\n", `\012`)
	return value
}

// TestRegressionCaptureRefusesInnerMountpoint is the HIGH regression: a
// mounted volume inside the captured tree would be silently dropped by tar's
// --one-file-system, so the capture must refuse it clearly before any bytes
// move. The check runs against the real mount table on the happy path and
// against synthetic tables here, covering all four mountinfo escapes.
func TestRegressionCaptureRefusesInnerMountpoint(t *testing.T) {
	root := t.TempDir()
	line := func(mountpoint string) string {
		return "21 1 8:2 / " + escapeMountInfoPath(mountpoint) + " rw - ext4 /dev/sda1 rw\n"
	}
	cases := []struct {
		name    string
		table   string
		base    string
		exclude []string
		refuses bool
	}{
		{"inner mount refused", line(filepath.Join(root, "inner", "vol")), "inner", nil, true},
		{"escaped space mount refused", line(filepath.Join(root, "inner", "with space")), "inner", nil, true},
		{"escaped tab mount refused", line(filepath.Join(root, "inner", "with\ttab")), "inner", nil, true},
		{"escaped newline mount refused", line(filepath.Join(root, "inner", "with\nnl")), "inner", nil, true},
		{"escaped backslash mount refused", line(filepath.Join(root, "inner", `with\bs`)), "inner", nil, true},
		{"mount at base itself allowed", line(filepath.Join(root, "inner")), "inner", nil, false},
		{"ancestor mount allowed", line(root), "inner", nil, false},
		{"excluded subtree allowed", line(filepath.Join(root, "inner", "vol")), "inner", []string{"vol"}, false},
	}
	for _, testCase := range cases {
		err := checkCaptureMountpointsTable(testCase.table, root, []string{testCase.base}, testCase.exclude)
		if testCase.refuses && err == nil {
			t.Fatalf("%s: BOUNDARY BYPASS REGRESSED: the mountpoint was not refused", testCase.name)
		}
		if !testCase.refuses && err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
	}

	// End-to-end through captureDirectory against a synthetic mount table.
	fixture := newRepositoryFixture(t)
	tree := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(filepath.Join(tree, "vol"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "vol", "data.txt"), []byte("volume content"), 0o600); err != nil {
		t.Fatal(err)
	}
	tableFile := filepath.Join(t.TempDir(), "mountinfo")
	synthetic := "21 1 8:2 / " + filepath.Join(tree, "vol") + " rw - ext4 /dev/sda1 rw\n"
	if err := os.WriteFile(tableFile, []byte(synthetic), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := mountinfoPath
	mountinfoPath = tableFile
	defer func() { mountinfoPath = saved }()

	_, err := captureDirectory(context.Background(), fixture.chunks, "workload-1", "filesystem-root", filepath.Dir(tree), []string{filepath.Base(tree)}, nil, 10)
	if err == nil || !strings.Contains(err.Error(), "mounted volume") {
		t.Fatalf("capture of a tree with an inner mountpoint must fail clearly, got %v", err)
	}

	// Without the inner mount the same capture succeeds.
	if err := os.WriteFile(tableFile, []byte("21 1 8:2 / /somewhere rw - ext4 /dev/sda1 rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureDirectory(context.Background(), fixture.chunks, "workload-1", "filesystem-root", filepath.Dir(tree), []string{filepath.Base(tree)}, nil, 10); err != nil {
		t.Fatalf("a mount-free tree must capture cleanly: %v", err)
	}
}

// TestRegressionCaptureTreatsDroppedContentWarningsAsErrors: even a clean tar
// exit must not hide a "not dumped" / "different filesystem" warning — a
// capture that quietly left data out is never a valid checkpoint.
func TestRegressionCaptureTreatsDroppedContentWarningsAsErrors(t *testing.T) {
	if captureDroppedContent("") {
		t.Fatal("empty stderr is not a dropped-content signal")
	}
	if captureDroppedContent("tar: Removing leading `/' from member names\n") {
		t.Fatal("ordinary tar chatter is not a dropped-content signal")
	}
	if !captureDroppedContent("tar: vol: file is on a different filesystem; not dumped") {
		t.Fatal("a different-filesystem skip must be treated as a failed capture")
	}
	if !captureDroppedContent("tar: vol/x: Not dumped") {
		t.Fatal("a not-dumped warning must be treated as a failed capture")
	}
}

// TestRegressionMountinfoEscapesDecoded keeps the restore-side mountpoint
// refusal honest for paths carrying any of mountinfo's four octal escapes —
// decoding only \040 silently skipped the snapshot-adapter refusal for roots
// with tab, newline, or backslash in their names.
func TestRegressionMountinfoEscapesDecoded(t *testing.T) {
	table := "21 1 8:2 / /srv/with\\040space rw - ext4 /dev/sda1 rw\n" +
		"22 1 8:3 / /srv/with\\011tab rw - ext4 /dev/sdb1 rw\n" +
		"23 1 8:4 / /srv/with\\012nl rw - ext4 /dev/sdc1 rw\n" +
		"24 1 8:5 / /srv/with\\134bs rw - ext4 /dev/sdd1 rw\n"
	if !mountinfoContains(table, "/srv/with space") {
		t.Fatal("escaped space must match its mountpoint")
	}
	if !mountinfoContains(table, "/srv/with\ttab") {
		t.Fatal("escaped tab must match its mountpoint")
	}
	if !mountinfoContains(table, "/srv/with\nnl") {
		t.Fatal("escaped newline must match its mountpoint")
	}
	if !mountinfoContains(table, `/srv/with\bs`) {
		t.Fatal("escaped backslash must match its mountpoint")
	}
	if mountinfoContains(table, "/srv/plain") {
		t.Fatal("an unlisted path must not match")
	}
}
