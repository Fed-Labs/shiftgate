package compatibility

import (
	"os"
	"path/filepath"
	"testing"

	"shift.dev/shift/internal/model"
)

// certifiedFixture builds a manifest/destination pair the shipped support
// matrix certifies (linux-amd64-ubuntu-2404), so tests can isolate the facts
// under exercise without tripping platform admission.
func certifiedFixture() (model.CheckpointManifest, model.MachineCapabilities) {
	manifest := model.CheckpointManifest{
		Format:        model.StateFormatName,
		FormatVersion: model.StateFormatVersion,
		SourceMachine: model.MachineCapabilities{
			OS: "linux", Architecture: "amd64", Kernel: "6.8.0", Distribution: "ubuntu-24.04", MemoryBytes: 1024,
			Features: map[string]bool{},
		},
		Workload: model.WorkloadSpec{RootPath: "/srv/app"},
	}
	destination := model.MachineCapabilities{
		OS: "linux", Architecture: "amd64", Kernel: "6.8.1", Distribution: "ubuntu-24.04", MemoryBytes: 4096,
		CRIU:     model.CRIUCapabilities{Installed: true, Healthy: true, Version: "4.2"},
		Features: map[string]bool{"gnu_tar": true},
		Storage:  []model.StorageDevice{{Mountpoint: "/", AvailableBytes: 4096}},
	}
	return manifest, destination
}

func TestRejectsMissingCPUFeature(t *testing.T) {
	manifest, destination := certifiedFixture()
	manifest.SourceMachine.Features = map[string]bool{"cpu.avx2": true}
	checker := Checker{}
	report := checker.Check(manifest, destination)
	if report.Compatible {
		t.Fatal("missing CPU feature was accepted")
	}
}

// gpuFixture builds a manifest/destination pair that is compatible except for
// the GPU facts under test, so each tier can be exercised in isolation. The
// checker's matrix certifies both vendors' GPU rows: the shipped matrix's
// external-hardware-runner policy is exercised separately in
// TestGPUExternalRunnerRequiredByDefault.
func gpuFixture(required model.GPUDevice, destinationGPUs []model.GPUDevice, runtimes []model.GPURuntime) (model.CheckpointManifest, model.MachineCapabilities, Checker) {
	manifest, destination := certifiedFixture()
	manifest.DeviceNeeds = []model.GPUDevice{required}
	destination.GPUs = destinationGPUs
	destination.GPURuntimes = runtimes
	checker := Checker{Support: certifiedGPUMatrix()}
	return manifest, destination, checker
}

// certifiedGPUMatrix is the shipped matrix with both GPU rows certified, the
// shape an operator ships after running the external hardware runner's
// evidence through scripts/verify-support-matrix.sh.
func certifiedGPUMatrix() SupportMatrix {
	matrix := DefaultSupportMatrix()
	matrix.GPUs = []SupportGPU{
		{Vendor: "nvidia", Status: StatusCertified, Evidence: "certified by external hardware runner evidence"},
		{Vendor: "amd", Status: StatusCertified, Evidence: "certified by external hardware runner evidence"},
	}
	return matrix
}

func issueCodes(report model.CompatibilityReport) []string {
	codes := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		codes = append(codes, issue.Code)
	}
	return codes
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestGPUModelDifferenceWarnsInsteadOfRejecting(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "A100-SXM4-40GB", ComputeCapability: "8.0", MemoryBytes: 40 << 30}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", ComputeCapability: "9.0", MemoryBytes: 80 << 30}
	manifest, destination, checker := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := checker.Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("a strictly more capable GPU rejected the migration: %v", issueCodes(report))
	}
	if !contains(issueCodes(report), "GPU_MODEL_DIFFERS") {
		t.Fatalf("model difference not warned about: %v", issueCodes(report))
	}
}

func TestIdenticalGPUModelProducesNoModelWarning(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "A100-SXM4-40GB", ComputeCapability: "8.0", MemoryBytes: 40 << 30}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "A100-SXM4-40GB", ComputeCapability: "8.0", MemoryBytes: 40 << 30, CheckpointRestore: true}
	manifest, destination, checker := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := checker.Check(manifest, destination)
	if contains(issueCodes(report), "GPU_MODEL_DIFFERS") {
		t.Fatalf("identical models produced a warning: %v", issueCodes(report))
	}
}

func TestGPURestoreRequirementRejectsIncapableDestination(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: true}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: false}
	manifest, destination, checker := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := checker.Check(manifest, destination)
	if report.Compatible {
		t.Fatal("a destination without GPU checkpoint/restore was accepted for a workload that needs it")
	}
	if !contains(issueCodes(report), "GPU_RESTORE_UNSUPPORTED") {
		t.Fatalf("expected GPU_RESTORE_UNSUPPORTED, got %v", issueCodes(report))
	}
}

