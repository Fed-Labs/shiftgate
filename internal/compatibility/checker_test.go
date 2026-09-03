package compatibility

import (
	"testing"

	"shift.dev/shift/internal/model"
)

func TestRejectsMissingCPUFeature(t *testing.T) {
	manifest := model.CheckpointManifest{
		Format:        model.StateFormatName,
		FormatVersion: model.StateFormatVersion,
		SourceMachine: model.MachineCapabilities{
			OS: "linux", Architecture: "amd64", Kernel: "6.8.0", MemoryBytes: 1024,
			Features: map[string]bool{"cpu.avx2": true},
		},
		Workload: model.WorkloadSpec{RootPath: "/srv/app"},
	}
	destination := model.MachineCapabilities{
		OS: "linux", Architecture: "amd64", Kernel: "6.8.1", MemoryBytes: 4096,
		CRIU:     model.CRIUCapabilities{Installed: true, Healthy: true},
		Features: map[string]bool{"gnu_tar": true},
		Storage:  []model.StorageDevice{{Mountpoint: "/", AvailableBytes: 4096}},
	}
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("missing CPU feature was accepted")
	}
}

// gpuFixture builds a manifest/destination pair that is compatible except for
// the GPU facts under test, so each tier can be exercised in isolation.
func gpuFixture(required model.GPUDevice, destinationGPUs []model.GPUDevice, runtimes []model.GPURuntime) (model.CheckpointManifest, model.MachineCapabilities) {
	manifest := model.CheckpointManifest{
		Format:        model.StateFormatName,
		FormatVersion: model.StateFormatVersion,
		SourceMachine: model.MachineCapabilities{
			OS: "linux", Architecture: "amd64", Kernel: "6.8.0", MemoryBytes: 1024,
			Features: map[string]bool{},
		},
		Workload:    model.WorkloadSpec{RootPath: "/srv/app"},
		DeviceNeeds: []model.GPUDevice{required},
	}
	destination := model.MachineCapabilities{
		OS: "linux", Architecture: "amd64", Kernel: "6.8.1", MemoryBytes: 4096,
		CRIU:        model.CRIUCapabilities{Installed: true, Healthy: true},
		Features:    map[string]bool{"gnu_tar": true},
		Storage:     []model.StorageDevice{{Mountpoint: "/", AvailableBytes: 4096}},
		GPUs:        destinationGPUs,
		GPURuntimes: runtimes,
	}
	return manifest, destination
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
	manifest, destination := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := (Checker{}).Check(manifest, destination)
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
	manifest, destination := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := (Checker{}).Check(manifest, destination)
	if contains(issueCodes(report), "GPU_MODEL_DIFFERS") {
		t.Fatalf("identical models produced a warning: %v", issueCodes(report))
	}
}

func TestGPURestoreRequirementRejectsIncapableDestination(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: true}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: false}
	manifest, destination := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("a destination without GPU checkpoint/restore was accepted for a workload that needs it")
	}
	if !contains(issueCodes(report), "GPU_RESTORE_UNSUPPORTED") {
		t.Fatalf("expected GPU_RESTORE_UNSUPPORTED, got %v", issueCodes(report))
	}
}

func TestDevicePolicyWarnDowngradesMissingGPU(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	manifest, destination := gpuFixture(required, nil, nil)
	manifest.Workload.DevicePolicy = model.DeviceWarnIncompatible
	report := (Checker{}).Check(manifest, destination)
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
	manifest, destination := gpuFixture(required, nil, nil)
	manifest.Workload.DevicePolicy = model.DeviceWarnIncompatible
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("a workload whose GPU state cannot be restored was accepted under warn_incompatible")
	}
	// A device that cannot restore is equally fatal under warn policy.
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", CheckpointRestore: false}
	manifest, destination = gpuFixture(required, []model.GPUDevice{offered}, nil)
	manifest.Workload.DevicePolicy = model.DeviceWarnIncompatible
	report = (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("a destination that cannot restore GPU state was accepted under warn_incompatible")
	}
	if !contains(issueCodes(report), "GPU_RESTORE_UNSUPPORTED") {
		t.Fatalf("expected GPU_RESTORE_UNSUPPORTED, got %v", issueCodes(report))
	}
}

func TestDevicePolicyRejectIsTheDefault(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	manifest, destination := gpuFixture(required, nil, nil)
	report := (Checker{}).Check(manifest, destination)
	if report.Compatible {
		t.Fatal("an unavailable GPU was accepted under the default device policy")
	}
}

func TestMissingGPURuntimeWarns(t *testing.T) {
	required := model.GPUDevice{Vendor: "NVIDIA", Model: "H100", Runtime: "CUDA 12.4"}
	offered := model.GPUDevice{Vendor: "NVIDIA", Model: "H100"}
	manifest, destination := gpuFixture(required, []model.GPUDevice{offered}, nil)
	report := (Checker{}).Check(manifest, destination)
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
	manifest, destination := gpuFixture(required, []model.GPUDevice{offered}, runtimes)
	report := (Checker{}).Check(manifest, destination)
	if !contains(issueCodes(report), "GPU_RUNTIME_OLDER") {
		t.Fatalf("older runtime not warned about: %v", issueCodes(report))
	}
}

func TestEqualGPURuntimeProducesNoWarning(t *testing.T) {
	required := model.GPUDevice{Vendor: "AMD", Model: "AMD Radeon RX 7900 XTX", Runtime: "ROCm 6.2"}
	offered := model.GPUDevice{Vendor: "AMD", Model: "AMD Radeon RX 7900 XTX", ComputeCapability: "gfx1100"}
	runtimes := []model.GPURuntime{{Name: "ROCm", Version: "6.2.4", Installed: true}}
	manifest, destination := gpuFixture(required, []model.GPUDevice{offered}, runtimes)
	report := (Checker{}).Check(manifest, destination)
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
