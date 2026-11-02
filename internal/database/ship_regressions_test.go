package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestRunMigrationStatementsCommitsPlainDDLBeforeNoTransaction: a file that
// mixes ordinary DDL with a `-- shift:no-transaction` statement must keep the
// DDL. Rolling the open block back when the concurrent statement starts — the
// old behavior — silently lost it while the file was recorded applied.
// PostgreSQL-gated.
func TestRunMigrationStatementsCommitsPlainDDLBeforeNoTransaction(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	conn, err := store.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	table := "migration_runner_probe_" + strings.ReplaceAll(fmt.Sprint(time.Now().UnixNano()), "-", "")
	statements := splitSQL(fmt.Sprintf("CREATE TABLE %s (id INT);\n-- shift:no-transaction\nSELECT 1;", table))
	if len(statements) != 2 || !statements[1].noTransaction {
		t.Fatalf("probe did not split into plain + no-transaction statements: %#v", statements)
	}
	bookkeeping := fmt.Sprintf("INSERT INTO migration_runner_probe_log(name) VALUES('%s') ON CONFLICT DO NOTHING", table)
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS migration_runner_probe_log(name TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := runMigrationStatements(ctx, conn, statements, bookkeeping); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_name=$1)`, table).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("plain DDL before a no-transaction statement was rolled back but the file was marked applied")
	}
	var marked bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM migration_runner_probe_log WHERE name=$1)`, table).Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if !marked {
		t.Fatal("bookkeeping row was not committed with the file")
	}
}

// TestMigrateDownRevertsFromHead: every migration ships a down companion, so
// the operator entry point can step the schema back. From the applied head,
// one step must revert the newest migration and leave the rest applied.
// PostgreSQL-gated.
func TestMigrateDownRevertsFromHead(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	var head string
	if err := store.pool.QueryRow(ctx, `SELECT name FROM schema_migrations ORDER BY name DESC LIMIT 1`).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateDown(ctx, 1); err != nil {
		t.Fatalf("MigrateDown(1) from head %s: %v", head, err)
	}
	var applied bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, head).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatalf("MigrateDown(1) left %s recorded as applied", head)
	}
	// The reverted migration re-applies cleanly (idempotent ups).
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("re-apply after revert: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, head).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatalf("Migrate did not re-apply %s after its revert", head)
	}
}

// TestCreateMigrationRejectsSecondActiveJobForWorkload: one workload may have
// at most one queued or running migration. Two live migrations of one workload
// would split its state between machines. PostgreSQL-gated.
func TestCreateMigrationRejectsSecondActiveJobForWorkload(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "mig-guard")
	owner := testOrganizationOwner(t, store, organizationID)
	source := createRegressionMachine(t, store, organizationID, "guard-source")
	destination := createRegressionMachine(t, store, organizationID, "guard-destination")

	workloadID := mustRegressionID(t)
	if _, err := store.UpsertWorkload(ctx, WorkloadRecord{
		ID: workloadID, OrganizationID: organizationID, MachineID: source,
		Name: "guard-workload", Spec: json.RawMessage(`{"name":"guard-workload"}`),
	}, testAudit(organizationID, "workload.upsert", workloadID)); err != nil {
		t.Fatal(err)
	}
	first, created, err := store.CreateMigration(ctx, MigrationRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, WorkloadID: workloadID,
		SourceMachineID: source, DestinationMachineID: destination,
		Mode: "cold", CreatedBy: owner, IdempotencyKey: "guard-first", Managed: true,
	}, testAudit(organizationID, "migration.create", "guard"))
	if err != nil || !created {
		t.Fatalf("first migration create = (%v, %v)", first, err)
	}
	// A different key against the same workload must be refused while the
	// first job is queued.
	_, _, err = store.CreateMigration(ctx, MigrationRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, WorkloadID: workloadID,
		SourceMachineID: source, DestinationMachineID: destination,
		Mode: "cold", CreatedBy: owner, IdempotencyKey: "guard-second", Managed: true,
	}, testAudit(organizationID, "migration.create", "guard"))
	if !errors.Is(err, ErrMigrationAlreadyActive) {
		t.Fatalf("second active migration error = %v, want ErrMigrationAlreadyActive", err)
	}
	// The same key replays the original job instead of creating a new one.
	replay, created, err := store.CreateMigration(ctx, MigrationRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, WorkloadID: workloadID,
		SourceMachineID: source, DestinationMachineID: destination,
		Mode: "cold", CreatedBy: owner, IdempotencyKey: "guard-first", Managed: true,
	}, testAudit(organizationID, "migration.create", "guard"))
	if err != nil || created || replay.ID != first.ID {
		t.Fatalf("idempotent replay = (%s, %v, %v), want original job", replay.ID, created, err)
	}
	// Once the first job settles, a new migration may start.
	if _, err := store.UpdateMigrationStatus(ctx, organizationID, first.ID, "completed", json.RawMessage(`{}`), "", testAudit(organizationID, "migration.complete", first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateMigration(ctx, MigrationRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, WorkloadID: workloadID,
		SourceMachineID: source, DestinationMachineID: destination,
		Mode: "cold", CreatedBy: owner, IdempotencyKey: "guard-third", Managed: true,
	}, testAudit(organizationID, "migration.create", "guard")); err != nil {
		t.Fatalf("create after settlement = %v, want success", err)
	}
}

