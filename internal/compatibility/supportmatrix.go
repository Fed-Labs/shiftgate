package compatibility

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// supportmatrix.go: the release support contract, enforced at the
// compatibility gate.
//
// docs/support-matrix.json is the versioned answer to "may this machine run
// unattended checkpoint/restore operations?", and its policy block is the
// product's promise: unknown combinations are not silently treated as
// compatible, uncertified rows are blocked for automatic operations, and GPU
// rows require an external hardware runner. The gate used to ignore all of it;
// now every restore-compatibility decision consults the matrix, the scheduler
// refuses automatic placements it forbids, and a regression test pins this
// embedded copy to docs/support-matrix.json so the two can never drift.

// SupportMatrix is one machine certification matrix: the policy block plus the
// platform and GPU rows it grades against.
type SupportMatrix struct {
	Version   string            `json:"version"`
	Policy    SupportPolicy     `json:"policy"`
	Platforms []SupportPlatform `json:"platforms"`
	GPUs      []SupportGPU      `json:"gpus"`
}

// SupportPolicy maps a row status to the action that governs it.
type SupportPolicy struct {
	Unknown     string `json:"unknown"`
	Untested    string `json:"untested"`
	GPUEvidence string `json:"gpu_evidence"`
	Certified   string `json:"certified,omitempty"`
	Unsupported string `json:"unsupported,omitempty"`
}

// SupportPlatform is one certified (or deliberately uncertified) platform row.
type SupportPlatform struct {
	ID           string   `json:"id"`
	Distribution string   `json:"distribution"`
	Architecture string   `json:"architecture"`
	KernelMin    string   `json:"kernel_min"`
	KernelMax    string   `json:"kernel_max,omitempty"`
	CRIUVersions []string `json:"criu_versions,omitempty"`
	Status       string   `json:"status"`
	Evidence     string   `json:"evidence"`
}

