package compatibility

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shift.dev/shift/internal/model"
)

type Checker struct {
	// Policy is the compatibility envelope. The zero value uses
	// DefaultPolicy; operators widen it with LoadPolicy after certifying
	// their own kernel and distribution pairs, or by pointing the
	// SHIFT_COMPATIBILITY_POLICY environment variable at a policy file.
	Policy Policy
	// Support is the release support contract. The zero value enforces
	// DefaultSupportMatrix — the decisions shipped in docs/support-matrix.json.
	Support SupportMatrix
}

func (c Checker) support() SupportMatrix {
	if c.Support.Version == "" {
		return DefaultSupportMatrix()
	}
	return c.Support
}

// SupportDecision answers the support contract for one destination machine:
// which matrix row (or unknown fallback) governs it and whether unattended
// operations may run there. Automatic paths — placement, migration admission,
// failover — must refuse when AllowAutomatic is false.
func (c Checker) SupportDecision(destination model.MachineCapabilities) SupportDecision {
	return c.support().Decide(supportEvidence(destination))
}

// supportEvidence projects a destination's capabilities onto the probe
// vocabulary the matrix matches against.
func supportEvidence(destination model.MachineCapabilities) SupportEvidence {
	evidence := SupportEvidence{
		OS:           destination.OS,
		Architecture: destination.Architecture,
		Distribution: destination.Distribution,
		Kernel:       destination.Kernel,
		CRIU:         criuVersionNumber(destination.CRIU.Version),
		CRIUCheck:    "passed",
	}
	if !destination.CRIU.Installed || !destination.CRIU.Healthy {
		evidence.CRIUCheck = "failed: destination CRIU is unavailable or failed its kernel capability check"
	}
	return evidence
}

func (c Checker) resolvedPolicy() (Policy, error) {
	if c.Policy.StateFormatFloor == "" && c.Policy.CrossDistroDefault == "" && len(c.Policy.KernelRules) == 0 && len(c.Policy.DistroRules) == 0 {
		if path := strings.TrimSpace(os.Getenv("SHIFT_COMPATIBILITY_POLICY")); path != "" {
			loaded, err := LoadPolicy(path)
			if err != nil {
				return DefaultPolicy(), fmt.Errorf("compatibility policy %s is unusable: %w", path, err)
			}
			return loaded, nil
		}
		return DefaultPolicy(), nil
	}
	if err := c.Policy.Validate(); err != nil {
		return DefaultPolicy(), err
	}
	return c.Policy, nil
}

func (c Checker) policy() Policy {
	policy, _ := c.resolvedPolicy()
	return policy
}