func TestDevicePolicyWarnDowngradesMissingGPU(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	manifest, destination, checker := gpuFixture(required, nil, nil)
	manifest.Workload.DevicePolicy = model.DeviceWarnIncompatible
	report := checker.Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("warn_incompatible rejected a missing device: %v", issueCodes(report))
	}
	for _, issue := range report.Issues {
		if issue.Code == "GPU_INCOMPATIBLE" && issue.Severity != "warning" {
			t.Fatalf("GPU_INCOMPATIBLE severity = %q under warn_incompatible", issue.Severity)
		}
	}
}

func TestDevicePolicyWarnKeepsRestoreRequirementFatal(t *testing.T) {
	// No device at all: the checkpointed GPU state cannot be restored, so
	// warn_incompatible does not downgrade the failure.
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: true}
	manifest, destination, checker := gpuFixture(required, nil, nil)
	manifest.Workload.DevicePolicy = model.DeviceWarnIncompatible
	report := checker.Check(manifest, destination)
	if report.Compatible {
		t.Fatal("a workload whose GPU state cannot be restored was accepted under warn_incompatible")
	}
	// A device that cannot restore is equally fatal under warn policy.
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: false}
	manifest, destination, checker = gpuFixture(required, []model.GPUDevice{offered}, nil)
	manifest.Workload.DevicePolicy = model.DeviceWarnIncompatible
	report = checker.Check(manifest, destination)
	if report.Compatible {
		t.Fatal("a destination that cannot restore GPU state was accepted under warn_incompatible")
	}
	if !contains(issueCodes(report), "GPU_RESTORE_UNSUPPORTED") {
		t.Fatalf("expected GPU_RESTORE_UNSUPPORTED, got %v", issueCodes(report))
	}
}

func TestDevicePolicyRejectIsTheDefault(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	manifest, destination, checker := gpuFixture(required, nil, nil)
	report := checker.Check(manifest, destination)
	if report.Compatible {
		t.Fatal("an unavailable GPU was accepted under the default device policy")
	}
}

func TestMissingGPURuntimeWarns(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", Runtime: "CUDA 12.4"}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	manifest, destination, checker := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := checker.Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("a missing runtime environment rejected the migration: %v", issueCodes(report))
	}
	if !contains(issueCodes(report), "GPU_RUNTIME_MISSING") {
		t.Fatalf("missing runtime not warned about: %v", issueCodes(report))
	}
}

func TestOlderGPURuntimeWarns(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", Runtime: "CUDA 12.4"}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	runtimes := []model.GPURuntime{{Name: "CUDA", Version: "12.1", Installed: true}}
	manifest, destination, checker := gpuFixture(required, []model.GPUDevice{offered}, runtimes)
	report := checker.Check(manifest, destination)
	if !contains(issueCodes(report), "GPU_RUNTIME_OLDER") {
		t.Fatalf("older runtime not warned about: %v", issueCodes(report))
	}
}

func TestEqualGPURuntimeProducesNoWarning(t *testing.T) {
	required := model.GPUDevice{Vendor: "AMD", Model: "AMD Radeon RX 7900 XTX", Runtime: "ROCm 6.2"}
	offered := model.GPUDevice{Vendor: "AMD", Model: "AMD Radeon RX 7900 XTX", ComputeCapability: "gfx1100"}
	runtimes := []model.GPURuntime{{Name: "ROCm", Version: "6.2.4", Installed: true}}
	manifest, destination, checker := gpuFixture(required, []model.GPUDevice{offered}, runtimes)
	report := checker.Check(manifest, destination)
	codes := issueCodes(report)
	if contains(codes, "GPU_RUNTIME_OLDER") || contains(codes, "GPU_RUNTIME_MISSING") {
		t.Fatalf("sufficient runtime produced a warning: %v", codes)
	}
}

func TestVersionAtLeastComparesNumerically(t *testing.T) {
	cases := []struct {
		version  string
		minimum  string
		expected bool
	}{
		{"12.10", "12.9", true},
		{"12.4", "12.10", false},
		{"12.4.1", "12.4", true},
		{"12.4", "12.4", true},
		{"6.2.4", "6.2", true},
		{"", "12.4", false},
		{"12.4", "", true},
		{"unparseable", "12.4", false},
	}
	for _, c := range cases {
		if got := versionAtLeast(c.version, c.minimum); got != c.expected {
			t.Fatalf("versionAtLeast(%q, %q) = %v, want %v", c.version, c.minimum, got, c.expected)
		}
	}
}

