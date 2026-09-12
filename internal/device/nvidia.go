package device

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
)

// nvidiaDeviceFiles lists the character devices a process using an NVIDIA GPU
// needs available. Restore compatibility means these exist on the destination.
func nvidiaDeviceFiles() []string {
	candidates := []string{"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools", "/dev/nvidia-modeset"}
	matches, _ := filepath.Glob("/dev/nvidia[0-9]*")
	candidates = append(candidates, matches...)
	return existingPaths(candidates)
}

func existingPaths(candidates []string) []string {
	var paths []string
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			paths = append(paths, candidate)
		}
	}
	return paths
}

// detectNVIDIA inventories NVIDIA GPUs through nvidia-smi, the standard
// control and monitoring binary every NVIDIA driver install ships. Without
// nvidia-smi there is no driver, and SHIFT reports no NVIDIA devices rather
// than probing PCI ids it cannot verify.
func detectNVIDIA(parent context.Context) []model.GPUDevice {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path,
		"--query-gpu=name,driver_version,memory.total,compute_cap",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	driverVersion := nvidiaDriverVersion()
	cudaVersion := nvidiaSMICUDAVersion(parent, path)
	deviceFiles := nvidiaDeviceFiles()
	// CUDA checkpoint/restore APIs shipped with the CUDA 12.4 toolkit and the
	// R550 driver line. Advertise the capability only when the installed
	// driver is at least that line: the version is a conservative, verifiable
	// fact, unlike a marketing name.
	checkpointRestore := driverAtLeast(driverVersion, 550)
	return parseNvidiaSMI(string(output), driverVersion, cudaVersion, checkpointRestore, deviceFiles)
}

// parseNvidiaSMI turns nvidia-smi's CSV output into device records. Split out
// from detectNVIDIA so the parsing is testable without a GPU.
func parseNvidiaSMI(output, driverVersion, cudaVersion string, checkpointRestore bool, deviceFiles []string) []model.GPUDevice {
	var devices []model.GPUDevice
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		memoryMiB, _ := strconv.ParseUint(strings.TrimSpace(fields[2]), 10, 64)
		device := model.GPUDevice{
			Vendor:            "NVIDIA",
			Model:             strings.TrimSpace(fields[0]),
			DriverVersion:     driverVersion,
			MemoryBytes:       memoryMiB * 1024 * 1024,
			ComputeCapability: normalizeComputeCapability(strings.TrimSpace(fields[3])),
			Runtime:           "CUDA",
			CheckpointRestore: checkpointRestore,
		}
		if cudaVersion != "" {
			device.Runtime = "CUDA " + cudaVersion
		}
		if len(deviceFiles) > 0 {
			device.DeviceFiles = deviceFiles
		}
		devices = append(devices, device)
	}
	return devices
}

// normalizeComputeCapability renders nvidia-smi's "8" or "8.9" as the dotted
// form the compatibility checker compares lexicographically.
func normalizeComputeCapability(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, ".") {
		if _, err := strconv.Atoi(value); err == nil {
			return value + ".0"
		}
	}
	return value
}

// nvidiaDriverVersion reads the kernel driver version straight from the
// driver's own procfs file, which exists even when nvidia-smi quirks differ.
func nvidiaDriverVersion() string {
	content, err := os.ReadFile("/proc/driver/nvidia/version")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(line, "NVRM version:") {
			// "NVRM version: NVIDIA UNIX x86_64 Kernel Module  550.54.15  ..."
			for _, field := range strings.Fields(line) {
				if versionRegexp.MatchString(field) {
					return field
				}
			}
		}
	}
	return ""
}

var versionRegexp = regexp.MustCompile(`^\d+\.\d+(\.\d+)*$`)

// driverAtLeast reports whether a dotted version string is at least the given
// major line. Unparseable versions are conservatively false: SHIFT does not
// advertise a capability it could not verify.
func driverAtLeast(version string, major int) bool {
	if version == "" {
		return false
	}
	first, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if err != nil {
		return false
	}
	return first >= major
}

// nvidiaSMICUDAVersion asks the driver binary which CUDA version it supports.
// The value is informative — the toolkit version is detected separately — but
// recording it lets operators compare source and destination driver stacks.
func nvidiaSMICUDAVersion(parent context.Context, path string) string {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "-q").Output()
	if err != nil {
		return ""
	}
	return parseNvidiaSMICUDAVersion(string(output))
}

// parseNvidiaSMICUDAVersion finds the first "CUDA Version : X.Y" line of an
// `nvidia-smi -q` report. Split out for testing without a GPU.
func parseNvidiaSMICUDAVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		key, value, found := strings.Cut(trimmed, ":")
		if !found || strings.TrimSpace(key) != "CUDA Version" {
			continue
		}
		value = strings.TrimSpace(value)
		if versionRegexp.MatchString(value) {
			return value
		}
	}
	return ""
}

// detectCUDA finds a CUDA toolkit installation: nvcc on PATH or a standard
// toolkit directory. The toolkit matters for building, not for restoring, so
// this is recorded for visibility and destination comparison, never required.
func detectCUDA(parent context.Context) model.GPURuntime {
	runtime := model.GPURuntime{Name: "CUDA"}
	if path, err := exec.LookPath("nvcc"); err == nil {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, path, "--version").Output()
		if err == nil {
			runtime.Version = parseNvccVersion(string(output))
			runtime.ToolkitPath = filepath.Dir(filepath.Dir(path))
		}
	}
	if toolkit, version := cudaToolkitDirectory(); toolkit != "" {
		if runtime.ToolkitPath == "" {
			runtime.ToolkitPath = toolkit
		}
		if runtime.Version == "" {
			runtime.Version = version
		}
	}
	runtime.Installed = runtime.ToolkitPath != ""
	return runtime
}

// parseNvccVersion extracts the release string from nvcc's version banner.
func parseNvccVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		index := strings.Index(line, "release ")
		if index < 0 {
			continue
		}
		rest := strings.TrimSpace(line[index+len("release "):])
		if comma := strings.Index(rest, ","); comma >= 0 {
			rest = rest[:comma]
		}
		// The release token is a dotted version ("12.4"). Anything else is
		// prose that happens to contain the word release, not the banner.
		if rest == "" || rest[0] < '0' || rest[0] > '9' {
			continue
		}
		return rest
	}
	return ""
}

// cudaToolkitDirectory looks for a standard toolkit install under /usr/local.
func cudaToolkitDirectory() (directory, version string) {
	matches, _ := filepath.Glob("/usr/local/cuda*")
	var best string
	for _, candidate := range matches {
		info, err := os.Stat(candidate)
		if err != nil || !info.IsDir() {
			continue
		}
		if candidate == "/usr/local/cuda" {
			// The unversioned symlink is the operator's default toolkit.
			if target, err := filepath.EvalSymlinks(candidate); err == nil {
				best = target
			}
			continue
		}
		if len(candidate) > len(best) {
			best = candidate
		}
	}
	if best == "" {
		return "", ""
	}
	version = strings.TrimPrefix(filepath.Base(best), "cuda-")
	if version == filepath.Base(best) {
		version = ""
	}
	return best, version
}

// commandOutput runs a command and returns its trimmed stdout, or "" if the
// command is missing or fails.
func commandOutput(ctx context.Context, name string, arguments ...string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
