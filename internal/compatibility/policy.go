package compatibility

import (
	"encoding/json"
	"errors"
	"os"
)

// policy.go: the configurable compatibility policy.
//
// CRIU restore is not a version-agnostic operation: it was written against a
// kernel's process-image format, and the safe envelope depends on the kernel
// and distribution pair. The checker's job is to encode that envelope as data
// instead of one hard-coded rule, so fleets widen it only with evidence: the
// default policy is deliberately conservative (same kernel major, equal-or-
// newer minor — the historically safe envelope), and certified cross-major
// and cross-distro pairs are opt-in entries an operator ships in a policy
// file after running their own restore tests.

// KernelRule admits one source→destination kernel relationship.
type KernelRule struct {
	// SourceKernel and DestinationKernel are dotted prefixes ("5.15",
	// "6.8"): a rule matches when both observed kernels fall under them.
	SourceKernel      string `json:"source_kernel"`
	DestinationKernel string `json:"destination_kernel"`
	// Certified marks the pair as tested end-to-end (checkpoint on the
	// source line, restore on the destination line). Uncertified pairs are
	// refused even when they would otherwise match: the product never claims
	// a restore it has not demonstrated.
	Certified bool   `json:"certified"`
	Evidence  string `json:"evidence,omitempty"`
}

// DistroRule governs cross-distribution restores. Cross-distro moves keep
// userland assumptions (glibc, paths, libraries) that the checker cannot see
// inside a checkpoint, so they are policy decisions, not facts.
type DistroRule struct {
	SourceDistribution      string `json:"source_distribution"`
	DestinationDistribution string `json:"destination_distribution"`
	// Allow admits the pair (with a warning); false means the checker
	// refuses it with DISTRO_INCOMPATIBLE.
	Allow    bool   `json:"allow"`
	Evidence string `json:"evidence,omitempty"`
}

// Policy is the compatibility envelope for state formats, kernels, and
// distributions. The zero value is not usable; call DefaultPolicy or
// LoadPolicy. A Checker without an explicit policy uses DefaultPolicy.
type Policy struct {
	// StateFormatFloor is the oldest state format this build restores
	// ("1.0.0"). Formats with the same major that are not newer than this
	// build's format are accepted; see stateFormatDecision.
	StateFormatFloor string `json:"state_format_floor"`
	// KernelRules are certified source→destination kernel pairs beyond the
	// default same-major envelope.
	KernelRules []KernelRule `json:"kernel_rules"`
	// CrossDistroDefault decides cross-distribution restores not named by
	// any DistroRule: "warn" (allowed, with a warning) or "refuse".
	CrossDistroDefault string `json:"cross_distro_default"`
	// DistroRules allowlist specific cross-distribution pairs.
	DistroRules []DistroRule `json:"distro_rules"`
}

// Cross-distribution defaults.
const (
	DistroWarn   = "warn"
	DistroRefuse = "refuse"
)

// DefaultPolicy is the envelope the project itself stands behind without
// operator evidence. It encodes CRIU's actual restore constraint — a dump is
// restorable on the same or a newer kernel, never an older one — plus the
// project's conservative refusal of uncertified cross-major moves:
//
//   - same kernel major, destination ≥ source: admitted;
//   - destination older than source (any major): KERNEL_INCOMPATIBLE;
//   - cross-major, destination newer: admitted only by a certified
//     KernelRule in the policy file (KERNEL_CROSS_MAJOR_NOT_CERTIFIED
//     otherwise). No pair is certified by default because the repository
//     runs no cross-major e2e evidence — operators widen the envelope by
//     testing a pair and recording it, with the evidence string required
//     reading for the next operator;
//   - cross-distribution: warned but allowed (userspace differences are
//     surfaced, not hidden), escalatable to a refusal per pair via
//     DistroRules or wholesale via CrossDistroDefault.
//
// State formats are accepted across the whole 1.x line (see
// stateFormatDecision): a 1.0.0 destination refuses 1.1.x manifests with
// STATE_FORMAT_UPGRADE_REQUIRED, and every other same-major combination
// restores. The floor is the oldest format this build still reads.
func DefaultPolicy() Policy {
	return Policy{
		StateFormatFloor:   "1.0.0",
		CrossDistroDefault: DistroWarn,
	}
}

// LoadPolicy reads a policy file. The file replaces the defaults entirely —
// a custom policy is an explicit statement of the envelope, so it cannot
// accidentally inherit pairs it did not name.
func LoadPolicy(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	var policy Policy
	if err := json.Unmarshal(data, &policy); err != nil {
		return Policy{}, err
	}
	if err := policy.Validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// Validate rejects policy files that cannot make a decision.
func (p Policy) Validate() error {
	if p.StateFormatFloor == "" {
		return errors.New("compatibility policy requires state_format_floor")
	}
	if p.CrossDistroDefault != DistroWarn && p.CrossDistroDefault != DistroRefuse {
		return errors.New("compatibility policy cross_distro_default must be \"warn\" or \"refuse\"")
	}
	for _, rule := range p.KernelRules {
		if rule.SourceKernel == "" || rule.DestinationKernel == "" {
			return errors.New("compatibility policy kernel rules require source_kernel and destination_kernel")
		}
	}
	for _, rule := range p.DistroRules {
		if rule.SourceDistribution == "" || rule.DestinationDistribution == "" {
			return errors.New("compatibility policy distro rules require both distributions")
		}
	}
	return nil
}
