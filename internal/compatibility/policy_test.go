package compatibility

import (
	"os"
	"path/filepath"
	"testing"

	"shift.dev/shift/internal/model"
)

// compatFixture builds a manifest/destination pair that is compatible except
// for the facts under test: same kernel, linux/amd64, healthy CRIU, ample
// memory and storage.
func compatFixture(kernel string) (model.CheckpointManifest, model.MachineCapabilities) {
	manifest := model.CheckpointManifest{
		Format:        model.StateFormatName,
		FormatVersion: model.StateFormatVersion,
		SourceMachine: model.MachineCapabilities{
			OS: "linux", Architecture: "amd64", Kernel: kernel, Distribution: "ubuntu-24.04", MemoryBytes: 1024,
			Features: map[string]bool{},
		},
		Workload: model.WorkloadSpec{RootPath: "/srv/app"},
	}
	destination := model.MachineCapabilities{
		OS: "linux", Architecture: "amd64", Kernel: kernel, Distribution: "ubuntu-24.04", MemoryBytes: 4096,
		CRIU:     model.CRIUCapabilities{Installed: true, Healthy: true, Version: "4.2"},
		Features: map[string]bool{"gnu_tar": true},
		Storage:  []model.StorageDevice{{Mountpoint: "/", AvailableBytes: 1 << 40}},
	}
	return manifest, destination
}

// TestStateFormatAcceptsSameMajorRange is M-7: a mixed 1.x fleet restores
// each other instead of exact-matching 1.0.0 and bricking at the first
// format bump.
func TestStateFormatAcceptsSameMajorRange(t *testing.T) {
	cases := []struct {
		manifestVersion string
		agentVersion    string
		accepted        bool
		code            string
	}{
		{"1.0.0", "1.0.0", true, ""},
		{"1.0.7", "1.0.0", true, ""}, // newer patch never carries features
		{"1.0.0", "1.3.2", true, ""}, // newer agent reads older manifests
		{"1.2.0", "1.3.0", true, ""}, // older minor on a newer agent
		{"1.4.0", "1.3.0", false, "STATE_FORMAT_UPGRADE_REQUIRED"},
		{"2.0.0", "1.3.0", false, "STATE_FORMAT_UNSUPPORTED"},
		{"0.9.0", "1.3.0", false, "STATE_FORMAT_UNSUPPORTED"},
		{"bogus", "1.0.0", false, "STATE_FORMAT_UNSUPPORTED"},
	}
	for _, c := range cases {
		accepted, code, description, adaptation := stateFormatDecision(c.manifestVersion, c.agentVersion, "1.0.0")
		if accepted != c.accepted || code != c.code {
			t.Fatalf("stateFormatDecision(%q, %q) = (%v, %q), want (%v, %q)", c.manifestVersion, c.agentVersion, accepted, code, c.accepted, c.code)
		}
		if !accepted && (description == "" || adaptation == "") {
			t.Fatalf("refusal for %q must carry a description and an adaptation", c.manifestVersion)
		}
	}
	// And through the checker, end to end.
	manifest, destination := compatFixture("6.8.0")
	manifest.FormatVersion = "1.9.0"
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("newer-minor manifest restored by an older agent")
	}
	found := false
	for _, issue := range report.Issues {
		if issue.Code == "STATE_FORMAT_UPGRADE_REQUIRED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected STATE_FORMAT_UPGRADE_REQUIRED, got %v", issueCodes(report))
	}
	manifest.FormatVersion = "1.0.5"
	report = (Checker{}).Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("same-major manifest refused: %v", issueCodes(report))
	}
}

