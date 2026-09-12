package compatibility

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
)

type Checker struct{}

func (Checker) Check(manifest model.CheckpointManifest, destination model.MachineCapabilities) model.CompatibilityReport {
	report := model.CompatibilityReport{Compatible: true, CheckedAt: time.Now().UTC()}
	issue := func(code, severity, resource, description, adaptation string) {
		report.Issues = append(report.Issues, model.CompatibilityIssue{
			Code: code, Severity: severity, Resource: resource, Description: description, Adaptation: adaptation,
		})
		if severity == "error" {
			report.Compatible = false
		}
	}
	if manifest.Format != model.StateFormatName || manifest.FormatVersion != model.StateFormatVersion {
		issue("STATE_FORMAT_UNSUPPORTED", "error", "state_format", "destination does not support this SHIFT state format", "upgrade the destination agent")
	}
	if destination.OS != "linux" {
		issue("OS_UNSUPPORTED", "error", "operating_system", "Linux CRIU state cannot be restored on "+destination.OS, "use a Linux destination")
	}
	if destination.Architecture != manifest.SourceMachine.Architecture {
		issue("ARCH_MISMATCH", "error", "cpu", fmt.Sprintf("source is %s and destination is %s", manifest.SourceMachine.Architecture, destination.Architecture), "select a destination with matching architecture")
	}
	if !destination.CRIU.Installed || !destination.CRIU.Healthy {
		issue("CRIU_UNAVAILABLE", "error", "criu", "destination CRIU is unavailable or failed its kernel capability check", "install a compatible CRIU build and satisfy `criu check`")
	}
	if sourceMajor, sourceMinor, ok := kernelVersion(manifest.SourceMachine.Kernel); ok {
		if destinationMajor, destinationMinor, destinationOK := kernelVersion(destination.Kernel); !destinationOK || destinationMajor != sourceMajor || destinationMinor < sourceMinor {
			issue("KERNEL_INCOMPATIBLE", "error", "kernel", fmt.Sprintf("source kernel %s is not conservatively compatible with destination kernel %s", manifest.SourceMachine.Kernel, destination.Kernel), "use the same kernel major with an equal or newer minor version")
		}
	}
	for feature, required := range manifest.SourceMachine.Features {
		if !required || !strings.HasPrefix(feature, "cpu.") {
			continue
		}
		if !destination.Features[feature] {
			issue("CPU_FEATURE_MISSING", "error", feature, "destination CPU lacks a feature used by the source", "choose a CPU-compatible machine")
		}
	}
	requiredMemory := manifest.Workload.Resources.MemoryBytes
	if requiredMemory == 0 {
		requiredMemory = uint64(float64(manifest.RequiredBytes) * 1.15)
	}
	if destination.MemoryBytes < requiredMemory {
		issue("MEMORY_INSUFFICIENT", "error", "memory", fmt.Sprintf("destination has %d bytes; workload requires at least %d", destination.MemoryBytes, requiredMemory), "select a larger destination or lower the workload memory limit")
	}
	requiredStorage := manifest.Workload.Resources.StorageBytes
	if requiredStorage == 0 {
		requiredStorage = uint64(float64(manifest.RequiredBytes) * 1.20)
	}
	if available := AvailableForPath(destination.Storage, manifest.Workload.RootPath); available < requiredStorage {
		issue("STORAGE_INSUFFICIENT", "error", "storage", fmt.Sprintf("target filesystem has %d bytes available; restore requires at least %d", available, requiredStorage), "free space or choose another destination")
	}
	// The workload's device policy decides what an unsatisfiable device
	// requirement does: reject_incompatible (the default) fails the migration,
	// warn_incompatible lets the workload start without the accelerator and
	// says so. A requirement for GPU checkpoint/restore stays an error under
	// both policies — proceeding would silently discard state the checkpoint
	// claims to carry.
	deviceSeverity := "error"
	if manifest.Workload.DevicePolicy == model.DeviceWarnIncompatible {
		deviceSeverity = "warning"
	}
	for _, required := range manifest.DeviceNeeds {
		match := MatchGPU(required, destination.GPUs)
		if match == nil {
			severity := deviceSeverity
			adaptation := "select a machine with a compatible GPU and driver"
			if deviceSeverity == "warning" {
				adaptation = "the workload will start without this device; its device policy is warn_incompatible"
			}
			if required.CheckpointRestore {
				severity = "error"
				adaptation = "select a machine with a GPU that advertises checkpoint/restore"
			}
			issue("GPU_INCOMPATIBLE", severity, "gpu", required.Vendor+" "+required.Model+" is unavailable", adaptation)
			continue
		}
		if required.CheckpointRestore && !match.CheckpointRestore {
			issue("GPU_RESTORE_UNSUPPORTED", "error", "gpu", "destination driver does not advertise GPU checkpoint/restore", "install a vendor-supported checkpoint/restore driver stack")
		}
		if !strings.EqualFold(match.Model, required.Model) {
			issue("GPU_MODEL_DIFFERS", "warning", "gpu", fmt.Sprintf("workload used %s %s; destination offers %s %s", required.Vendor, required.Model, match.Vendor, match.Model), "the workload runs on a different GPU model of equal or greater capability; validate accelerator-sensitive behavior after restore")
		}
	}
	for _, required := range manifest.DeviceNeeds {
		if required.Runtime == "" {
			continue
		}
		family, version, _ := strings.Cut(required.Runtime, " ")
		installed, found := findRuntime(destination.GPURuntimes, family)
		if !found {
			issue("GPU_RUNTIME_MISSING", "warning", "gpu", "destination does not report a "+family+" environment", "install the "+family+" stack on the destination or confirm the workload carries its runtime libraries inside its root")
			continue
		}
		if version != "" && !versionAtLeast(installed.Version, version) {
			installedVersion := installed.Version
			if installedVersion == "" {
				installedVersion = "an unversioned install"
			}
			issue("GPU_RUNTIME_OLDER", "warning", "gpu", fmt.Sprintf("destination %s environment is %s; workload used %s", family, installedVersion, version), "install a newer "+family+" stack on the destination")
		}
	}
	for _, item := range manifest.StateInventory {
		if item.Class == model.StateExternal && item.Required {
			issue("EXTERNAL_STATE_REQUIRED", "error", item.Name, item.Description, "configure a reconnect or external-state adapter")
		}
	}
	if manifest.Workload.NetworkPolicy == model.NetworkPreserve && manifest.SourceMachine.Kernel != destination.Kernel {
		issue("NETWORK_STATE_RISK", "warning", "network", "transparent TCP repair across different kernels is not guaranteed", "use SHIFT reconnect forwarding or identical kernels")
	}
	if manifest.SourceMachine.Distribution != destination.Distribution {
		issue("DISTRIBUTION_DIFFERENCE", "warning", "userspace", "source and destination distributions differ", "use a matching system image when workloads depend on host libraries")
	}
	if !destination.Features["gnu_tar"] {
		issue("ARCHIVE_TOOL_MISSING", "error", "filesystem", "GNU tar is required to restore ACLs, xattrs, sparse files, and ownership", "install GNU tar")
	}
	return report
}

