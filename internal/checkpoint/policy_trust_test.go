package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"shift.dev/shift/internal/chunkstore"
	"shift.dev/shift/internal/identity"
	"shift.dev/shift/internal/model"
	linuxplatform "shift.dev/shift/internal/platform/linux"
	shiftruntime "shift.dev/shift/internal/runtime"
	"shift.dev/shift/internal/securestore"
)

type inertEngine struct{}

func (inertEngine) Check(context.Context) error                { return nil }
func (inertEngine) Version(context.Context) (string, error)    { return "inert", nil }
func (inertEngine) PreDump(context.Context, DumpOptions) error { return nil }
func (inertEngine) Dump(context.Context, DumpOptions) error    { return nil }
func (inertEngine) Restore(context.Context, RestoreOptions) (int, error) {
	return 0, errors.New("inert engine")
}

func newIngestFixture(t *testing.T) (*Service, *identity.Identity) {
	t.Helper()
	stateRoot := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runtimeManager, err := shiftruntime.OpenManager(filepath.Join(stateRoot, "runtime"), false, logger)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := identity.Ensure(filepath.Join(stateRoot, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := securestore.Open(filepath.Join(stateRoot, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := chunkstore.Open(filepath.Join(stateRoot, "objects"), 64<<10, keys)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := OpenRepository(filepath.Join(stateRoot, "checkpoints"), keys)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(stateRoot, runtimeManager, machine, linuxplatform.NewInventory(machine.Machine.ID), chunks, repository, inertEngine{}, logger)
	return service, machine
}

func ingestManifest(t *testing.T, signer *identity.Identity) model.CheckpointManifest {
	t.Helper()
	id, err := model.NewID()
	if err != nil {
		t.Fatal(err)
	}
	manifest := model.CheckpointManifest{
		Format: model.StateFormatName, FormatVersion: model.StateFormatVersion,
		ID: id, Kind: model.CheckpointFull,
		Workload:  model.WorkloadSpec{ID: "workload-ingest", Name: "ingest", RootPath: "/srv/ingest"},
		CreatedAt: time.Now().UTC(),
	}
	if err := SignManifest(&manifest, signer); err != nil {
		t.Fatal(err)
	}
	return manifest
}

// TestImportRequiresTrustAnchor: a self-consistent signature is not
// admission. Non-peer ingest admits only this machine or an operator-pinned
// source identity; anything else fails closed.
func TestImportRequiresTrustAnchor(t *testing.T) {
	service, local := newIngestFixture(t)

	outsider, err := identity.Ensure(filepath.Join(t.TempDir(), "identity"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := ingestManifest(t, outsider)
	if err := service.Import(manifest); !errors.Is(err, ErrUntrustedSigner) {
		t.Fatalf("import of an untrusted signer must fail closed, got %v", err)
	}

	if err := service.repository.Trust().Pin(outsider.Machine.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Import(manifest); err != nil {
		t.Fatalf("a pinned source must be admitted: %v", err)
	}

	localManifest := ingestManifest(t, local)
	if err := service.Import(localManifest); err != nil {
		t.Fatalf("this machine's own checkpoints must be admitted: %v", err)
	}
}

func TestTrustStoreValidatesPins(t *testing.T) {
	store, err := OpenTrustStore(filepath.Join(t.TempDir(), "trusted-sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin("not-a-machine-id"); err == nil {
		t.Fatal("an invalid machine id must not be pinnable")
	}
	id := strings.Repeat("ab", 32)
	if err := store.Pin(id); err != nil {
		t.Fatal(err)
	}
	if !store.Trusted(id) {
		t.Fatal("a pinned identity must be trusted")
	}
	reopened, err := OpenTrustStore(filepath.Join(filepath.Dir(store.path), "trusted-sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Trusted(id) {
		t.Fatal("pins must survive a reopen")
	}
}

func TestPolicySanitizeExtractedTree(t *testing.T) {
	root := t.TempDir()
	setuidBinary := filepath.Join(root, "suid")
	if err := os.WriteFile(setuidBinary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(setuidBinary, 0o755|os.ModeSetuid|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(setuidBinary)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Fatalf("setuid/setgid bits must be stripped, got %v", info.Mode())
	}

	escaping := filepath.Join(root, "escape")
	if err := os.Symlink("../../outside", escaping); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{}); err == nil || !strings.Contains(err.Error(), "escapes the extraction root") {
		t.Fatalf("a symlink escaping the extraction root must be refused, got %v", err)
	}
	if err := os.Remove(escaping); err != nil {
		t.Fatal(err)
	}
	absolute := filepath.Join(root, "absolute")
	if err := os.Symlink("/usr/bin/env", absolute); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{}); err == nil || !strings.Contains(err.Error(), "absolute target") {
		t.Fatalf("an absolute symlink target is an escape and must be refused, got %v", err)
	}
	if err := os.Remove(absolute); err != nil {
		t.Fatal(err)
	}
	chained := filepath.Join(root, "chained")
	escapeHop := filepath.Join(root, "hop")
	if err := os.Symlink("/etc", escapeHop); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hop/shadow", chained); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{}); err == nil || !strings.Contains(err.Error(), "escapes the extraction root") {
		t.Fatalf("a chained symlink reaching the host must be refused, got %v", err)
	}
	if err := os.Remove(chained); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(escapeHop); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside-target")
	outside := filepath.Join(root, "outside-link")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside-target", outside); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{}); err != nil {
		t.Fatalf("a symlink resolving inside the extraction root must be allowed: %v", err)
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}

	kept := filepath.Join(root, "kept")
	if err := os.WriteFile(kept, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(kept, 0o755|os.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if err := sanitizeExtractedTree(root, RestorePolicy{AllowSetuid: true}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(kept)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Fatal("an explicit setuid allowance must keep the bits")
	}
}

// TestPolicyXattrStrippingDecision pins the privilege-boundary decision for
// extracted extended attributes without needing CAP_SETFCAP: only the
// security and trusted namespaces are privilege-bearing, security.capability
// survives solely under allow_file_caps, and user/POSIX-ACL attributes are
// never touched.
func TestPolicyXattrStrippingDecision(t *testing.T) {
	strict := RestorePolicy{}
	kept := RestorePolicy{AllowFileCaps: true}
	for _, name := range []string{"security.capability", "security.selinux", "security.SMACK64", "security.apparmor", "trusted.overlay.opaque", "trusted.overlay.whiteout"} {
		if !xattrStrippedByPolicy(name, strict) {
			t.Fatalf("%s must be stripped under the default policy", name)
		}
	}
	if xattrStrippedByPolicy("security.capability", kept) {
		t.Fatal("an explicit file-capability allowance must keep security.capability")
	}
	if !xattrStrippedByPolicy("security.selinux", kept) {
		t.Fatal("allow_file_caps covers capabilities only; other security labels stay stripped")
	}
	for _, name := range []string{"user.checksum", "system.posix_acl_access", "system.posix_acl_default", "security", "not-security.capability"} {
		if xattrStrippedByPolicy(name, strict) {
			t.Fatalf("%s is not privilege-bearing and must be kept", name)
		}
	}
}

// TestPolicySanitizeExtractedEntryNeutralizesSpecialFilesAndCaps is the
// file-capability / special-file regression for the extraction policy gate:
// device nodes, FIFOs, and sockets a hostile archive plants are refused, the
// setuid escape is neutralized exactly like the tree walk does, and
// privilege-bearing xattrs are stripped while ordinary ones survive.
func TestPolicySanitizeExtractedEntryNeutralizesSpecialFilesAndCaps(t *testing.T) {
	root := t.TempDir()

	for name, mode := range map[string]os.FileMode{
		"device node": os.ModeDevice | 0o600,
		"char device": os.ModeDevice | os.ModeCharDevice | 0o600,
		"fifo":        os.ModeNamedPipe | 0o600,
		"socket":      os.ModeSocket | 0o600,
		"irregular":   os.ModeIrregular | 0o600,
	} {
		special := filepath.Join(root, "special")
		if err := SanitizeExtractedEntry(special, mode, RestorePolicy{}); err == nil {
			t.Fatalf("a planted %s must be refused by the policy gate", name)
		}
	}

	fifo := filepath.Join(root, "real-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(fifo)
	if err != nil {
		t.Fatal(err)
	}
	if err := SanitizeExtractedEntry(fifo, info.Mode(), RestorePolicy{}); err == nil {
		t.Fatal("a FIFO surviving extraction must be refused")
	}
	if err := SanitizeExtractedEntry(fifo, info.Mode(), RestorePolicy{AllowDeviceNodes: true}); err != nil {
		t.Fatalf("the device-node policy override must cover FIFOs too: %v", err)
	}
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}

	suid := filepath.Join(root, "suid")
	if err := os.WriteFile(suid, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(suid, 0o755|os.ModeSetuid|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(suid)
	if err != nil {
		t.Fatal(err)
	}
	if err := SanitizeExtractedEntry(suid, info.Mode(), RestorePolicy{}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Lstat(suid)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Fatalf("the setuid escape must be neutralized, got %v", info.Mode())
	}

	plain := filepath.Join(root, "with-xattrs")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(plain, "user.checksum", []byte("abc"), 0); err != nil {
		t.Fatal(err)
	}
	if err := SanitizeExtractedEntry(plain, 0o644, RestorePolicy{}); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 256)
	read, err := syscall.Listxattr(plain, buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buffer[:read]), "user.checksum") {
		t.Fatal("ordinary user xattrs are not privilege-bearing and must survive sanitization")
	}

	t.Run("file capabilities", func(t *testing.T) {
		capable := filepath.Join(root, "capable")
		if err := os.WriteFile(capable, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		// v2 security.capability with cap_setuid+cap_dac_override effective.
		blob := []byte{0x01, 0x00, 0x00, 0x02, 0x81, 0x00, 0x00, 0x00, 0x81, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		if err := syscall.Setxattr(capable, "security.capability", blob, 0); err != nil {
			t.Skipf("cannot plant security.capability without CAP_SETFCAP: %v", err)
		}
		if err := SanitizeExtractedEntry(capable, 0o755, RestorePolicy{}); err != nil {
			t.Fatal(err)
		}
		read, err := syscall.Listxattr(capable, buffer)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(buffer[:read]), "security.capability") {
			t.Fatal("BUG REGRESSED: file capabilities survived restore sanitization")
		}
		if err := syscall.Setxattr(capable, "security.capability", blob, 0); err != nil {
			t.Fatal(err)
		}
		if err := SanitizeExtractedEntry(capable, 0o755, RestorePolicy{AllowFileCaps: true}); err != nil {
			t.Fatal(err)
		}
		read, err = syscall.Listxattr(capable, buffer)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(buffer[:read]), "security.capability") {
			t.Fatal("an explicit allow_file_caps must keep the capabilities")
		}
	})
}

func TestPolicyLoadRestorePolicyRejectsTrailingDataAndSymlinks(t *testing.T) {
	stateDir := t.TempDir()
	metadata := filepath.Join(stateDir, "metadata")
	if err := os.MkdirAll(metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(metadata, "restore-policy.json")
	if err := os.WriteFile(path, []byte(`{"allow_root_workload": true}{"garbage": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRestorePolicy(stateDir); err == nil {
		t.Fatal("trailing JSON after the policy value must be refused, not silently dropped")
	}
	if err := os.WriteFile(path, []byte(`{"allow_root_workload": true}   `), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadRestorePolicy(stateDir)
	if err != nil || !policy.AllowRootWorkload {
		t.Fatalf("trailing whitespace is not trailing data: %+v %v", policy, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(stateDir, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"allow_root_workload": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRestorePolicy(stateDir); err == nil {
		t.Fatal("a symlink planted at the policy path must be refused, not followed")
	}
}

// TestTrustStoreRefusesUnsafeFileTypes: trusted-sources.json is operator
// authority exactly like restore-policy.json and must clear the same bar —
// symlink follow, FIFO/hang, missing owner and mode checks are all refused.
func TestTrustStoreRefusesUnsafeFileTypes(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "trusted-sources.json")

	outside := filepath.Join(directory, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"trusted_machine_ids": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrustStore(path); err == nil {
		t.Fatal("a symlink planted at the trust-file path must be refused, not followed")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != `{"trusted_machine_ids": []}` {
		t.Fatalf("the symlink target must be untouched, got %q %v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrustStore(path); err == nil {
		t.Fatal("a FIFO at the trust-file path must be refused, not opened")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(`{"trusted_machine_ids": []}`), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrustStore(path); err == nil {
		t.Fatal("a group- or world-writable trust file must be refused")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"trusted_machine_ids": []}{"extra": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrustStore(path); err == nil {
		t.Fatal("trailing JSON after the trust value must be refused")
	}
	big := filepath.Join(directory, "big.json")
	if err := os.WriteFile(big, append([]byte(`{"trusted_machine_ids": ["`), append(bytes.Repeat([]byte("a"), int(maxTrustFileBytes)), []byte(`"]}`)...)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrustStore(big); err == nil {
		t.Fatal("an oversized trust file must be refused")
	}
}

// TestTrustStorePinDoesNotFollowSymlinks: persist replaces the trust-file
// directory entry via rename, so a symlink swapped in after the store was
// opened is never written through into an arbitrary root-owned file.
func TestTrustStorePinDoesNotFollowSymlinks(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "trusted-sources.json")
	store, err := OpenTrustStore(path)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(directory, "victim.json")
	original := []byte("do not clobber")
	if err := os.WriteFile(victim, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("ab", 32)
	if err := store.Pin(id); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(victim); err != nil || !bytes.Equal(data, original) {
		t.Fatalf("Pin must never write through a planted symlink, victim = %q %v", data, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("the planted symlink must be replaced by the real trust file")
	}
	reopened, err := OpenTrustStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Trusted(id) {
		t.Fatal("pins must survive a reopen through the hardened path")
	}
}