// TestKernelPolicy is M-8: destination-older is always refused, same-major
// forward is the default envelope, and crossing majors forward is admitted
// only by a certified policy entry — with a precise failure code otherwise.
func TestKernelPolicy(t *testing.T) {
	manifest, destination := compatFixture("6.8.0")

	destination.Kernel = "6.7.0"
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "KERNEL_INCOMPATIBLE") {
		t.Fatalf("older destination kernel accepted: %v", issueCodes(report))
	}

	destination.Kernel = "6.11.0"
	report = (Checker{}).Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("same-major forward move refused: %v", issueCodes(report))
	}

	// Cross-major forward: refused by the default policy with the precise
	// code, admitted once a certified rule records the pair.
	manifest, destination = compatFixture("5.15.0")
	destination.Kernel = "6.8.0"
	report = (Checker{}).Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "KERNEL_CROSS_MAJOR_NOT_CERTIFIED") {
		t.Fatalf("uncertified cross-major move accepted: %v", issueCodes(report))
	}
	policy := DefaultPolicy()
	policy.KernelRules = []KernelRule{{SourceKernel: "5.15", DestinationKernel: "6.8", Certified: true, Evidence: "operator-tested pair"}}
	report = Checker{Policy: policy}.Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("certified cross-major pair refused: %v", issueCodes(report))
	}
	// An uncertified rule records nothing.
	policy.KernelRules = []KernelRule{{SourceKernel: "5.15", DestinationKernel: "6.8", Certified: false}}
	report = Checker{Policy: policy}.Check(manifest, destination)
	if report.Compatible {
		t.Fatal("uncertified rule admitted a cross-major move")
	}

	// Cross-major BACKWARD is refused even with a rule: older kernels never
	// restore newer state.
	manifest, destination = compatFixture("6.8.0")
	destination.Kernel = "5.15.0"
	policy.KernelRules = []KernelRule{{SourceKernel: "6.8", DestinationKernel: "5.15", Certified: true, Evidence: "cannot override CRIU physics"}}
	report = Checker{Policy: policy}.Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "KERNEL_INCOMPATIBLE") {
		t.Fatalf("backward kernel move accepted: %v", issueCodes(report))
	}
}

// TestDistroPolicy: cross-distro warns and admits by default, a refusing rule
// turns it into a precise error, and an allowing rule keeps the warning.
func TestDistroPolicy(t *testing.T) {
	manifest, destination := compatFixture("6.8.0")
	manifest.SourceMachine.Distribution = "ubuntu-24.04"
	destination.Distribution = "debian-12"

	report := (Checker{}).Check(manifest, destination)
	if !report.Compatible || !contains(issueCodes(report), "DISTRIBUTION_DIFFERENCE") {
		t.Fatalf("default cross-distro behavior wrong: %v", issueCodes(report))
	}

	policy := DefaultPolicy()
	policy.CrossDistroDefault = DistroRefuse
	report = Checker{Policy: policy}.Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "DISTRO_INCOMPATIBLE") {
		t.Fatalf("refuse default wrong: %v", issueCodes(report))
	}

	policy = DefaultPolicy()
	policy.CrossDistroDefault = DistroRefuse
	policy.DistroRules = []DistroRule{{SourceDistribution: "ubuntu-24.04", DestinationDistribution: "debian-12", Allow: true, Evidence: "tested pair"}}
	report = Checker{Policy: policy}.Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("allowing rule ignored: %v", issueCodes(report))
	}
	policy.DistroRules = []DistroRule{{SourceDistribution: "ubuntu-24.04", DestinationDistribution: "debian-12", Allow: false}}
	report = Checker{Policy: policy}.Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "DISTRO_INCOMPATIBLE") {
		t.Fatalf("refusing rule ignored: %v", issueCodes(report))
	}
}

// TestLoadPolicyRoundTrip proves a policy file is a real, validated
// configuration surface.
func TestLoadPolicyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	data := `{
		"state_format_floor": "1.0.0",
		"cross_distro_default": "refuse",
		"kernel_rules": [{"source_kernel": "5.15", "destination_kernel": "6.8", "certified": true, "evidence": "fleet tested"}],
		"distro_rules": [{"source_distribution": "ubuntu-22.04", "destination_distribution": "ubuntu-24.04", "allow": true}]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	if policy.CrossDistroDefault != DistroRefuse || len(policy.KernelRules) != 1 || !policy.KernelRules[0].Certified {
		t.Fatalf("policy decoded wrong: %+v", policy)
	}
	if err := os.WriteFile(path, []byte(`{"cross_distro_default":"maybe"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if err := (Policy{CrossDistroDefault: DistroWarn}).Validate(); err == nil {
		t.Fatal("policy without state_format_floor accepted")
	}
}
