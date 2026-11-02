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
	"shift.dev/shift/internal/network"
)

// TestWorkloadEnvironmentIsMinimalAndScrubbed: a workload must never inherit
// the agent's environment — it carries the machines-scope API key and the
// storage credentials, and the threat model makes a malicious workload an
// explicit trust boundary. Only a minimal base plus the spec's explicitly
// scoped variables may reach the process, and agent credentials are scrubbed
// even when something asks for them by name.
func TestWorkloadEnvironmentIsMinimalAndScrubbed(t *testing.T) {
	t.Setenv("SHIFT_CONTROL_API_KEY", "shift_ak_machine_scope")
	t.Setenv("SHIFT_S3_SECRET_ACCESS_KEY", "s3-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("LEAKY_UNRELATED", "leaked")
	t.Setenv("LANG", "en_US.UTF-8")

	env := mergedEnvironment(map[string]string{
		"FOO":                   "bar",
		"PATH":                  "/custom/bin",
		"SHIFT_CONTROL_API_KEY": "injected",
		"AWS_SECRET_ACCESS_KEY": "injected",
		"":                      "no-key",
	})

	joined := strings.Join(env, "\n")
	for _, want := range []string{"FOO=bar", "PATH=/custom/bin", "LANG=en_US.UTF-8"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("explicitly scoped state missing from %v", env)
		}
	}
	for _, forbidden := range []string{"SHIFT_", "AWS_", "S3_", "LEAKY_UNRELATED", "no-key"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("agent state leaked into workload env (%q): %v", forbidden, env)
		}
	}
}

// TestStartedWorkloadRunsScrubbedEnvironment is the same guarantee end to
// end: the process SHIFT actually spawns reads no agent credential from its
// own environment.
func TestStartedWorkloadRunsScrubbedEnvironment(t *testing.T) {
	t.Setenv("SHIFT_CONTROL_API_KEY", "shift_ak_machine_scope")
	t.Setenv("SHIFT_S3_SECRET_ACCESS_KEY", "s3-secret")
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name:       "env-scrub",
		Command:    []string{"/bin/sh", "-c", "/usr/bin/env > env.out; while true; do sleep 1; done"},
		RootPath:   root,
		WorkingDir: root,
		UID:        os.Geteuid(),
		GID:        os.Getegid(),
		Environment: map[string]string{
			"FOO":                   "bar",
			"SHIFT_CONTROL_API_KEY": "injected",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(workload.Spec.ID); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = manager.Stop(workload.Spec.ID, 2*time.Second) }()

	deadline := time.Now().Add(5 * time.Second)
	var content []byte
	for time.Now().Before(deadline) {
		content, err = os.ReadFile(filepath.Join(root, "env.out"))
		if err == nil && len(content) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(content) == 0 {
		t.Fatalf("workload never wrote its environment: %v", err)
	}
	text := string(content)
	if !strings.Contains(text, "FOO=bar") || !strings.Contains(text, "PATH=") {
		t.Fatalf("explicit environment missing:\n%s", text)
	}
	for _, forbidden := range []string{"SHIFT_", "AWS_", "S3_"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("agent credential reachable from workload environment (%q):\n%s", forbidden, text)
		}
	}
}