func kernelVersion(value string) (major, minor int, ok bool) {
	parts := strings.SplitN(value, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	return major, minor, errMajor == nil && errMinor == nil
}

// findRuntime locates a GPU software environment by family name ("CUDA",
// "ROCm"), case-insensitively.
func findRuntime(runtimes []model.GPURuntime, family string) (model.GPURuntime, bool) {
	for _, runtime := range runtimes {
		if strings.EqualFold(runtime.Name, family) {
			return runtime, true
		}
	}
	return model.GPURuntime{}, false
}

// versionAtLeast compares dotted numeric versions segment by segment, so
// "12.10" sorts after "12.9" the way operators read version strings. A missing
// version on the destination fails closed; an empty minimum is always
// satisfied.
func versionAtLeast(version, minimum string) bool {
	if minimum == "" {
		return true
	}
	if version == "" {
		return false
	}
	versionParts := strings.Split(version, ".")
	minimumParts := strings.Split(minimum, ".")
	for index, minimumPart := range minimumParts {
		if index >= len(versionParts) {
			return false
		}
		current, errCurrent := strconv.Atoi(versionParts[index])
		required, errRequired := strconv.Atoi(minimumPart)
		if errCurrent != nil || errRequired != nil {
			return false
		}
		if current != required {
			return current > required
		}
	}
	return true
}

// AvailableForPath reports the free bytes of the mounted device that owns
// target. The scheduler and the checker must agree on which filesystem a
// restore would land on, so both call this function.
func AvailableForPath(devices []model.StorageDevice, target string) uint64 {
	target = filepath.Clean(target)
	longest := -1
	var available uint64
	for _, device := range devices {
		mount := filepath.Clean(device.Mountpoint)
		relative, err := filepath.Rel(mount, target)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if len(mount) > longest {
			longest = len(mount)
			available = device.AvailableBytes
		}
	}
	return available
}

// MatchGPU returns the GPU that best satisfies required, or nil when the
// destination cannot serve it. An exact model match always wins over a
// capability match so a machine holding both the requested model and a
// stronger one offers the requested model. Placement scoring and migration
// pre-flight both use this so a scheduled destination cannot pass one check and
// fail the other.
func MatchGPU(required model.GPUDevice, available []model.GPUDevice) *model.GPUDevice {
	var capabilityMatch *model.GPUDevice
	for index := range available {
		candidate := &available[index]
		if !strings.EqualFold(candidate.Vendor, required.Vendor) {
			continue
		}
		if !computeCapabilitySatisfied(required.ComputeCapability, candidate.ComputeCapability) {
			continue
		}
		if candidate.MemoryBytes < required.MemoryBytes {
			continue
		}
		if strings.EqualFold(candidate.Model, required.Model) {
			return candidate
		}
		if capabilityMatch == nil {
			capabilityMatch = candidate
		}
	}
	return capabilityMatch
}

// computeCapabilitySatisfied reports whether a candidate's compute capability
// serves a requirement. NVIDIA compute capabilities are ordered dotted
// numbers, so "10.0" must outrank "9.0" — a plain string comparison gets that
// wrong. AMD gfx targets name an instruction-set architecture rather than a
// level, so only an exact match satisfies a gfx requirement.
func computeCapabilitySatisfied(required, candidate string) bool {
	if required == "" {
		return true
	}
	if isDottedNumber(required) {
		return isDottedNumber(candidate) && versionAtLeast(candidate, required)
	}
	return strings.EqualFold(candidate, required)
}

// isDottedNumber reports whether a value is entirely dot-separated integers.
func isDottedNumber(value string) bool {
	if value == "" {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}
