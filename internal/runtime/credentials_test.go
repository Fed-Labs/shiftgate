package runtime

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"shift.dev/shift/internal/model"
	"shift.dev/shift/internal/securestore"
)

// Regression suite for the uid-0 gate bypass: admission read only the
// manifest-declared Workload.UID, while the restored process's real
// credentials come from the CRIU images and were never reconciled. A hostile
// checkpoint declaring uid 1000 and carrying root images was adopted as a
// root process. Adoption must read /proc/<pid>/status and refuse — and kill
// — anything that does not match the manifest.

func spawnCredentialProbe(t *testing.T) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(t.Context(), "/bin/sh", "-c", "while true; do sleep 1; done")
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = command.Wait() }()
	t.Cleanup(func() { _ = command.Process.Kill() })
	return command
}

func TestVerifyRestoredCredentials(t *testing.T) {
	command := spawnCredentialProbe(t)
	pid := command.Process.Pid
	self := model.WorkloadSpec{UID: os.Geteuid(), GID: os.Getegid()}
	if err := verifyRestoredCredentials(pid, self); err != nil {
		t.Fatalf("a process matching the manifest must be accepted: %v", err)
	}
	mismatch := model.WorkloadSpec{UID: os.Geteuid() + 1, GID: os.Getegid()}
	err := verifyRestoredCredentials(pid, mismatch)
	if err == nil {
		t.Fatal("a uid mismatch between the live process and the manifest must be refused")
	}
	if !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("the refusal must say the process is not adopted, got %v", err)
	}
	gidMismatch := model.WorkloadSpec{UID: os.Geteuid(), GID: os.Getegid() + 1}
	if err := verifyRestoredCredentials(pid, gidMismatch); err == nil {
		t.Fatal("a gid mismatch between the live process and the manifest must be refused")
	}
	if err := verifyRestoredCredentials(-1, self); err == nil {
		t.Fatal("an unreadable process must not verify")
	}
}

// TestAdoptRestoredRefusesCredentialMismatch is the full gate: a workload
// registered under one identity must not adopt a restored process running
// under another — the exact shape of the crafted-image bypass.
func TestAdoptRestoredRefusesCredentialMismatch(t *testing.T) {
	stateDir := t.TempDir()
	keys, err := securestore.Open(filepath.Join(stateDir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManagerWithKeys(filepath.Join(stateDir, "runtime"), false, logger, keys)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workload, err := manager.Create(model.WorkloadSpec{
		Name: "cred-mismatch", Command: []string{"/bin/true"}, RootPath: root, WorkingDir: root,
		UID: os.Geteuid() + 1, GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	command := spawnCredentialProbe(t)
	pid := command.Process.Pid
	_, err = manager.AdoptRestored(workload.Spec.ID, pid)
	if err == nil {
		t.Fatal("BYPASS REGRESSED: a restored process with mismatched credentials was adopted")
	}
	if !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("the refusal must name the credential mismatch, got %v", err)
	}
	// The mismatched process must be gone, not merely unadopted.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the mismatched restored process must be killed on refusal")
}

func TestParseProcessCredentials(t *testing.T) {
	status := "Name:\tsh\nUid:\t1000\t1000\t1000\t1000\nGid:\t1001\t1001\t1001\t1001\n"
	uid, gid, err := parseProcessCredentials(status)
	if err != nil || uid != 1000 || gid != 1001 {
		t.Fatalf("parsed %d %d %v, want 1000 1001", uid, gid, err)
	}
	if _, _, err := parseProcessCredentials("Name:\tsh\n"); err == nil {
		t.Fatal("a status document without Uid/Gid must not parse")
	}
}
