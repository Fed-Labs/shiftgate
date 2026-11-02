package supportmatrix

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndProbeUnknownWithoutCRIU(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix.json")
	data := []byte(`{"version":"test","policy":{"unknown":"block_for_automatic_operations"},"platforms":[],"gpus":[]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	matrix, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	evidence := Probe(context.Background(), matrix)
	if evidence.MatrixVersion != "test" || evidence.Architecture == "" || evidence.ObservedAt.IsZero() {
		t.Fatalf("evidence = %#v", evidence)
	}
}

// decisionFixture is the shipped matrix shape minus real hardware claims:
// one certified combo, one untested, and the unknown fallback.
func decisionFixture() Matrix {
	return Matrix{
		Version: "test",
		Policy: Policy{
			Unknown:     ActionBlockAutomatic,
			Untested:    ActionExperimentalOnly,
			GPUEvidence: ActionExternalGPURequired,
		},
		Platforms: []Platform{
			{ID: "cert", Distribution: "ubuntu-24.04", Architecture: "amd64", KernelMin: "6.8", KernelMax: "6.14", CRIUVersions: []string{"4.2"}, Status: "certified"},
			{ID: "lts", Distribution: "ubuntu-22.04", Architecture: "amd64", KernelMin: "5.15", KernelMax: "5.15", CRIUVersions: []string{"3.17", "4.0"}, Status: "untested"},
			{ID: "gen", Distribution: "linux", Architecture: "amd64", KernelMin: "5.10", Status: "unknown"},
		},
	}
}

// TestDecideEnforcesBlockForAutomaticOperations is B-6's enforcement
// contract: certified combos run automatic operations; untested ones are
// experimental-only; everything else is blocked even when `criu check`
// passes — a probe is evidence, not certification.
func TestDecideEnforcesBlockForAutomaticOperations(t *testing.T) {
	matrix := decisionFixture()
	pass := func() Evidence {
		return Evidence{OS: "linux", Architecture: "amd64", Distribution: "ubuntu-24.04", Kernel: "6.8.0-45-generic", CRIU: "4.2.1", CRIUCheck: "passed"}
	}

	decision := matrix.Decide(pass())
	if decision.Status != "certified" || !decision.AllowAutomatic || decision.PlatformID != "cert" {
		t.Fatalf("certified combo decided %+v", decision)
	}

	untested := pass()
	untested.Distribution = "ubuntu-22.04"
	untested.Kernel = "5.15.0-100-generic"
	untested.CRIU = "3.17.1"
	decision = matrix.Decide(untested)
	if decision.Status != "untested" || decision.AllowAutomatic || decision.Action != ActionExperimentalOnly {
		t.Fatalf("untested combo decided %+v", decision)
	}

	// Certified kernel but uncertified CRIU line degrades to untested.
	badCRIU := pass()
	badCRIU.CRIU = "3.19.0"
	decision = matrix.Decide(badCRIU)
	if decision.Status != "untested" || decision.AllowAutomatic {
		t.Fatalf("uncertified CRIU line decided %+v", decision)
	}

	// Unknown distribution falls through to the generic entry: blocked.
	unknown := pass()
	unknown.Distribution = "nixos-24.11"
	unknown.Kernel = "6.10.0"
	decision = matrix.Decide(unknown)
	if decision.Status != "unknown" || decision.AllowAutomatic || decision.Action != ActionBlockAutomatic {
		t.Fatalf("unknown combo decided %+v", decision)
	}

	// No generic entry at all is still blocked.
	empty := decisionFixture()
	empty.Platforms = empty.Platforms[:2]
	decision = empty.Decide(unknown)
	if decision.AllowAutomatic {
		t.Fatalf("unmatched combo allowed: %+v", decision)
	}

	// CRIU check failure and non-linux platforms are unsupported regardless
	// of matrix entries.
	broken := pass()
	broken.CRIUCheck = "failed: criu check says no"
	decision = matrix.Decide(broken)
	if decision.Status != "unsupported" || decision.AllowAutomatic {
		t.Fatalf("broken CRIU decided %+v", decision)
	}
	other := pass()
	other.OS = "darwin"
	decision = matrix.Decide(other)
	if decision.Status != "unsupported" || decision.AllowAutomatic {
		t.Fatalf("non-linux decided %+v", decision)
	}
}

// TestDecideMatchesKernelRangesNumerically: 6.10 must outrank 6.8 in range
// checks and kernel suffixes must not break matching.
func TestDecideMatchesKernelRangesNumerically(t *testing.T) {
	matrix := decisionFixture()
	evidence := Evidence{OS: "linux", Architecture: "amd64", Distribution: "ubuntu-24.04", Kernel: "6.10.0-rc1", CRIU: "4.2", CRIUCheck: "passed"}
	decision := matrix.Decide(evidence)
	if decision.Status != "certified" {
		t.Fatalf("6.10 kernel inside 6.8-6.14 range decided %+v", decision)
	}
	evidence.Kernel = "6.15.0"
	decision = matrix.Decide(evidence)
	if decision.PlatformID == "cert" {
		t.Fatalf("kernel above the certified range still matched cert: %+v", decision)
	}
}

// TestLoadRejectsInvalidPolicyVocabulary: a typo in the policy block is a
// configuration error, not a silent fallback to a stricter or looser action.
func TestLoadRejectsInvalidPolicyVocabulary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix.json")
	data := []byte(`{"version":"test","policy":{"unknown":"block"},"platforms":[],"gpus":[]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("an unknown policy action was accepted")
	}
}