func TestMatchGPUPrefersExactModel(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "A100-SXM4-40GB", ComputeCapability: "8.0"}
	available := []model.GPUDevice{
		{Vendor: "NVIDIA", Model: "H100", ComputeCapability: "9.0"},
		{Vendor: "NVIDIA", Model: "A100-SXM4-40GB", ComputeCapability: "8.0"},
	}
	match := MatchGPU(required, available)
	if match == nil || match.Model != "A100-SXM4-40GB" {
		t.Fatalf("exact model not preferred: %+v", match)
	}
}

func TestMatchGPUAcceptsHigherComputeCapabilityNumerically(t *testing.T) {
	// Blackwell's compute capability is 10.0, which sorts before "9.0" as
	// text; the numeric comparison must still accept it.
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", ComputeCapability: "9.0"}
	available := []model.GPUDevice{{Vendor: "NVIDIA", Model: "B200", ComputeCapability: "10.0"}}
	match := MatchGPU(required, available)
	if match == nil {
		t.Fatal("a 10.0-capability GPU failed a 9.0 requirement")
	}
}

func TestMatchGPURejectsLowerComputeCapability(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", ComputeCapability: "9.0"}
	available := []model.GPUDevice{{Vendor: "NVIDIA", Model: "L40S", ComputeCapability: "8.9"}}
	if match := MatchGPU(required, available); match != nil {
		t.Fatalf("an 8.9-capability GPU satisfied a 9.0 requirement: %+v", match)
	}
}

func TestMatchGPURequiresExactGfxTarget(t *testing.T) {
	required := model.GPUDevice{Vendor: "AMD", Model: "AMD Radeon RX 7900 XTX", ComputeCapability: "gfx1100"}
	stronger := []model.GPUDevice{{Vendor: "AMD", Model: "AMD Instinct MI300X", ComputeCapability: "gfx942"}}
	if match := MatchGPU(required, stronger); match != nil {
		t.Fatalf("a different gfx architecture satisfied a gfx1100 requirement: %+v", match)
	}
	exact := []model.GPUDevice{{Vendor: "AMD", Model: "AMD Radeon RX 7900 XTX", ComputeCapability: "gfx1100"}}
	if match := MatchGPU(required, exact); match == nil {
		t.Fatal("an exact gfx target failed its own requirement")
	}
}

func TestMatchGPUIgnoresOtherVendors(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	available := []model.GPUDevice{{Vendor: "AMD", Model: "AMD Instinct MI300X"}}
	if match := MatchGPU(required, available); match != nil {
		t.Fatalf("an AMD GPU satisfied an NVIDIA requirement: %+v", match)
	}
}

// TestCriuVersionFloor pins the documented CRIU policy: the 3.17+ line or the
// 4.x line, and nothing else — the floor the README and deployment guide
// promise and the support matrix certifies.
func TestCriuVersionFloor(t *testing.T) {
	cases := []struct {
		version  string
		supportd bool
	}{
		{"3.17", true},
		{"3.17.1", true},
		{"3.19.0", true},
		{"4.0", true},
		{"4.2", true},
		{"4.2.1", true},
		{"v4.1", true},
		{"Version: 4.2 (git)", true},
		{"3.16.1", false},
		{"3.10", false},
		{"2.0", false},
		{"5.0", false},
		{"5.1.2", false},
		{"", false},
		{"unparseable", false},
	}
	for _, c := range cases {
		if got := criuVersionSupported(c.version); got != c.supportd {
			t.Fatalf("criuVersionSupported(%q) = %v, want %v", c.version, got, c.supportd)
		}
	}
}

func TestCRIUVersionFloorRefusesRestore(t *testing.T) {
	manifest, destination := certifiedFixture()
	destination.CRIU.Version = "3.16.1"
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("CRIU below the 3.17 floor was accepted")
	}
	if !contains(issueCodes(report), "CRIU_VERSION_UNSUPPORTED") {
		t.Fatalf("expected CRIU_VERSION_UNSUPPORTED, got %v", issueCodes(report))
	}
	destination.CRIU.Version = "4.2.1"
	report = (Checker{}).Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("supported CRIU refused: %v", issueCodes(report))
	}
	// A raw banner from an older agent floors the same way.
	destination.CRIU.Version = "Version: 3.10"
	report = (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("raw banner of an unsupported CRIU was accepted")
	}
}

