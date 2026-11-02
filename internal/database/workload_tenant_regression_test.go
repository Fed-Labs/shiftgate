package database

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Regression cover for the cross-tenant workload takeover (ship finding 03
// blocker 1): workload ids are agent-supplied, and an upsert carrying an id
// that already belongs to another organization must never re-parent or
// overwrite the row. PostgreSQL-gated.
func TestUpsertWorkloadRefusesForeignOrganization(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	orgA := testOrganization(t, store, "tenant-a")
	orgB := testOrganization(t, store, "tenant-b")

	machineA, err := store.CreateMachine(ctx, MachineRecord{
		ID: mustRegressionID(t), OrganizationID: orgA, MachineID: "machine-a", Name: "machine-a",
		AgentURL: "https://127.0.0.1:18443", Capabilities: json.RawMessage(`{}`),
	}, testAudit(orgA, "machine.create", "machine-a"))
	if err != nil {
		t.Fatal(err)
	}
	machineB, err := store.CreateMachine(ctx, MachineRecord{
		ID: mustRegressionID(t), OrganizationID: orgB, MachineID: "machine-b", Name: "machine-b",
		AgentURL: "https://127.0.0.1:18444", Capabilities: json.RawMessage(`{}`),
	}, testAudit(orgB, "machine.create", "machine-b"))
	if err != nil {
		t.Fatal(err)
	}

	workloadID := mustRegressionID(t)
	spec := json.RawMessage(`{"name":"victim","command":["/bin/true"],"root_path":"/srv/victim"}`)
	created, err := store.UpsertWorkload(ctx, WorkloadRecord{
		ID: workloadID, OrganizationID: orgA, MachineID: machineA.MachineID,
		Name: "victim", Spec: spec,
	}, testAudit(orgA, "workload.upsert", workloadID))
	if err != nil {
		t.Fatalf("owner organization upsert refused: %v", err)
	}

	// The attacker upserts the same workload id from another organization.
	if _, err := store.UpsertWorkload(ctx, WorkloadRecord{
		ID: workloadID, OrganizationID: orgB, MachineID: machineB.MachineID,
		Name: "stolen", Spec: json.RawMessage(`{"name":"stolen"}`),
	}, testAudit(orgB, "workload.upsert", workloadID)); err == nil {
		t.Fatal("cross-tenant upsert was accepted: workload ids can be hijacked between organizations")
	} else if !errors.Is(err, pgx.ErrNoRows) && !IsNotFound(err) {
		t.Fatalf("cross-tenant upsert error = %v, want a not-found refusal", err)
	}

	// The original row must be untouched: same organization, name, and spec.
	var organizationID, name string
	var storedSpec json.RawMessage
	if err := store.pool.QueryRow(ctx, `SELECT organization_id,name,spec FROM workloads WHERE id=$1`, workloadID).
		Scan(&organizationID, &name, &storedSpec); err != nil {
		t.Fatal(err)
	}
	if organizationID != orgA || name != created.Name || name != "victim" {
		t.Fatalf("workload row was modified by a foreign upsert: org=%s name=%s", organizationID, name)
	}
	// JSONB re-serializes on storage, so compare the documents, not the text.
	var want, got any
	if err := json.Unmarshal(spec, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(storedSpec, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("workload spec was overwritten by a foreign upsert: %s", storedSpec)
	}

	// The owner can still update its own row.
	if _, err := store.UpsertWorkload(ctx, WorkloadRecord{
		ID: workloadID, OrganizationID: orgA, MachineID: machineA.MachineID,
		Name: "victim-v2", Spec: spec,
	}, testAudit(orgA, "workload.upsert", workloadID)); err != nil {
		t.Fatalf("owner organization cannot update its own workload: %v", err)
	}
}
