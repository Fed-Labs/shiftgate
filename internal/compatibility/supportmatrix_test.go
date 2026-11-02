package compatibility

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// TestDefaultSupportMatrixMatchesDocs pins the embedded support contract to
// docs/support-matrix.json: the gate enforces the exact decisions the release
// documents, and any drift between the two is a test failure, not a silent
// divergence between the product and its promises.
func TestDefaultSupportMatrixMatchesDocs(t *testing.T) {
	data, err := os.ReadFile("../../docs/support-matrix.json")
	if err != nil {
		t.Fatal(err)
	}
	var documented SupportMatrix
	if err := json.Unmarshal(data, &documented); err != nil {
		t.Fatal(err)
	}
	if err := documented.Validate(); err != nil {
		t.Fatalf("docs/support-matrix.json is invalid: %v", err)
	}
	if !reflect.DeepEqual(documented, DefaultSupportMatrix()) {
		t.Fatal("embedded support matrix drifted from docs/support-matrix.json; copy the updated file into internal/compatibility/support-matrix.json")
	}
}

// TestGPUDecisionAppliesGPUEvidencePolicy pins the gpus block behavior the
// deployment guide promises: uncertified GPU rows require an external
// hardware runner, and only a certified row admits local restore.
func TestGPUDecisionAppliesGPUEvidencePolicy(t *testing.T) {
	matrix := DefaultSupportMatrix()
	decision := matrix.GPUDecision("nvidia")
	if decision.Action != ActionExternalGPURequired || decision.AllowAutomatic {
		t.Fatalf("shipped nvidia row decided %+v", decision)
	}
	decision = matrix.GPUDecision("AMD")
	if decision.Action != ActionExternalGPURequired || decision.AllowAutomatic {
		t.Fatalf("shipped amd row decided %+v", decision)
	}
	decision = matrix.GPUDecision("intel")
	if decision.Action != ActionExternalGPURequired {
		t.Fatalf("an uncertified vendor was admitted: %+v", decision)
	}
	certified := DefaultSupportMatrix()
	certified.GPUs = []SupportGPU{{Vendor: "nvidia", Status: StatusCertified, Evidence: "external runner evidence"}}
	decision = certified.GPUDecision("NVIDIA")
	if decision.Action != ActionAllow || !decision.AllowAutomatic {
		t.Fatalf("certified GPU row decided %+v", decision)
	}
}

// TestLoadSupportMatrixRejectsBadStatuses: rows whose status no policy could
// resolve are configuration errors.
func TestLoadSupportMatrixRejectsBadStatuses(t *testing.T) {
	path := t.TempDir() + "/matrix.json"
	data := `{"version":"t","platforms":[{"id":"x","distribution":"d","architecture":"amd64","kernel_min":"6.8","status":"probably","evidence":"e"}],"gpus":[]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSupportMatrix(path); err == nil {
		t.Fatal("matrix with an unknown row status was accepted")
	}
}