// TestSupportMatrixGate is the runtime enforcement of docs/support-matrix.json:
// certified platforms are clean, unknown combinations are named on every
// report instead of silently treated as compatible, and rows the matrix calls
// unsupported are refused outright.
func TestSupportMatrixGate(t *testing.T) {
	manifest, destination := certifiedFixture()
	report := (Checker{}).Check(manifest, destination)
	if !report.Compatible || contains(issueCodes(report), "PLATFORM_NOT_CERTIFIED") {
		t.Fatalf("certified ubuntu-24.04/6.8/CRIU 4.2 platform was not clean: %v", issueCodes(report))
	}

	destination.Distribution = "nixos-24.11"
	report = (Checker{}).Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("unknown platform hard-failed at the gate: %v", issueCodes(report))
	}
	if !contains(issueCodes(report), "PLATFORM_NOT_CERTIFIED") {
		t.Fatalf("unknown platform was silently treated as compatible: %v", issueCodes(report))
	}
	for _, issue := range report.Issues {
		if issue.Code == "PLATFORM_NOT_CERTIFIED" && issue.Severity != "warning" {
			t.Fatalf("PLATFORM_NOT_CERTIFIED severity = %q", issue.Severity)
		}
	}

	// The untested ubuntu-22.04 row: experimental, named, still not silent.
	manifest, destination = certifiedFixture()
	manifest.SourceMachine.Distribution = "ubuntu-22.04"
	manifest.SourceMachine.Kernel = "5.15.0"
	destination.Distribution = "ubuntu-22.04"
	destination.Kernel = "5.15.0"
	destination.CRIU.Version = "3.17.1"
	report = (Checker{}).Check(manifest, destination)
	if !report.Compatible || !contains(issueCodes(report), "PLATFORM_EXPERIMENTAL") {
		t.Fatalf("untested row decided wrong: %v", issueCodes(report))
	}

	// A row the matrix calls unsupported is a hard refusal.
	matrix := DefaultSupportMatrix()
	matrix.Platforms = append(matrix.Platforms, SupportPlatform{
		ID: "banned", Distribution: "bannedos-1", Architecture: "amd64",
		KernelMin: "6.8", Status: StatusUnsupported, Evidence: "known-bad",
	})
	manifest, destination = certifiedFixture()
	destination.Distribution = "bannedos-1"
	report = Checker{Support: matrix}.Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "PLATFORM_UNSUPPORTED") {
		t.Fatalf("unsupported row admitted: %v", issueCodes(report))
	}
}

// TestGPUExternalRunnerRequiredByDefault is the gpus block made real: the
// shipped matrix certifies no local GPU hardware, so a workload that needs a
// GPU is refused until an operator certifies the row (as gpuFixture does).
func TestGPUExternalRunnerRequiredByDefault(t *testing.T) {
	manifest, destination := certifiedFixture()
	manifest.DeviceNeeds = []model.GPUDevice{{Vendor: "NVIDIA", Model: "H100"}}
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("local GPU restore was admitted against the shipped matrix")
	}
	if !contains(issueCodes(report), "GPU_EXTERNAL_RUNNER_REQUIRED") {
		t.Fatalf("expected GPU_EXTERNAL_RUNNER_REQUIRED, got %v", issueCodes(report))
	}
	// Without device needs the matrix stays quiet about GPUs.
	manifest.DeviceNeeds = nil
	report = (Checker{}).Check(manifest, destination)
	if contains(issueCodes(report), "GPU_EXTERNAL_RUNNER_REQUIRED") {
		t.Fatalf("GPU policy fired without device needs: %v", issueCodes(report))
	}
}

func TestArchUnsupportedOutsideX8664(t *testing.T) {
	manifest, destination := certifiedFixture()
	manifest.SourceMachine.Architecture = "arm64"
	destination.Architecture = "arm64"
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("arm64 destination accepted")
	}
	codes := issueCodes(report)
	if !contains(codes, "ARCH_UNSUPPORTED") {
		t.Fatalf("expected ARCH_UNSUPPORTED, got %v", codes)
	}
}

func TestStateFormatFloorComparesSegmentWise(t *testing.T) {
	accepted, code, _, _ := stateFormatDecision("1.0.0", "1.9.0", "1.5.0")
	if accepted || code != "STATE_FORMAT_UNSUPPORTED" {
		t.Fatalf("manifest below the minor floor was accepted (accepted=%v code=%q)", accepted, code)
	}
	accepted, code, _, _ = stateFormatDecision("1.5.0", "1.9.0", "1.5.0")
	if !accepted {
		t.Fatalf("manifest at the floor refused (code %q)", code)
	}
	accepted, code, _, _ = stateFormatDecision("1.6.2", "1.9.0", "1.5.0")
	if !accepted {
		t.Fatalf("manifest above the floor refused (code %q)", code)
	}
}

