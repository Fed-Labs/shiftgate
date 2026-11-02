package controlplane

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMachineKeyBinding(t *testing.T) {
	for _, test := range []struct {
		name     string
		keyName  string
		scopes   []string
		machine  string
		wantBind bool
	}{
		{name: "name binds", keyName: "machine:db-1", scopes: []string{"machines"}, machine: "db-1", wantBind: true},
		{name: "scope binds", keyName: "fleet key", scopes: []string{"machines", "machine:db-1"}, machine: "db-1", wantBind: true},
		{name: "case insensitive", keyName: "Machine:DB-1", scopes: nil, machine: "db-1", wantBind: true},
		{name: "other machine", keyName: "machine:db-2", scopes: []string{"machines", "machine:db-2"}, machine: "db-1", wantBind: false},
		{name: "unbound org key", keyName: "fleet automation key", scopes: []string{"machines"}, machine: "db-1", wantBind: false},
		{name: "admin key", keyName: "admin", scopes: []string{"admin"}, machine: "db-1", wantBind: false},
		{name: "empty machine", keyName: "machine:db-1", scopes: nil, machine: "", wantBind: false},
		{name: "prefix is not a binding", keyName: "machine:db-10", scopes: nil, machine: "db-1", wantBind: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := machineKeyBoundTo(test.keyName, test.scopes, test.machine); got != test.wantBind {
				t.Fatalf("machineKeyBoundTo(%q, %v, %q) = %v, want %v", test.keyName, test.scopes, test.machine, got, test.wantBind)
			}
		})
	}
}

func TestClaimLeaseRecordRoundTrip(t *testing.T) {
	payload := json.RawMessage(`{"migration_id":"migration-1"}`)
	record := claimLeaseRecord{
		TargetID: "command-1", TargetAction: "relay.start", TargetPayload: payload,
		ClaimToken: "token-1", ClaimedAt: time.Unix(1700000000, 0).UTC(), Attempt: 2,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded claimLeaseRecord
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TargetID != record.TargetID || decoded.TargetAction != record.TargetAction ||
		decoded.ClaimToken != record.ClaimToken || decoded.Attempt != record.Attempt ||
		string(decoded.TargetPayload) != string(payload) {
		t.Fatalf("claim lease round trip changed the record: %+v", decoded)
	}
}