// TestCreateRejectsClientSuppliedIDOverwrite: creating a workload with an ID
// that already exists must never replace the existing record — the victim's
// spec, checkpoints, and bookkeeping would be destroyed and, if it runs, its
// exit silently discarded. IDs must also not shadow names (or vice versa):
// Get resolves both, and a collision strands one of the records.
func TestCreateRejectsClientSuppliedIDOverwrite(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	victim, err := manager.Create(model.WorkloadSpec{
		ID: "wl_victim", Name: "victim", Command: []string{"/bin/true"},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(model.WorkloadSpec{
		ID: "wl_victim", Name: "attacker", Command: []string{"/bin/false"},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	}); err == nil {
		t.Fatal("BYPASS REGRESSED: a client-supplied ID overwrote an existing workload record")
	}
	kept, err := manager.Get("wl_victim")
	if err != nil || kept.Spec.Name != "victim" {
		t.Fatalf("victim record damaged: %+v %v", kept, err)
	}
	if kept.Generation != victim.Generation {
		t.Fatalf("victim record was touched: %+v", kept)
	}
	if _, err := manager.Create(model.WorkloadSpec{
		ID: "victim", Name: "shadow-id", Command: []string{"/bin/true"},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	}); err == nil {
		t.Fatal("an ID colliding with an existing workload name must be refused")
	}
	if _, err := manager.Create(model.WorkloadSpec{
		ID: "wl_other", Name: "wl_victim", Command: []string{"/bin/true"},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	}); err == nil {
		t.Fatal("a name colliding with an existing workload ID must be refused")
	}
}

// TestPrepareRestoreValidatesAndPreservesIdentity: the replacement spec is
// checkpoint-manifest data — hostile input — so the existing-record path must
// validate it like a fresh spec and refuse identity changes, instead of
// assigning it raw and letting an empty command panic the next Start.
func TestPrepareRestoreValidatesAndPreservesIdentity(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.WorkloadSpec{
		ID: "wl_restore", Name: "restored", Command: []string{"/bin/true"},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	}
	if _, err := manager.Create(spec); err != nil {
		t.Fatal(err)
	}

	renamed := spec
	renamed.Name = "renamed"
	if _, _, err := manager.PrepareRestore(renamed); err == nil {
		t.Fatal("a restore must not rename the workload it replaces")
	}

	empty := spec
	empty.Command = nil
	if _, _, err := manager.PrepareRestore(empty); err == nil {
		t.Fatal("a hostile manifest with an empty command must be rejected on the existing-record path too")
	}

	outside := spec
	outside.RootPath = "/"
	if _, _, err := manager.PrepareRestore(outside); err == nil {
		t.Fatal("a manifest claiming the filesystem root must be rejected")
	}

	// The name lookup backdoor: a spec whose ID is the record's name would
	// silently create a second record keyed by that name.
	shadow := spec
	shadow.ID = "restored"
	if _, _, err := manager.PrepareRestore(shadow); err == nil {
		t.Fatal("a restore spec that does not match the record's ID must be refused")
	}

	replaced := spec
	replaced.Command = []string{"/bin/false"}
	updated, created, err := manager.PrepareRestore(replaced)
	if err != nil {
		t.Fatalf("valid replacement must be accepted: %v", err)
	}
	if created || updated.Spec.Command[0] != "/bin/false" {
		t.Fatalf("replacement not applied: %+v", updated)
	}
}

// TestAdoptRestoredRecordsExitPromptly: a restored workload that dies must
// be recorded as failed right away — not left "running" until the next agent
// restart — must never be restarted, and must not leak as a zombie when the
// agent is its parent.
func TestAdoptRestoredRecordsExitPromptly(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "loop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name: "adopted", Command: []string{script},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(script)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if _, err := manager.AdoptRestored(workload.Spec.ID, pid); err != nil {
		t.Fatal(err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	failed := waitForStatus(t, manager, workload.Spec.ID, model.WorkloadFailed)
	if failed.Process != nil {
		t.Fatal("failed workload still reports a process")
	}
	if failed.LastError == "" {
		t.Fatal("adopted death records no reason")
	}
	// Nothing restarts it: the status stays failed.
	time.Sleep(2 * adoptedReapInterval)
	stable, err := manager.Get(workload.Spec.ID)
	if err != nil || stable.Status != model.WorkloadFailed {
		t.Fatalf("adopted workload must stay failed, got %+v %v", stable, err)
	}
	// The dead child must have been reaped, not left as a zombie.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatal("adopted process leaked as a zombie")
	}
}

// TestStopDoesNotHideLaterDeathAfterFailedSignal: when Stop's signal fails,
// the agent is no longer ordering this workload's end — the stopping marker
// must not leak, or a later death of its own is recorded as a clean stop and
// the crash is hidden from the operator.
func TestStopDoesNotHideLaterDeathAfterFailedSignal(t *testing.T) {
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "loop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workload, err := manager.Create(model.WorkloadSpec{
		Name: "sigfail", Command: []string{script},
		RootPath: root, WorkingDir: root, UID: os.Geteuid(), GID: os.Getegid(),
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := manager.Start(workload.Spec.ID)
	if err != nil {
		t.Fatal(err)
	}

	original := killProcessGroup
	killProcessGroup = func(int, syscall.Signal) error { return syscall.EPERM }
	_, stopErr := manager.Stop(workload.Spec.ID, time.Second)
	killProcessGroup = original
	if stopErr == nil {
		t.Fatal("Stop must report a signal it could not deliver")
	}
	manager.mu.Lock()
	leaked := manager.stopping[workload.Spec.ID]
	manager.mu.Unlock()
	if leaked {
		t.Fatal("an abandoned stop must not leave the stopping marker set")
	}

	// The crash the operator must see: the process dies on its own now.
	if err := syscall.Kill(-started.Process.PGID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	failed := waitForStatus(t, manager, workload.Spec.ID, model.WorkloadFailed)
	if failed.LastError == "" {
		t.Fatal("a crash after a failed stop must be recorded as a failure, not a clean stop")
	}
}

// TestOpenManagerRebuildsPortClaims: workloads that survive an agent restart
// keep holding their host ports, and the rebuilt NAT claims must say so —
// otherwise the next restore on the machine is promised a still-occupied
// port at VALIDATE and fails only mid-restore.
func TestOpenManagerRebuildsPortClaims(t *testing.T) {
	t.Cleanup(func() { network.RebuildReservations(nil) })
	root := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "loop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	base := func(name string, hostPort int) model.WorkloadSpec {
		return model.WorkloadSpec{
			Name: name, Command: []string{script}, RootPath: root, WorkingDir: root,
			UID: os.Geteuid(), GID: os.Getegid(),
			Ports: []model.PortSpec{{Protocol: "tcp", ContainerPort: hostPort + 1, HostPort: hostPort}},
		}
	}
	alive, err := manager.Create(base("alive", 18901))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(alive.Spec.ID); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = manager.Stop(alive.Spec.ID, 2*time.Second) }()
	registered, err := manager.Create(base("registered", 18902))
	if err != nil {
		t.Fatal(err)
	}
	checkpointed, err := manager.Create(base("checkpointed", 18903))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.MarkCheckpoint(checkpointed.Spec.ID, "cp-1", false); err != nil {
		t.Fatal(err)
	}

	// The agent restarts: a new manager over the same state dir rebuilds the
	// machine's port claims before the coordinator exists.
	reopened, err := OpenManager(root+"/state", false, logger)
	if err != nil {
		t.Fatal(err)
	}
	_ = reopened
	coordinator := network.NewCoordinator(logger)

	claim := func(hostPort int) []model.PortMapping {
		return []model.PortMapping{{Protocol: "tcp", ContainerPort: hostPort + 1, HostPort: hostPort}}
	}
	if err := coordinator.Prepare("rival", claim(18901)); err == nil {
		t.Fatal("a live workload's host port must stay reserved across the restart")
	}
	if err := coordinator.Prepare(alive.Spec.ID, claim(18901)); err != nil {
		t.Fatalf("the claim holder must re-reserve its own ports: %v", err)
	}
	if err := coordinator.Prepare("rival", claim(18903)); err == nil {
		t.Fatal("a checkpointed record's host port must stay reserved across the restart")
	}
	if err := coordinator.Prepare("rival", claim(18902)); err != nil {
		t.Fatalf("a workload with no process and no checkpoint holds no ports: %v", err)
	}
	_ = registered
}
