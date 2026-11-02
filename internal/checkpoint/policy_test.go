package checkpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shift.dev/shift/internal/model"
)

func testWorkload(uid int) model.WorkloadSpec {
	return model.WorkloadSpec{UID: uid, GID: os.Getegid(), Command: []string{"/bin/true"}}
}

func TestPolicyRestoreTargetRefusesSystemPaths(t *testing.T) {
	policy := RestorePolicy{AllowedRootPrefixes: []string{"/"}, AllowRootWorkload: true}
	for _, target := range []string{"/etc", "/etc/shift", "/usr", "/bin", "/var", "/var/lib/app", "/run", "/boot", "/lib64", "/root", "/proc", "/sys", "/dev", "/",
		"/opt", "/opt/app", "/home", "/home/alice", "/srv", "/srv/shift", "/mnt", "/mnt/data", "/media", "/media/usb", "/snap", "/snap/bin", "/tmp", "/tmp/scratch", "/"} {
		err := policy.CheckRestoreTarget(target, testWorkload(os.Geteuid()), "")
		if err == nil || !strings.Contains(err.Error(), "denied by destination policy") {
			t.Fatalf("system path %s must be refused, got %v", target, err)
		}
	}
}

func TestPolicyRestoreTargetAllowlistAndRegistration(t *testing.T) {
	allowedParent := t.TempDir()
	allowed := filepath.Join(allowedParent, "app")
	other := filepath.Join(t.TempDir(), "app")
	policy := RestorePolicy{AllowedRootPrefixes: []string{allowedParent}, AllowSystemTreeTargets: []string{os.TempDir()}}
	workload := testWorkload(os.Geteuid())

	if err := policy.CheckRestoreTarget(allowed, workload, ""); err != nil {
		t.Fatalf("target under the operator allowlist must be admitted: %v", err)
	}
	err := policy.CheckRestoreTarget(other, workload, "")
	if err == nil || !strings.Contains(err.Error(), "operator-registered restore root") {
		t.Fatalf("target outside the allowlist must be refused, got %v", err)
	}

	registered := filepath.Join(t.TempDir(), "registered")
	if err := policy.CheckRestoreTarget(registered, workload, registered); err != nil {
		t.Fatalf("the workload's registered root must be admitted with an empty allowlist: %v", err)
	}
	err = policy.CheckRestoreTarget(other, workload, registered)
	if err == nil || !strings.Contains(err.Error(), "registered on this destination") {
		t.Fatalf("a checkpoint targeting a different root than the registered workload must be refused, got %v", err)
	}

	failing := RestorePolicy{}
	if err := failing.CheckRestoreTarget(other, workload, ""); err == nil {
		t.Fatal("a zero policy must fail closed for unregistered targets")
	}
}

func TestPolicyRestoreTargetOwnership(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owned")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := RestorePolicy{AllowSystemTreeTargets: []string{os.TempDir()}}
	if err := policy.CheckRestoreTarget(target, testWorkload(os.Geteuid()), target); err != nil {
		t.Fatalf("target owned by the workload uid must be admitted: %v", err)
	}
	err := policy.CheckRestoreTarget(target, testWorkload(os.Geteuid()+1), target)
	if err == nil || !strings.Contains(err.Error(), "not owned by workload uid") {
		t.Fatalf("target owned by another uid must be refused, got %v", err)
	}

	link := filepath.Join(root, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err = policy.CheckRestoreTarget(link, testWorkload(os.Geteuid()), link)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("a symlinked restore target must be refused, got %v", err)
	}
}

func TestPolicyCheckWorkloadIdentity(t *testing.T) {
	restricted := RestorePolicy{}
	rootWorkload := testWorkload(0)
	err := restricted.CheckWorkload(rootWorkload)
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("uid 0 must be refused without explicit authorization, got %v", err)
	}
	authorized := RestorePolicy{AllowRootWorkload: true}
	if err := authorized.CheckWorkload(rootWorkload); err != nil {
		t.Fatalf("uid 0 must restore under an explicit root authorization: %v", err)
	}
	for name, workload := range map[string]model.WorkloadSpec{
		"negative uid":  {UID: -1, Command: []string{"/bin/true"}},
		"empty command": {UID: 1000, Command: nil},
		"nul in argv":   {UID: 1000, Command: []string{"/bin/true", "a\x00b"}},
	} {
		if err := restricted.CheckWorkload(workload); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestCheckNewRootPolicy(t *testing.T) {
	parent := t.TempDir()
	sourceRoot := filepath.Join(parent, "source")
	if err := os.Mkdir(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	policy := RestorePolicy{AllowSystemTreeTargets: []string{os.TempDir()}}
	workload := testWorkload(os.Geteuid())

	sibling := filepath.Join(parent, "source-fork-1234")
	if err := policy.CheckNewRoot(sibling, sourceRoot, workload); err != nil {
		t.Fatalf("a derived sibling root beside the source must be admitted: %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere-fork")
	if err := policy.CheckNewRoot(elsewhere, sourceRoot, workload); err == nil {
		t.Fatal("a root outside the allowlist and beside-nothing must be refused")
	}
	if err := (RestorePolicy{AllowedRootPrefixes: []string{filepath.Dir(elsewhere)}, AllowSystemTreeTargets: []string{os.TempDir()}}).CheckNewRoot(elsewhere, sourceRoot, workload); err != nil {
		t.Fatalf("an allowlisted new root must be admitted: %v", err)
	}
	if err := policy.CheckNewRoot(sourceRoot, sourceRoot, workload); err == nil {
		t.Fatal("an already-existing fork root must be refused")
	}
	if err := policy.CheckNewRoot("/usr/local/newdir", sourceRoot, workload); err == nil {
		t.Fatal("a system-path fork root must be refused")
	}
}

func TestLoadRestorePolicyFile(t *testing.T) {
	stateDir := t.TempDir()
	policy, err := LoadRestorePolicy(stateDir)
	if err != nil || policy.AllowedRootPrefixes != nil {
		t.Fatalf("a missing policy file must yield the fail-closed zero policy, got %+v %v", policy, err)
	}

	metadata := filepath.Join(stateDir, "metadata")
	if err := os.MkdirAll(metadata, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(metadata, "restore-policy.json")
	if err := os.WriteFile(path, []byte(`{"allowed_root_prefixes": ["/data/shift", "/workloads"], "allow_root_workload": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err = LoadRestorePolicy(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.AllowRootWorkload || len(policy.AllowedRootPrefixes) != 2 {
		t.Fatalf("policy file not loaded: %+v", policy)
	}

	if err := os.WriteFile(path, []byte(`{"allowed_root_prefixes": ["/etc"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRestorePolicy(stateDir); err == nil {
		t.Fatal("an allowlist entry inside a system tree must be refused")
	}

	if err := os.WriteFile(path, []byte(`{"allowed_root_prefixes": ["/srv"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRestorePolicy(stateDir); err == nil {
		t.Fatal("a group- or world-writable policy file must be refused")
	}
}
