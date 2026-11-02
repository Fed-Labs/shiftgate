package model

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestWorkloadNormalizeRejectsRoot(t *testing.T) {
	spec := WorkloadSpec{Name: "bad", Command: []string{"true"}, RootPath: string(filepath.Separator)}
	if err := spec.Normalize(); err == nil {
		t.Fatal("expected filesystem root capture to be rejected")
	}
}

func TestCheckpointPolicyValidation(t *testing.T) {
	var absent *CheckpointPolicySpec
	if err := absent.Validate(); err != nil {
		t.Fatalf("a nil policy means no scheduling and must be valid: %v", err)
	}
	for name, policy := range map[string]CheckpointPolicySpec{
		"interval below the floor": {IntervalSeconds: 9},
		"negative interval":        {IntervalSeconds: -60},
		"negative keep":            {IntervalSeconds: 600, KeepLast: -1},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	policy := CheckpointPolicySpec{IntervalSeconds: 600, KeepLast: 5}
	if err := policy.Validate(); err != nil {
		t.Fatalf("a sane policy must be valid: %v", err)
	}
	spec := WorkloadSpec{Name: "policed", Command: []string{"true"}, RootPath: t.TempDir(), CheckpointPolicy: &CheckpointPolicySpec{IntervalSeconds: 5}}
	if err := spec.Normalize(); err == nil {
		t.Fatal("Normalize must reject a policy the scheduler could not honor")
	}
}

// TestFailoverPolicyValidation pins the standby designation's own rules and
// its pairing with a checkpoint schedule: without one, nothing would ever be
// replicated, so a failover policy alone must never normalize.
func TestFailoverPolicyValidation(t *testing.T) {
	var absent *FailoverPolicySpec
	if err := absent.Validate(); err != nil {
		t.Fatalf("a nil policy means no standby and must be valid: %v", err)
	}
	for name, policy := range map[string]FailoverPolicySpec{
		"missing url":        {},
		"plain http":         {AgentURL: "http://standby.example:9443"},
		"query string":       {AgentURL: "https://standby.example:9443/?peer=1"},
		"fragment":           {AgentURL: "https://standby.example:9443/#main"},
		"hostless":           {AgentURL: "https://"},
		"negative keep":      {AgentURL: "https://standby.example:9443", KeepLast: -1},
		"not a URL at all":   {AgentURL: "standby.example"},
		"unix socket scheme": {AgentURL: "unix:///run/shift/agent.sock"},
	} {
		if err := policy.Validate(); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	policy := FailoverPolicySpec{AgentURL: "https://standby.example:9443", MachineID: "machine-standby", KeepLast: 3}
	if err := policy.Validate(); err != nil {
		t.Fatalf("a pinned https standby must be valid: %v", err)
	}
	// The pair: a standby without a schedule replicates nothing, forever.
	orphan := WorkloadSpec{Name: "stranded", Command: []string{"true"}, RootPath: t.TempDir(),
		FailoverPolicy: &FailoverPolicySpec{AgentURL: "https://standby.example:9443"}}
	if err := orphan.Normalize(); err == nil {
		t.Fatal("a failover policy without a checkpoint policy must be refused")
	}
	paired := orphan
	paired.CheckpointPolicy = &CheckpointPolicySpec{IntervalSeconds: 600}
	if err := paired.Normalize(); err != nil {
		t.Fatalf("a policy pair must normalize: %v", err)
	}
}

func TestMigrationTransitionSafety(t *testing.T) {
	migration := Migration{Stage: MigrationCreated}
	if err := migration.Transition(MigrationCommit, "skip validation", 1); err == nil {
		t.Fatal("unsafe transition was accepted")
	}
	if err := migration.Transition(MigrationDiscover, "inventory", 0.05); err != nil {
		t.Fatalf("valid transition rejected: %v", err)
	}
}

// TestMigrationFailureRollsBackDirectly pins the stage machine's
// rollback-reachability invariant: a failure at any pre-commit stage must be
// able to enter ROLLING_BACK without passing through FAILED. FAILED is a
// terminal stage for every observer — the CLI, the dashboard, the policy
// scheduler — so routing a rolling-back migration through it makes a
// preserved source look like an abandoned one.
func TestMigrationFailureRollsBackDirectly(t *testing.T) {
	preCommit := []MigrationStage{
		MigrationDiscover, MigrationValidate, MigrationSnapshot, MigrationPrepare,
		MigrationTransfer, MigrationVerify, MigrationRestore, MigrationPostValidate,
		MigrationSwitch, MigrationCommit,
	}
	for _, stage := range preCommit {
		if !CanTransition(stage, MigrationRollingBack) {
			t.Fatalf("stage %s cannot roll back directly — a failure there would be observable as terminal FAILED", stage)
		}
	}
	if CanTransition(MigrationCompleted, MigrationRollingBack) {
		t.Fatal("a completed migration must never roll back")
	}
}

func TestManifestPublicStripsSensitiveFields(t *testing.T) {
	manifest := CheckpointManifest{
		Format: StateFormatName, FormatVersion: StateFormatVersion, ID: "cp-1",
		Workload: WorkloadSpec{
			ID: "w-1", Name: "app", RootPath: "/srv/app",
			Command:     []string{"/srv/app/run.sh", "--token", "super-secret"},
			Environment: map[string]string{"API_TOKEN": "super-secret", "LANG": "C"},
			Paths:       []PathSpec{{Path: "/srv/app", Mode: PathReadWrite}},
			HealthCheck: &HealthCheckSpec{Type: "command", Command: []string{"/bin/curl", "http://x/health"}},
		},
		Assets: []AssetManifest{{
			Name: "filesystem-root",
			Chunks: []ChunkRef{
				{Address: "aa", PlainSHA256: "deadbeef", CipherSHA256: "cafebabe"},
				{Address: "bb", PlainSHA256: "feedface"},
			},
		}},
	}
	public := manifest.Public()

	if public.Workload.Environment != nil {
		t.Fatal("environment values must not leave the data plane")
	}
	if len(public.Workload.Paths) != 0 || public.Workload.HealthCheck != nil {
		t.Fatal("paths and health-check details must not leave the data plane")
	}
	if public.Workload.Command[0] != "/srv/app/run.sh" || public.Workload.Command[1] != "<redacted>" {
		t.Fatalf("command arguments must be redacted, got %v", public.Workload.Command)
	}
	for _, chunk := range public.Assets[0].Chunks {
		if chunk.PlainSHA256 != "" {
			t.Fatal("unkeyed plaintext digests must not leave the data plane")
		}
	}
	if manifest.Workload.Environment["API_TOKEN"] != "super-secret" {
		t.Fatal("Public must not mutate the source manifest")
	}
	if manifest.Assets[0].Chunks[0].PlainSHA256 != "deadbeef" {
		t.Fatal("Public must not mutate the source chunk refs")
	}
}

// TestHeartbeatResponseParsesTheEntitlement pins the wire contract both sides
// share: the control plane serializes live_migration_allowed on every
// heartbeat and clients parse it from this one type, so the licensing field
// can never again be written and dropped.
func TestHeartbeatResponseParsesTheEntitlement(t *testing.T) {
	var response HeartbeatResponse
	if err := json.Unmarshal([]byte(`{"live_migration_allowed": true}`), &response); err != nil {
		t.Fatal(err)
	}
	if !response.LiveMigrationAllowed {
		t.Fatal("live_migration_allowed did not parse")
	}
	var missing HeartbeatResponse
	if err := json.Unmarshal([]byte(`{"machine_id": "m-1"}`), &missing); err != nil {
		t.Fatal(err)
	}
	if missing.LiveMigrationAllowed {
		t.Fatal("an absent field must parse as not-allowed, never open the gate")
	}
}

// TestAgentCommandCarriesItsPayloadVersion pins the version-skew contract: the
// envelope names the payload schema so a mixed-version window refuses (and
// redelivers) instead of mis-executing.
func TestAgentCommandCarriesItsPayloadVersion(t *testing.T) {
	command := AgentCommand{ID: "c-1", Action: "relay.start", Payload: json.RawMessage(`{}`), PayloadVersion: AgentCommandPayloadVersion, ClaimToken: "t"}
	encoded, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AgentCommand
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PayloadVersion != AgentCommandPayloadVersion {
		t.Fatalf("payload_version did not round-trip: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte(`"payload_version":1`)) {
		t.Fatalf("payload_version is not on the wire: %s", encoded)
	}
}
