package device

import "testing"

func TestParseNvidiaSMIReportsEveryGPU(t *testing.T) {
	output := "NVIDIA GeForce RTX 4090, 550.54.15, 24564, 8.9\n" +
		"NVIDIA A100-SXM4-40GB, 550.54.15, 40960, 8.0\n"
	devices := parseNvidiaSMI(output, "550.54.15", "12.4", true, []string{"/dev/nvidiactl"})
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	first := devices[0]
	if first.Vendor != "NVIDIA" || first.Model != "NVIDIA GeForce RTX 4090" {
		t.Fatalf("unexpected identity: %+v", first)
	}
	if first.DriverVersion != "550.54.15" {
		t.Fatalf("driver version = %q", first.DriverVersion)
	}
	if first.MemoryBytes != 24564*1024*1024 {
		t.Fatalf("memory = %d bytes", first.MemoryBytes)
	}
	if first.ComputeCapability != "8.9" {
		t.Fatalf("compute capability = %q", first.ComputeCapability)
	}
	if first.Runtime != "CUDA 12.4" {
		t.Fatalf("runtime = %q", first.Runtime)
	}
	if !first.CheckpointRestore {
		t.Fatal("a 550-line driver advertises checkpoint/restore")
	}
	if len(first.DeviceFiles) != 1 || first.DeviceFiles[0] != "/dev/nvidiactl" {
		t.Fatalf("device files = %v", first.DeviceFiles)
	}
	if devices[1].ComputeCapability != "8.0" {
		t.Fatalf("second compute capability = %q", devices[1].ComputeCapability)
	}
}

func TestParseNvidiaSMIWithoutCUDAVersion(t *testing.T) {
	devices := parseNvidiaSMI("NVIDIA T4, 470.42, 15360, 7.5\n", "470.42", "", false, nil)
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0].Runtime != "CUDA" {
		t.Fatalf("runtime = %q", devices[0].Runtime)
	}
	if devices[0].CheckpointRestore {
		t.Fatal("a 470-line driver must not advertise checkpoint/restore")
	}
	if devices[0].DeviceFiles != nil {
		t.Fatalf("device files = %v", devices[0].DeviceFiles)
	}
}

func TestParseNvidiaSMISkipsMalformedLines(t *testing.T) {
	output := "NVIDIA H100, 550.54.15, 81559, 9.0\nnot,enough,fields\n\n"
	if devices := parseNvidiaSMI(output, "550.54.15", "", true, nil); len(devices) != 1 {
		t.Fatalf("expected 1 device after skipping bad lines, got %d", len(devices))
	}
}

func TestNormalizeComputeCapability(t *testing.T) {
	cases := map[string]string{"8": "8.0", "8.9": "8.9", "9.0": "9.0", "": "", "N/A": "N/A"}
	for input, expected := range cases {
		if got := normalizeComputeCapability(input); got != expected {
			t.Fatalf("normalizeComputeCapability(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestDriverAtLeast(t *testing.T) {
	cases := []struct {
		version  string
		major    int
		expected bool
	}{
		{"550.54.15", 550, true},
		{"565.57.01", 550, true},
		{"545.23.08", 550, false},
		{"470.42", 550, false},
		{"", 550, false},
		{"unknown", 550, false},
	}
	for _, c := range cases {
		if got := driverAtLeast(c.version, c.major); got != c.expected {
			t.Fatalf("driverAtLeast(%q, %d) = %v, want %v", c.version, c.major, got, c.expected)
		}
	}
}

func TestParseNvidiaSMICUDAVersion(t *testing.T) {
	report := "Driver Version                      : 550.54.15\n" +
		"    CUDA Version                    : 12.4     \n"
	if got := parseNvidiaSMICUDAVersion(report); got != "12.4" {
		t.Fatalf("CUDA version = %q, want 12.4", got)
	}
	if got := parseNvidiaSMICUDAVersion("Driver Version : 550.54.15"); got != "" {
		t.Fatalf("CUDA version = %q, want empty", got)
	}
}

func TestParseNvccVersion(t *testing.T) {
	banner := "nvcc: NVIDIA (R) Cuda compiler driver\n" +
		"Cuda compilation tools, release 12.4, V12.4.131\n" +
		"Build cuda_12.4.r12.4/compiler.3.25917460_0\n"
	if got := parseNvccVersion(banner); got != "12.4" {
		t.Fatalf("nvcc version = %q, want 12.4", got)
	}
	if got := parseNvccVersion("no release line"); got != "" {
		t.Fatalf("nvcc version = %q, want empty", got)
	}
}

func TestRocminfoAgentsSeparatesCPUFromGPUs(t *testing.T) {
	report := "HSA Runtime found\n" +
		"Agent[1] :\n" +
		"  Marketing Name: AMD Ryzen 9 7950X 16-Core Processor\n" +
		"  Name: AMD Ryzen 9 7950X\n" +
		"  Device Type: CPU\n" +
		"Agent[2] :\n" +
		"  Marketing Name: AMD Radeon RX 7900 XTX\n" +
		"  Name: gfx1100\n" +
		"  Device Type: GPU\n" +
		"  Compute Unit: 96\n"
	agents := rocminfoAgents(report)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	if agents[0].gfx != "" {
		t.Fatalf("CPU agent has gfx target %q", agents[0].gfx)
	}
	if agents[1].name != "AMD Radeon RX 7900 XTX" {
		t.Fatalf("GPU marketing name = %q", agents[1].name)
	}
	if agents[1].gfx != "gfx1100" {
		t.Fatalf("GPU gfx target = %q", agents[1].gfx)
	}
}

func TestRocminfoAgentsOnEmptyReport(t *testing.T) {
	if agents := rocminfoAgents(""); len(agents) != 0 {
		t.Fatalf("empty report produced %d agents", len(agents))
	}
	if agents := rocminfoAgents("HSA Runtime not found\n"); len(agents) != 0 {
		t.Fatalf("agent-less report produced %d agents", len(agents))
	}
}

func TestRocminfoAgentsReadsHeaderGfxTarget(t *testing.T) {
	// Some rocminfo builds print the gfx target on the agent header line.
	report := "Agent[1] : gfx90a\n" +
		"  Marketing Name: AMD Instinct MI210\n" +
		"  Device Type: GPU\n"
	agents := rocminfoAgents(report)
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].gfx != "gfx90a" {
		t.Fatalf("gfx target = %q, want gfx90a", agents[0].gfx)
	}
	if agents[0].name != "AMD Instinct MI210" {
		t.Fatalf("marketing name = %q", agents[0].name)
	}
}

func TestAmdDeviceFilesKeepsOnlyExistingNodes(t *testing.T) {
	// On any machine, the returned list can only contain paths that exist;
	// /dev/kfd exists only with the amdgpu driver loaded, render nodes only
	// with DRM. The invariant under test is the filter, not the hardware.
	files := amdDeviceFiles([]string{"/dev/dri/renderD128", "/definitely/not/a/device"})
	for _, path := range files {
		if path == "/definitely/not/a/device" {
			t.Fatal("nonexistent device file survived the filter")
		}
	}
}