func TestDistributionKey(t *testing.T) {
	cases := []struct{ value, want string }{
		{"ubuntu-24.04", "ubuntu-24.04"},
		{"Ubuntu 24.04.1 LTS", "ubuntu-24.04"},
		{"Ubuntu 24.04.2 LTS", "ubuntu-24.04"},
		{"debian-12", "debian-12"},
		{"Debian GNU/Linux 12 (bookworm)", "debian-12"},
		{"kali-rolling", "kali-rolling"},
		{"linux", "linux"},
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := distributionKey(c.value); got != c.want {
			t.Fatalf("distributionKey(%q) = %q, want %q", c.value, got, c.want)
		}
	}
}

func TestPointReleaseIsTheSameDistribution(t *testing.T) {
	manifest, destination := certifiedFixture()
	manifest.SourceMachine.Distribution = "Ubuntu 24.04.1 LTS"
	destination.Distribution = "Ubuntu 24.04.2 LTS"
	report := (Checker{}).Check(manifest, destination)
	for _, issue := range report.Issues {
		if issue.Code == "DISTRIBUTION_DIFFERENCE" || issue.Code == "DISTRO_INCOMPATIBLE" {
			t.Fatalf("point releases of one line looked like different distributions: %v", issueCodes(report))
		}
	}
	// And a PRETTY_NAME destination still matches the id-version matrix row.
	if !report.Compatible || contains(issueCodes(report), "PLATFORM_NOT_CERTIFIED") {
		t.Fatalf("PRETTY_NAME probe missed the certified row: %v", issueCodes(report))
	}
}

func TestDistroRuleMatchesNormalizedForm(t *testing.T) {
	manifest, destination := certifiedFixture()
	manifest.SourceMachine.Distribution = "Ubuntu 24.04.1 LTS"
	destination.Distribution = "Debian GNU/Linux 12 (bookworm)"
	policy := DefaultPolicy()
	policy.CrossDistroDefault = DistroRefuse
	policy.DistroRules = []DistroRule{{SourceDistribution: "ubuntu-24.04", DestinationDistribution: "debian-12", Allow: true, Evidence: "tested pair"}}
	report := Checker{Policy: policy}.Check(manifest, destination)
	if !report.Compatible {
		t.Fatalf("rule written in matrix vocabulary never fired for PRETTY_NAME inputs: %v", issueCodes(report))
	}
}

func TestInvalidPolicyIsARefusalNotASilentFallback(t *testing.T) {
	manifest, destination := certifiedFixture()
	checker := Checker{Policy: Policy{StateFormatFloor: "1.0.0", CrossDistroDefault: "maybe"}}
	report := checker.Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "POLICY_INVALID") {
		t.Fatalf("invalid policy silently accepted: %v", issueCodes(report))
	}
}

func TestPolicyFileFromEnvironment(t *testing.T) {
	manifest, destination := certifiedFixture()
	manifest.SourceMachine.Distribution = "ubuntu-24.04"
	destination.Distribution = "debian-12"
	path := filepath.Join(t.TempDir(), "policy.json")
	data := `{"state_format_floor":"1.0.0","cross_distro_default":"refuse"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHIFT_COMPATIBILITY_POLICY", path)
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "DISTRO_INCOMPATIBLE") {
		t.Fatalf("policy file from SHIFT_COMPATIBILITY_POLICY not honored: %v", issueCodes(report))
	}
	// A broken policy file fails closed with a precise code.
	if err := os.WriteFile(path, []byte(`{"cross_distro_default":"maybe"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report = (Checker{}).Check(manifest, destination)
	if report.Compatible || !contains(issueCodes(report), "POLICY_INVALID") {
		t.Fatalf("broken policy file silently ignored: %v", issueCodes(report))
	}
}

// TestKernelInRange covers the numeric range helper the support matrix rows
// use.
func TestKernelInRange(t *testing.T) {
	cases := []struct {
		observed, minimum, maximum string
		want                       bool
	}{
		{"6.8.0-45-generic", "6.8", "6.14", true},
		{"6.14.0", "6.8", "6.14", true},
		{"6.14.1", "6.8", "6.14", true},
		{"6.15.0", "6.8", "6.14", false},
		{"6.7.9", "6.8", "", false},
		{"6.8.0", "6.8", "", true},
		{"5.15.0-100-generic", "5.15", "5.15", true},
	}
	for _, c := range cases {
		if got := kernelInRange(c.observed, c.minimum, c.maximum); got != c.want {
			t.Fatalf("kernelInRange(%q, %q, %q) = %v, want %v", c.observed, c.minimum, c.maximum, got, c.want)
		}
	}
}