// createRegressionMachine registers a machine for the guard fixtures.
func createRegressionMachine(t *testing.T, store *Store, organizationID, label string) string {
	t.Helper()
	record, err := store.CreateMachine(context.Background(), MachineRecord{
		ID: mustRegressionID(t), OrganizationID: organizationID, MachineID: label, Name: label,
		AgentURL: "https://127.0.0.1:18443", Capabilities: json.RawMessage(`{}`),
	}, testAudit(organizationID, "machine.create", label))
	if err != nil {
		t.Fatal(err)
	}
	return record.MachineID
}

// TestAgentCommandClaimTokenAndExpiryAreEnforced: expired commands are never
// delivered, and a completion must match the machine and one-use claim token
// that polled the command. PostgreSQL-gated.
func TestAgentCommandClaimTokenAndExpiryAreEnforced(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "cmd-claim")
	machine := createRegressionMachine(t, store, organizationID, "cmd-machine")

	// A command that expired before anyone polled it must not be claimed.
	expired := mustRegressionID(t)
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := store.pool.Exec(ctx, `INSERT INTO agent_commands(id,organization_id,machine_id,action,payload,expires_at) VALUES($1,$2,$3,'relay.start','{}',$4)`, expired, organizationID, machine, past); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimAgentCommand(ctx, organizationID, machine); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired command claim = %v, want no delivery", err)
	}

	live := mustRegressionID(t)
	if err := store.EnqueueAgentCommand(ctx, AgentCommandRecord{
		ID: live, OrganizationID: organizationID, MachineID: machine, Action: "relay.start",
		Payload: json.RawMessage(`{"workload_id":"w"}`),
	}, testAudit(organizationID, "command.enqueue", live)); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimAgentCommand(ctx, organizationID, machine)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != live || claimed.ClaimToken == "" {
		t.Fatalf("claim = %+v, want the queued command with a claim token", claimed)
	}
	// A completion from the wrong machine, or with the wrong token, is refused.
	if err := store.CompleteAgentCommand(ctx, live, "other-machine", claimed.ClaimToken, false, ""); !errors.Is(err, ErrAgentCommandClaim) {
		t.Fatalf("foreign-machine completion = %v, want ErrAgentCommandClaim", err)
	}
	if err := store.CompleteAgentCommand(ctx, live, machine, "forged-token", false, ""); !errors.Is(err, ErrAgentCommandClaim) {
		t.Fatalf("forged-token completion = %v, want ErrAgentCommandClaim", err)
	}
	// A second completion of the same claim is a replay and is refused.
	if err := store.CompleteAgentCommand(ctx, live, machine, claimed.ClaimToken, false, ""); err != nil {
		t.Fatalf("first completion = %v", err)
	}
	if err := store.CompleteAgentCommand(ctx, live, machine, claimed.ClaimToken, false, ""); !errors.Is(err, ErrAgentCommandClaim) {
		t.Fatalf("replayed completion = %v, want ErrAgentCommandClaim", err)
	}
}

