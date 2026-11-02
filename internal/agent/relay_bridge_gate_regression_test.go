package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shift.dev/shift/internal/controlplane"
	"shift.dev/shift/internal/model"
)

// Regression cover for the relay.start admission gate: the agent command
// channel is an admission point the product controls, and live mode is a paid
// entitlement. A relayed live migration without one must be refused with the
// shared billing error contract here, exactly like the local API's create
// gate — never silently run the paid feature for a free-tier org. Cold mode
// and preflight are never gated.
func TestRelayStartLiveModeRequiresEntitlement(t *testing.T) {
	service, workload := localAPIFixture(t)
	service.config.Relay.Enabled = true

	payloadFor := func(mode model.MigrationMode) []byte {
		payload, err := json.Marshal(relayStart{
			WorkloadID: workload.Spec.ID,
			Destination: model.Destination{
				MachineID: "detached-destination", AgentURL: "https://127.0.0.1:18443",
				Route: "relay", RelayTicket: "one-use-ticket",
			},
			Mode: mode, TimeoutSeconds: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	send := func(payload []byte) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return service.handleRelayCommand(ctx, controlplane.AgentCommand{Action: "relay.start", Payload: payload})
	}

	err := send(payloadFor(model.MigrationLive))
	if err == nil || !strings.Contains(err.Error(), "LIVE_MIGRATION_PLAN_REQUIRED") {
		t.Fatalf("unentitled relay.start live mode must carry the shared paywall contract, got %v", err)
	}

	// Cold mode is never gated: any outcome other than the paywall proves
	// the gate is mode-specific (here the relay binding check refuses the
	// fake ticket, which is fine).
	err = send(payloadFor(model.MigrationCold))
	if err != nil && strings.Contains(err.Error(), "LIVE_MIGRATION_PLAN_REQUIRED") {
		t.Fatalf("cold relay.start was paywalled: %v", err)
	}

	// The learned entitlement (the heartbeat's live_migration_allowed) is
	// the licensing truth: once it allows live moves, the gate steps aside.
	service.noteEntitlement(true)
	err = send(payloadFor(model.MigrationLive))
	if err != nil && strings.Contains(err.Error(), "LIVE_MIGRATION_PLAN_REQUIRED") {
		t.Fatalf("an entitled relay.start live mode was paywalled: %v", err)
	}
}