func (c Checker) Check(manifest model.CheckpointManifest, destination model.MachineCapabilities) model.CompatibilityReport {
	policy, policyErr := c.resolvedPolicy()
	report := model.CompatibilityReport{Compatible: true, CheckedAt: time.Now().UTC()}
	issue := func(code, severity, resource, description, adaptation string) {
		report.Issues = append(report.Issues, model.CompatibilityIssue{
			Code: code, Severity: severity, Resource: resource, Description: description, Adaptation: adaptation,
		})
		if severity == "error" {
			report.Compatible = false
		}
	}
	if policyErr != nil {
		issue("POLICY_INVALID", "error", "policy", policyErr.Error(), "fix or remove the compatibility policy file named by SHIFT_COMPATIBILITY_POLICY")
	}
	// State-format compatibility is range-based within the format's major
	// line: a mixed-version fleet restores each other's 1.x checkpoints, an
	// older destination refuses only manifests written by a newer minor
	// (they may carry features it cannot read), and a different major is
	// simply a different format.
	if manifest.Format != model.StateFormatName {
		issue("STATE_FORMAT_UNSUPPORTED", "error", "state_format", "checkpoint is not a SHIFT state file (format "+manifest.Format+")", "restore with the SHIFT agent that wrote it")
	} else if accepted, code, description, adaptation := stateFormatDecision(manifest.FormatVersion, model.StateFormatVersion, policy.StateFormatFloor); !accepted {
		issue(code, "error", "state_format", description, adaptation)
	}
	if destination.OS != "linux" {
		issue("OS_UNSUPPORTED", "error", "operating_system", "Linux CRIU state cannot be restored on "+destination.OS, "use a Linux destination")
	}
	if destination.Architecture != manifest.SourceMachine.Architecture {
		issue("ARCH_MISMATCH", "error", "cpu", fmt.Sprintf("source is %s and destination is %s", manifest.SourceMachine.Architecture, destination.Architecture), "select a destination with matching architecture")
	}
	if destination.Architecture != "amd64" {
		issue("ARCH_UNSUPPORTED", "error", "cpu", "SHIFT supports Linux x86_64 only; "+destination.Architecture+" destinations are outside the support contract", "use an x86_64 destination")
	}
	if !destination.CRIU.Installed || !destination.CRIU.Healthy {
		issue("CRIU_UNAVAILABLE", "error", "criu", "destination CRIU is unavailable or failed its kernel capability check", "install a compatible CRIU build and satisfy `criu check`")
	} else if !criuVersionSupported(destination.CRIU.Version) {
		issue("CRIU_VERSION_UNSUPPORTED", "error", "criu", fmt.Sprintf("destination CRIU %q does not satisfy the supported floor: CRIU 3.17+ or 4.x (see docs/support-matrix.json)", criuVersionNumber(destination.CRIU.Version)), "install CRIU 3.17 or newer from the 3.x line, or any 4.x release")
	}
	// The release support contract (docs/support-matrix.json) decides whether
	// this platform is certified at all. Unknown combinations are never
	// silently treated as compatible: the gate names the matrix decision on
	// every report, and it is an error on rows the matrix calls unsupported.
	if destination.OS == "linux" && destination.Architecture == "amd64" && destination.CRIU.Installed && destination.CRIU.Healthy {
		decision := c.support().Decide(supportEvidence(destination))
		switch {
		case decision.Action == ActionAllow:
		case decision.Status == StatusUnsupported:
			issue("PLATFORM_UNSUPPORTED", "error", "platform", decision.Reason, "the support matrix marks this platform unsupported; do not restore here")
		case decision.Action == ActionExperimentalOnly:
			issue("PLATFORM_EXPERIMENTAL", "warning", "platform", decision.Reason+" (policy: experimental_only — unattended operations are blocked)", "run scripts/verify-support-matrix.sh to certify this combination, or treat this restore as an experiment")
		default:
			issue("PLATFORM_NOT_CERTIFIED", "warning", "platform", decision.Reason+" (policy: "+decision.Action+" — unattended operations are blocked)", "run scripts/verify-support-matrix.sh to certify this combination and add it to docs/support-matrix.json")
		}
	}
	// Kernel policy, in the order CRIU's own restore constraints dictate:
	// never restore onto an older kernel; the same major forward is the
	// safe envelope; crossing majors forward is admitted only by a certified
	// policy rule.
	sourceKernel := versionParts(manifest.SourceMachine.Kernel)
	destinationKernel := versionParts(destination.Kernel)
	if len(sourceKernel) == 0 || len(destinationKernel) == 0 {
		issue("KERNEL_INCOMPATIBLE", "error", "kernel", fmt.Sprintf("cannot compare source kernel %q with destination kernel %q", manifest.SourceMachine.Kernel, destination.Kernel), "report kernel releases as dotted numeric versions")
	} else if compareVersions(destinationKernel, sourceKernel) < 0 {
		issue("KERNEL_INCOMPATIBLE", "error", "kernel", fmt.Sprintf("source kernel %s is newer than destination kernel %s; CRIU state cannot be restored onto an older kernel", manifest.SourceMachine.Kernel, destination.Kernel), "restore onto the same or a newer kernel")
	} else if sourceKernel[0] != destinationKernel[0] && !certifiedKernelPair(policy, manifest.SourceMachine.Kernel, destination.Kernel) {
		issue("KERNEL_CROSS_MAJOR_NOT_CERTIFIED", "error", "kernel", fmt.Sprintf("source kernel %s to destination kernel %s crosses a kernel major without a certified policy entry", manifest.SourceMachine.Kernel, destination.Kernel), "certify this kernel pair in a compatibility policy file (SHIFT_COMPATIBILITY_POLICY=<path>, see compatibility.Policy) after running checkpoint/restore tests for it, or stay within one kernel major")
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
	// The support matrix's GPU rows decide whether local GPU restore is
	// certified at all: the shipped matrix certifies none, so a workload that
	// needs a GPU is governed by the gpu_evidence policy — an external
	// hardware runner, not this gate. A matrix that certifies a vendor's row
	// hands the decision to the device-tier checks below.
	support := c.support()
	for _, required := range manifest.DeviceNeeds {
		decision := support.GPUDecision(required.Vendor)
		if decision.Action == ActionAllow {
			continue
		}
		if decision.Action == ActionExternalGPURequired {
			issue("GPU_EXTERNAL_RUNNER_REQUIRED", "error", "gpu", decision.Reason, "run this GPU restore through the external hardware runner required by the support matrix; local GPU checkpoint/restore is not certified")
			continue
		}
		severity := "warning"
		if decision.Action == ActionBlockAutomatic {
			severity = "error"
		}
		issue("GPU_NOT_CERTIFIED", severity, "gpu", decision.Reason, "the support matrix does not certify this GPU row for restore; run scripts/verify-support-matrix.sh or use the external hardware runner")
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
	if distributionKey(manifest.SourceMachine.Distribution) != distributionKey(destination.Distribution) {
		// Cross-distribution restores keep userland assumptions the checker
		// cannot see inside a checkpoint. Policy decides: the default warns
		// and admits; a policy can refuse a specific pair (or all of them)
		// with a precise code instead of a shrug.
		allowed, refusedByRule := distroAllowed(policy, manifest.SourceMachine.Distribution, destination.Distribution)
		if !allowed {
			code := "DISTRO_INCOMPATIBLE"
			description := fmt.Sprintf("source distribution %s to destination %s is refused by the compatibility policy", manifest.SourceMachine.Distribution, destination.Distribution)
			if !refusedByRule {
				description = fmt.Sprintf("cross-distribution restore from %s to %s is refused by policy (cross_distro_default=refuse)", manifest.SourceMachine.Distribution, destination.Distribution)
			}
			issue(code, "error", "userspace", description, "use a matching system image, or add a certified distro pair to a compatibility policy file (SHIFT_COMPATIBILITY_POLICY=<path>, see compatibility.Policy)")
		} else {
			issue("DISTRIBUTION_DIFFERENCE", "warning", "userspace", "source and destination distributions differ", "use a matching system image when workloads depend on host libraries")
		}
	}
	if !destination.Features["gnu_tar"] {
		issue("ARCHIVE_TOOL_MISSING", "error", "filesystem", "GNU tar is required to restore ACLs, xattrs, sparse files, and ownership", "install GNU tar")
	}
	return report
}

// stateFormatDecision answers whether a manifest's format version is
// restorable here. Compatibility is range-based within the format's major
// line (semver-style): every 1.x manifest is readable by every 1.x agent at
// or above its floor, except that a manifest written by a newer minor than
// this build may carry features it cannot honor and is refused with a
// precise upgrade error rather than a blanket mismatch. Patch releases never
// carry features, so a newer patch is always readable.
func stateFormatDecision(manifestVersion, agentVersion, floor string) (accepted bool, code, description, adaptation string) {
	manifestParts := versionParts(manifestVersion)
	agentParts := versionParts(agentVersion)
	floorParts := versionParts(floor)
	if len(manifestParts) == 0 || len(agentParts) == 0 {
		return false, "STATE_FORMAT_UNSUPPORTED", "checkpoint state format version " + manifestVersion + " is not a dotted numeric version", "upgrade the destination agent"
	}
	if len(floorParts) > 0 && compareVersions(manifestParts, floorParts) < 0 {
		return false, "STATE_FORMAT_UNSUPPORTED", fmt.Sprintf("checkpoint state format %s is older than the oldest supported %s", manifestVersion, floor), "restore with an agent from the era that wrote the checkpoint"
	}
	if manifestParts[0] != agentParts[0] {
		return false, "STATE_FORMAT_UNSUPPORTED", fmt.Sprintf("checkpoint state format %s is a different format line than %s", manifestVersion, agentVersion), "upgrade the destination agent to the checkpoint's format line"
	}
	if len(manifestParts) > 1 && len(agentParts) > 1 && manifestParts[1] > agentParts[1] {
		return false, "STATE_FORMAT_UPGRADE_REQUIRED", fmt.Sprintf("checkpoint state format %s was written by a newer minor version than this agent supports (%s); it may carry features this agent cannot restore", manifestVersion, agentVersion), "upgrade the destination agent to at least " + fmt.Sprintf("%d.%d", manifestParts[0], manifestParts[1]) + ".0 before restoring"
	}
	return true, "", "", ""
}

// certifiedKernelPair reports whether the policy records a certified
// cross-major source→destination kernel line pair. Rules match on dotted
// prefixes ("5.15" matches 5.15.0-91-generic); only Certified rules admit.
func certifiedKernelPair(policy Policy, sourceKernel, destinationKernel string) bool {
	for _, rule := range policy.KernelRules {
		if rule.Certified && kernelUnder(sourceKernel, rule.SourceKernel) && kernelUnder(destinationKernel, rule.DestinationKernel) {
			return true
		}
	}
	return false
}

// distroAllowed resolves the cross-distribution question through the policy:
// an explicit rule wins (its Allow decides, and a refusing rule is reported
// as refusedByRule), and an unruled pair falls to CrossDistroDefault. Rules
// and observations match in the normalized id-version vocabulary, so a rule
// written as "ubuntu-24.04" fires for PRETTY_NAME probes too.
func distroAllowed(policy Policy, sourceDistribution, destinationDistribution string) (allowed, refusedByRule bool) {
	source := distributionKey(sourceDistribution)
	destination := distributionKey(destinationDistribution)
	for _, rule := range policy.DistroRules {
		if distributionKey(rule.SourceDistribution) == source && distributionKey(rule.DestinationDistribution) == destination {
			return rule.Allow, !rule.Allow
		}
	}
	return policy.CrossDistroDefault != DistroRefuse, false
}

// kernelUnder reports whether an observed kernel release falls under a
// dotted prefix: "6.8" covers "6.8.0-45-generic"; major-only "6" covers the
// whole line.
func kernelUnder(observed, prefix string) bool {
	observedParts := versionParts(observed)
	prefixParts := versionParts(prefix)
	if len(prefixParts) == 0 || len(observedParts) < len(prefixParts) {
		return false
	}
	return compareVersions(observedParts[:len(prefixParts)], prefixParts) == 0
}

// versionParts parses the dotted numeric prefix of a version string,
// tolerating kernel-style suffixes ("6.8.0-45-generic", "6.2.4+1").
func versionParts(value string) []int {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	var parts []int
	for _, segment := range strings.Split(value, ".") {
		number, err := strconv.Atoi(segment)
		if err != nil {
			break
		}
		parts = append(parts, number)
	}
	return parts
}

// compareVersions orders dotted numeric versions segment by segment,
// treating a missing segment as zero so "6.8" equals "6.8.0".
func compareVersions(left, right []int) int {
	for index := 0; index < len(left) || index < len(right); index++ {
		var l, r int
		if index < len(left) {
			l = left[index]
		}
		if index < len(right) {
			r = right[index]
		}
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
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

var versionTokenRegexp = regexp.MustCompile(`\d+(?:\.\d+)*`)

// criuVersionNumber extracts the dotted version from a CRIU version string.
// The inventory stores a clean dotted version, but capabilities from older
// agents carry the raw `criu --version` banner ("Version: 4.2 (git...)"), and
// both must floor the same way.
func criuVersionNumber(value string) string {
	return versionTokenRegexp.FindString(strings.TrimPrefix(strings.TrimSpace(value), "v"))
}

// criuVersionSupported reports whether a CRIU version satisfies the documented
// support floor: the 3.17+ line or the 4.x line (docs/support-matrix.json,
// README). Anything else — 3.16 and older, a 5.x line, an absent or
// unparseable version — fails closed: SHIFT never restores on a CRIU line the
// release does not stand behind.
func criuVersionSupported(version string) bool {
	parts := versionParts(criuVersionNumber(version))
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case 3:
		return compareVersions(parts, []int{3, 17}) >= 0
	case 4:
		return true
	default:
		return false
	}
}

// distributionKey normalizes a distribution identity to the "id-version"
// vocabulary the support matrix and distro rules use ("ubuntu-24.04").
// PRETTY_NAME probes ("Ubuntu 24.04.1 LTS") collapse to the same key as the
// os-release id-version form, and point releases of one line never look like
// different distributions.
func distributionKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "unknown"
	}
	value = strings.NewReplacer("_", "-", " ", "-", "/", "-").Replace(value)
	for strings.Contains(value, "--") {
		value = strings.ReplaceAll(value, "--", "-")
	}
	value = strings.Trim(value, "-")
	if match := versionTokenRegexp.FindString(value); match != "" {
		parts := strings.Split(match, ".")
		if len(parts) > 2 {
			match = strings.Join(parts[:2], ".")
		}
		index := strings.Index(value, match)
		id := strings.Trim(value[:index], "-")
		if id == "" {
			return match
		}
		if first, _, found := strings.Cut(id, "-"); found && first != "" {
			id = first
		}
		return id + "-" + match
	}
	return value
}