// SupportGPU is one GPU vendor row. The shipped matrix certifies no local GPU
// hardware: nvidia is "no local hardware claim", amd is unsupported.
type SupportGPU struct {
	Vendor   string `json:"vendor"`
	Driver   string `json:"driver,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

// SupportEvidence is one observed machine, as cmd/shift-verify probes it and
// as the compatibility gate derives it from a destination's capabilities.
type SupportEvidence struct {
	MatrixVersion  string    `json:"matrix_version"`
	ObservedAt     time.Time `json:"observed_at"`
	OS             string    `json:"os"`
	Architecture   string    `json:"architecture"`
	Distribution   string    `json:"distribution,omitempty"`
	Kernel         string    `json:"kernel"`
	CRIU           string    `json:"criu,omitempty"`
	CRIUCheck      string    `json:"criu_check"`
	PlatformStatus string    `json:"platform_status"`
	Notes          []string  `json:"notes,omitempty"`
}

// SupportDecision is the enforcement answer for one observed machine: which
// matrix status applies, which policy action governs it, and whether automatic
// operations (unattended migration/failover orchestration) may run.
type SupportDecision struct {
	Status         string `json:"status"`
	Action         string `json:"action"`
	AllowAutomatic bool   `json:"allow_automatic"`
	PlatformID     string `json:"platform_id,omitempty"`
	Reason         string `json:"reason"`
}

// Policy action vocabulary.
const (
	ActionAllow               = "allow"
	ActionBlockAutomatic      = "block_for_automatic_operations"
	ActionExperimentalOnly    = "experimental_only"
	ActionExternalGPURequired = "external_hardware_runner_required"
)

// Platform status vocabulary.
const (
	StatusCertified    = "certified"
	StatusSupported    = "supported"
	StatusUntested     = "untested"
	StatusExperimental = "experimental"
	StatusUnknown      = "unknown"
	StatusUnsupported  = "unsupported"
)

var validActions = map[string]bool{
	ActionAllow:               true,
	ActionBlockAutomatic:      true,
	ActionExperimentalOnly:    true,
	ActionExternalGPURequired: true,
}

var validStatuses = map[string]bool{
	StatusCertified:    true,
	StatusSupported:    true,
	StatusUntested:     true,
	StatusExperimental: true,
	StatusUnknown:      true,
	StatusUnsupported:  true,
}

//go:embed support-matrix.json
var defaultSupportMatrixJSON []byte

var (
	defaultSupportMatrixOnce sync.Once
	defaultSupportMatrixVal  SupportMatrix
)

// DefaultSupportMatrix is the shipped support contract (docs/support-matrix.json,
// pinned to it by TestDefaultSupportMatrixMatchesDocs). A Checker without an
// explicit matrix enforces this one, so the runtime always applies the exact
// decisions the release documents.
func DefaultSupportMatrix() SupportMatrix {
	defaultSupportMatrixOnce.Do(func() {
		if err := json.Unmarshal(defaultSupportMatrixJSON, &defaultSupportMatrixVal); err != nil {
			panic("compatibility: embedded support matrix is unreadable: " + err.Error())
		}
		if err := defaultSupportMatrixVal.Validate(); err != nil {
			panic("compatibility: embedded support matrix is invalid: " + err.Error())
		}
	})
	return defaultSupportMatrixVal
}

// LoadSupportMatrix reads a support matrix file. The file must be a complete,
// well-formed matrix: an invalid policy vocabulary is a configuration error,
// never a silent fallback.
func LoadSupportMatrix(path string) (SupportMatrix, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SupportMatrix{}, err
	}
	var matrix SupportMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		return SupportMatrix{}, fmt.Errorf("decode support matrix: %w", err)
	}
	if err := matrix.Validate(); err != nil {
		return SupportMatrix{}, err
	}
	return matrix, nil
}

// Validate rejects matrices that cannot make a decision: missing version,
// policy actions outside the vocabulary, and rows whose status no policy
// action could ever resolve.
func (m SupportMatrix) Validate() error {
	if m.Version == "" {
		return errors.New("support matrix version is required")
	}
	checkAction := func(field, action string) error {
		if action == "" {
			return nil
		}
		if !validActions[action] {
			return fmt.Errorf("support matrix policy %s is %q; must be one of allow, block_for_automatic_operations, experimental_only, external_hardware_runner_required", field, action)
		}
		return nil
	}
	for field, action := range map[string]string{
		"unknown": m.Policy.Unknown, "untested": m.Policy.Untested,
		"gpu_evidence": m.Policy.GPUEvidence, "certified": m.Policy.Certified,
		"unsupported": m.Policy.Unsupported,
	} {
		if err := checkAction(field, action); err != nil {
			return err
		}
	}
	for _, platform := range m.Platforms {
		if !validStatuses[platform.Status] {
			return fmt.Errorf("support matrix platform %s has status %q; must be one of certified, supported, untested, experimental, unknown, unsupported", platform.ID, platform.Status)
		}
		if platform.Architecture == "" {
			return fmt.Errorf("support matrix platform %s requires an architecture", platform.ID)
		}
	}
	for _, gpu := range m.GPUs {
		if !validStatuses[gpu.Status] {
			return fmt.Errorf("support matrix GPU row %s has status %q; must be one of certified, supported, untested, experimental, unknown, unsupported", gpu.Vendor, gpu.Status)
		}
	}
	return nil
}

// Decide matches the observed machine against the matrix and applies the
// matrix's own policy block. The rules are fail-closed: an unmatched
// combination is "unknown" and blocked for automatic operations even when
// `criu check` passes — a passing probe is evidence, not certification.
func (m SupportMatrix) Decide(evidence SupportEvidence) SupportDecision {
	if evidence.OS != "linux" || evidence.Architecture != "amd64" {
		return SupportDecision{Status: StatusUnsupported, Action: ActionBlockAutomatic, Reason: "SHIFT supports Linux x86_64 only"}
	}
	if strings.HasPrefix(evidence.CRIUCheck, "failed") || strings.HasPrefix(evidence.CRIUCheck, "not-run") {
		return SupportDecision{Status: StatusUnsupported, Action: ActionBlockAutomatic, Reason: "CRIU is unavailable or failed `criu check`: " + evidence.CRIUCheck}
	}
	match, ok := m.matchPlatform(evidence)
	if !ok {
		action := m.policyAction(StatusUnknown, ActionBlockAutomatic)
		return SupportDecision{Status: StatusUnknown, Action: action, AllowAutomatic: action == ActionAllow, Reason: "no support-matrix entry covers this distribution/kernel/CRIU combination; run scripts/verify-support-matrix.sh to certify it"}
	}
	status := match.Status
	reason := "matched support-matrix entry " + match.ID + ": " + match.Evidence
	if len(match.CRIUVersions) > 0 && !matchesCRIULine(match.CRIUVersions, evidence.CRIU) {
		// The machine matches the entry's distribution and kernel line but
		// runs a CRIU the entry does not certify: degrade the entry's status
		// to untested instead of falling through to the unknown fallback —
		// the kernel combination has evidence, the CRIU line does not.
		if status == StatusCertified {
			status = StatusUntested
		}
		reason = "matches support-matrix entry " + match.ID + " but CRIU " + evidence.CRIU + " is not one of its certified lines (" + strings.Join(match.CRIUVersions, ", ") + "); run scripts/verify-support-matrix.sh"
	}
	action := m.policyAction(status, defaultActionFor(status))
	return SupportDecision{Status: status, Action: action, AllowAutomatic: action == ActionAllow, PlatformID: match.ID, Reason: reason}
}

// GPUDecision applies the gpus block and the gpu_evidence policy to one device
// requirement. The shipped matrix certifies no local GPU hardware, so a
// workload that needs a GPU is governed by Policy.GPUEvidence
// (external_hardware_runner_required): GPU rows require an external hardware
// runner. A matrix that certifies a vendor's GPU row allows local restore and
// the checker's device-tier logic takes over.
func (m SupportMatrix) GPUDecision(vendor string) SupportDecision {
	vendor = strings.TrimSpace(vendor)
	for _, gpu := range m.GPUs {
		if strings.EqualFold(gpu.Vendor, vendor) {
			if gpu.Status == StatusCertified || gpu.Status == StatusSupported {
				action := m.policyAction(gpu.Status, ActionAllow)
				return SupportDecision{Status: gpu.Status, Action: action, AllowAutomatic: action == ActionAllow, Reason: "matched support-matrix GPU row " + gpu.Vendor + ": " + gpu.Evidence}
			}
			action := m.Policy.GPUEvidence
			if !validActions[action] {
				action = ActionExternalGPURequired
			}
			return SupportDecision{Status: gpu.Status, Action: action, AllowAutomatic: action == ActionAllow, Reason: "support-matrix GPU row " + gpu.Vendor + " is " + gpu.Status + ": " + gpu.Evidence}
		}
	}
	action := m.Policy.GPUEvidence
	if !validActions[action] {
		action = ActionExternalGPURequired
	}
	return SupportDecision{Status: StatusUnknown, Action: action, AllowAutomatic: action == ActionAllow, Reason: "no support-matrix GPU row certifies vendor " + vendor + "; GPU rows require an external hardware runner"}
}

// policyAction resolves a status through the matrix's policy block, falling
// back to the given default when the policy does not name the status.
func (m SupportMatrix) policyAction(status, fallback string) string {
	switch status {
	case StatusUnknown:
		if m.Policy.Unknown != "" {
			return m.Policy.Unknown
		}
	case StatusUntested, StatusExperimental:
		if m.Policy.Untested != "" {
			return m.Policy.Untested
		}
	case StatusUnsupported:
		if m.Policy.Unsupported != "" {
			return m.Policy.Unsupported
		}
		return ActionBlockAutomatic
	case StatusCertified, StatusSupported:
		if m.Policy.Certified != "" {
			return m.Policy.Certified
		}
		return ActionAllow
	}
	return fallback
}

func defaultActionFor(status string) string {
	switch status {
	case StatusCertified, StatusSupported:
		return ActionAllow
	case StatusUntested, StatusExperimental:
		return ActionExperimentalOnly
	default:
		return ActionBlockAutomatic
	}
}

// matchPlatform finds the most specific entry covering the evidence's
// distribution and kernel range: an exact distribution match outranks the
// generic linux fallback. CRIU lines are graded in Decide — a machine can
// match an entry's kernel while running a CRIU the entry never certified.
// Distributions compare in the normalized id-version vocabulary, so a
// PRETTY_NAME probe ("Ubuntu 24.04.1 LTS") still matches an "ubuntu-24.04" row.
func (m SupportMatrix) matchPlatform(evidence SupportEvidence) (SupportPlatform, bool) {
	var generic SupportPlatform
	genericFound := false
	for _, platform := range m.Platforms {
		if platform.Architecture != evidence.Architecture {
			continue
		}
		if !kernelInRange(evidence.Kernel, platform.KernelMin, platform.KernelMax) {
			continue
		}
		if distributionKey(platform.Distribution) == distributionKey(evidence.Distribution) {
			return platform, true
		}
		if strings.EqualFold(platform.Distribution, "linux") && !genericFound {
			generic = platform
			genericFound = true
		}
	}
	return generic, genericFound
}

// kernelInRange reports whether an observed kernel release satisfies
// kernel_min (and kernel_max when the entry bounds one). Bounds name kernel
// lines: "6.8" covers every 6.8.x release, and the maximum covers its whole
// line too ("6.14" admits 6.14.1 but not 6.15.0). Releases compare
// numerically segment by segment, so 6.10 outranks 6.8.
func kernelInRange(observed, minimum, maximum string) bool {
	if minimum == "" {
		return true
	}
	observedParts := versionParts(observed)
	minParts := versionParts(minimum)
	if len(observedParts) == 0 || len(minParts) == 0 || len(observedParts) < len(minParts) {
		return false
	}
	if compareVersions(observedParts[:len(minParts)], minParts) < 0 {
		return false
	}
	if maximum != "" {
		maxParts := versionParts(maximum)
		if len(maxParts) == 0 || len(observedParts) < len(maxParts) {
			return false
		}
		if compareVersions(observedParts[:len(maxParts)], maxParts) > 0 {
			return false
		}
	}
	return true
}

// matchesCRIULine reports whether an observed CRIU version belongs to one of
// the entry's certified lines. A line is a prefix ("4.2" covers "4.2.1"), so
// patch releases inherit their line's certification.
func matchesCRIULine(lines []string, observed string) bool {
	observed = strings.TrimPrefix(strings.TrimSpace(observed), "v")
	for _, line := range lines {
		line = strings.TrimPrefix(strings.TrimSpace(line), "v")
		if observed == line || strings.HasPrefix(observed, line+".") {
			return true
		}
	}
	return false
}