// TestEnforceRetentionSweepsReplayAndReservationTables: the fixed-lifetime
// housekeeping must retire terminal compute reservations, processed stripe
// events, expired relay request nonces, and expired queued/claimed agent
// commands — none of which any policy window covers. PostgreSQL-gated.
func TestEnforceRetentionSweepsReplayAndReservationTables(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "retention-extra")
	machine := createRegressionMachine(t, store, organizationID, "retention-machine")

	// A terminal compute reservation released long ago.
	offerID := mustRegressionID(t)
	if _, err := store.pool.Exec(ctx, `INSERT INTO compute_offers(id,organization_id,machine_id,machine_name) VALUES($1,$2,$3,$3)`, offerID, organizationID, machine); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO compute_reservations(id,offer_id,organization_id,state,expires_at,released_at,updated_at) VALUES($1,$2,$3,'RELEASED',now()-interval '120 days',now()-interval '120 days',now()-interval '120 days')`, mustRegressionID(t), offerID, organizationID); err != nil {
		t.Fatal(err)
	}
	// A stripe event processed long ago, and one never processed but long
	// received.
	staleID := mustRegressionID(t)
	oldID := mustRegressionID(t)
	if _, err := store.pool.Exec(ctx, `INSERT INTO stripe_events(event_id,event_type,payload,received_at,processed_at) VALUES($1,'x','{}',now()-interval '120 days',now()-interval '120 days'),($2,'x','{}',now()-interval '120 days',NULL)`, oldID, staleID); err != nil {
		t.Fatal(err)
	}
	// An expired relay request nonce.
	nonce := "nonce-" + mustRegressionID(t)
	if _, err := store.pool.Exec(ctx, `INSERT INTO relay_request_nonces(machine_id,nonce,expires_at) VALUES($1,$2,now()-interval '1 day')`, machine, nonce); err != nil {
		t.Fatal(err)
	}
	// An expired queued command and a fresh one that must survive.
	expiredCommand := mustRegressionID(t)
	if _, err := store.pool.Exec(ctx, `INSERT INTO agent_commands(id,organization_id,machine_id,action,payload,expires_at) VALUES($1,$2,$3,'relay.start','{}',now()-interval '1 day')`, expiredCommand, organizationID, machine); err != nil {
		t.Fatal(err)
	}
	freshCommand := mustRegressionID(t)
	if err := store.EnqueueAgentCommand(ctx, AgentCommandRecord{
		ID: freshCommand, OrganizationID: organizationID, MachineID: machine, Action: "relay.start",
		Payload: json.RawMessage(`{}`),
	}, testAudit(organizationID, "command.enqueue", freshCommand)); err != nil {
		t.Fatal(err)
	}

	sweep, err := store.EnforceRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.ReservationsPurged < 1 || sweep.StripeEventsPurged < 2 || sweep.RelayNoncesPurged < 1 {
		t.Fatalf("sweep missed replay/reservation tables: %+v", sweep)
	}
	var survivors int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM agent_commands WHERE id=$1`, expiredCommand).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors != 0 {
		t.Fatal("an expired queued command survived the retention sweep")
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM agent_commands WHERE id=$1`, freshCommand).Scan(&survivors); err != nil {
		t.Fatal(err)
	}
	if survivors != 1 {
		t.Fatal("the retention sweep removed a live agent command")
	}
}

// TestEraseUserScrubsNestedMetadataPII: audit rows the erased user touched as
// actor or as subject carry PII in metadata at any depth — invites and SCIM
// rows name the email — and none of it may survive. PostgreSQL-gated.
func TestEraseUserScrubsNestedMetadataPII(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	userID, organizationID := erasureUser(t, store, "nested-pii")

	subjectRow := mustRegressionID(t)
	nested := fmt.Sprintf(`{"invite":{"email":"nested-%s@example.test","note":"keep me"},"user_agent":"secret-agent","details":[{"email":"deep-%s@example.test","kept":true}]}`, userID, userID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO audit_events(id,organization_id,actor_user_id,action,resource_type,resource_id,metadata,remote_addr) VALUES($1,$2,NULL,'scim.user.create','user',$3,$4::jsonb,'10.0.0.9')`,
		subjectRow, organizationID, userID, nested); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EraseUser(ctx, userID, AuditInput{ID: "audit-nested-" + userID, OrganizationID: organizationID, ActorUserID: userID, Metadata: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var stored json.RawMessage
	var remote any
	if err := store.pool.QueryRow(ctx, `SELECT metadata,remote_addr FROM audit_events WHERE id=$1`, subjectRow).Scan(&stored, &remote); err != nil {
		t.Fatal(err)
	}
	if remote != nil {
		t.Fatalf("subject-side audit row kept a remote address: %v", remote)
	}
	text := string(stored)
	if strings.Contains(text, "example.test") || strings.Contains(text, "secret-agent") {
		t.Fatalf("nested PII survived erasure: %s", text)
	}
	if !strings.Contains(text, "keep me") || !strings.Contains(text, "kept") {
		t.Fatalf("erasure destroyed non-PII metadata: %s", text)
	}
}

// TestSetEntitlementSeatsIsWebhookOnlyContract guards the seat contract the
// member paths rely on: the record never validates seats downward from Go —
// purchases are recorded verbatim and negatives are refused.
func TestSetEntitlementSeatsRefusesNegative(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	organizationID := testOrganization(t, store, "seat-negative")
	if err := store.SetEntitlementSeats(ctx, organizationID, -1, ""); err == nil {
		t.Fatal("negative seat count accepted")
	}
	if err := store.SetEntitlementSeats(ctx, organizationID, 5, "si_test"); err != nil {
		t.Fatal(err)
	}
	record, err := store.Entitlement(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Seats != 5 || record.StripeItemID != "si_test" {
		t.Fatalf("entitlement = %+v, want 5 seats mirrored to si_test", record)
	}
}
