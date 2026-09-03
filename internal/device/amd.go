package device

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
)

// amdVendorID is AMD's PCI vendor id, as it appears in sysfs.
const amdVendorID = "0x1002"

// amdCard is one AMD GPU discovered through the kernel's DRM topology.
type amdCard struct {
	memoryBytes uint64
	productName string
}

// detectAMD inventories AMD GPUs from sysfs DRM topology, which the amdgpu
// driver maintains without any vendor tool. VRAM comes from the driver's own
// mem_info counters; the model name and gfx compute target come from rocminfo
// when it is installed and its GPU count matches the hardware, because
// SHIFT will not guess a rocminfo-to-card mapping it cannot verify.
func detectAMD(parent context.Context) []model.GPUDevice {
	cards := amdDRMCards()
	if len(cards) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	renderNodes := amdRenderNodes()
	// rocminfo lists the CPU as an agent too; only agents with a gfx target
	// are GPUs, and the model/gfx mapping is used only when rocminfo saw
	// exactly as many GPU agents as the kernel saw AMD cards.
	agents := rocminfoAgents(commandOutput(ctx, "rocminfo"))
	gpuAgents := make([]rocminfoAgent, 0, len(agents))
	for _, agent := range agents {
		if agent.gfx != "" {
			gpuAgents = append(gpuAgents, agent)
		}
	}
	driverVersion := commandOutput(ctx, "modinfo", "-F", "version", "amdgpu")
	runtime := "ROCm"
	if version := rocmInstallVersion(); version != "" {
		runtime = "ROCm " + version
	}
	var devices []model.GPUDevice
	for index, card := range cards {
		// CheckpointRestore stays false: ROCm has no API to checkpoint a
		// process's GPU state, so a GPU-dependent live migration fails the
		// compatibility check instead of pretending the accelerator state
		// traveled.
		device := model.GPUDevice{
			Vendor:            "AMD",
			MemoryBytes:       card.memoryBytes,
			DriverVersion:     driverVersion,
			Runtime:           runtime,
			CheckpointRestore: false,
		}
		device.Model = card.productName
		if len(gpuAgents) == len(cards) {
			if gpuAgents[index].name != "" {
				device.Model = gpuAgents[index].name
			}
			device.ComputeCapability = gpuAgents[index].gfx
		}
		if device.Model == "" {
			device.Model = "AMD GPU"
		}
		device.DeviceFiles = amdDeviceFiles(renderNodes)
		devices = append(devices, device)
	}
	return devices
}

// amdDRMCards reads VRAM and the product name of each AMD card from the
// amdgpu driver's sysfs attributes, via the shared card enumeration.
func amdDRMCards() []amdCard {
	var cards []amdCard
	for _, cardPath := range amdCardPaths() {
		card := amdCard{}
		card.memoryBytes, _ = readSysfsUint(cardPath, "device", "mem_info_vram_total")
		if name, ok := readSysfsString(cardPath, "device", "product_name"); ok {
			card.productName = name
		}
		cards = append(cards, card)
	}
	return cards
}

var cardNameRegexp = regexp.MustCompile(`^card[0-9]+$`)

// amdRenderNodes lists the DRI render nodes currently registered on the
// machine, e.g. /dev/dri/renderD128.
func amdRenderNodes() []string {
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	return existingPaths(matches)
}

// amdDeviceFiles returns the device files a process using AMD GPUs needs: the
// shared /dev/kfd compute interface and the DRI render nodes. Card indices and
// render-node numbers are assigned by the kernel and SHIFT does not pretend to
// know the pairing, so every existing render node is listed: restoring a
// process that opened one of them requires that node to exist on the
// destination, and the checker compares presence, never inode numbers.
func amdDeviceFiles(renderNodes []string) []string {
	candidates := append([]string{"/dev/kfd"}, renderNodes...)
	return existingPaths(candidates)
}

// rocminfoAgent is one agent (typically one GPU) in a rocminfo report.
type rocminfoAgent struct {
	name string
	gfx  string
}

// rocminfoAgents extracts the marketing name and gfx target of every agent
// from a rocminfo report — one "Agent[N]" section per CPU or GPU. Parsing is
// split from detection so it is testable without ROCm installed.
func rocminfoAgents(output string) []rocminfoAgent {
	var agents []rocminfoAgent
	var current *rocminfoAgent
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Agent[") {
			if current != nil {
				agents = append(agents, *current)
			}
			current = &rocminfoAgent{}
			// Some rocminfo builds print the gfx target on the agent header
			// line itself ("Agent[1] : gfx1100").
			if _, value, found := strings.Cut(trimmed, ":"); found {
				if value = strings.TrimSpace(value); strings.HasPrefix(value, "gfx") {
					current.gfx = value
				}
			}
			continue
		}
		if current == nil {
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "Marketing Name":
			current.name = value
		case "Name":
			// The gfx target: "gfx90a", "gfx1100", ... Only overwrite the
			// compute capability when it looks like one.
			if strings.HasPrefix(value, "gfx") {
				current.gfx = value
			}
		}
	}
	if current != nil {
		agents = append(agents, *current)
	}
	return agents
}

// rocmInstallVersion reads ROCm's own version file. The canonical location is
// /opt/rocm/.info/version; some distributions ship /opt/rocm/info/version.
func rocmInstallVersion() string {
	for _, candidate := range []string{"/opt/rocm/.info/version", "/opt/rocm/info/version"} {
		if content, err := os.ReadFile(candidate); err == nil {
			if version := strings.TrimSpace(string(content)); version != "" {
				return version
			}
		}
	}
	return ""
}

// detectROCm reports the ROCm environment installed on the machine: the
// version file under /opt/rocm and the presence of its binaries. As with CUDA,
// the machine must provide the driver (recorded per GPU); the runtime entry
// exists so destinations can be compared.
func detectROCm() model.GPURuntime {
	runtime := model.GPURuntime{Name: "ROCm"}
	if version := rocmInstallVersion(); version != "" {
		runtime.Version = version
	}
	for _, candidate := range []string{"/opt/rocm/bin/rocminfo", "/opt/rocm/bin/rocm-smi"} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			runtime.ToolkitPath = "/opt/rocm"
			break
		}
	}
	if runtime.ToolkitPath == "" {
		// Fall back to PATH discovery for distribution packages that install
		// ROCm tools outside /opt/rocm.
		if _, err := exec.LookPath("rocminfo"); err == nil {
			runtime.ToolkitPath = "PATH"
		}
	}
	runtime.Installed = runtime.Version != "" || runtime.ToolkitPath != ""
	return runtime
}
