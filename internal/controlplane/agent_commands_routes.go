package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"shift.dev/shift/internal/database"
	"shift.dev/shift/internal/model"
)

// agent_commands_routes.go: the machine-bound agent command channel. Poll
// claims one queued command and complete settles the claim. Both endpoints
// verify the presenting API key is bound to the machine whose queue is being
// addressed, so one machine's key can never claim or finalize another
// machine's commands. Claims carry a lease watchdog: a command claimed and
// then interrupted (agent crash, control-plane restart, lost settlement) is
// requeued for redelivery instead of sitting 'claimed' forever.

// claimLeaseAction marks the internal watchdog rows that bound how long a
// claim may stay unsettled. They are control traffic: the poll handler
// consumes them and only ever delivers real work to the agent.
const claimLeaseAction = "internal.claim_lease"

// maxClaimSteps bounds how many control rows one poll may consume before it
// answers, so a backlog of lease watchdogs cannot stall command delivery.
const maxClaimSteps = 8

// defaultCommandClaimLease is the claim lease: how long a claimed command may
// stay unsettled before it is failed and requeued for redelivery. It is
// comfortably longer than one command execution, so slow handlers are not
// redelivered while they run, and short enough that a crashed agent's queue
// recovers on its next poll instead of never.
const defaultCommandClaimLease = 5 * time.Minute

// claimLeaseRecord is the watchdog payload: everything needed to detect a
// stranded claim and requeue the command it guards.
type claimLeaseRecord struct {
	TargetID      string          `json:"target_id"`
	TargetAction  string          `json:"target_action"`
	TargetPayload json.RawMessage `json:"target_payload"`
	ClaimToken    string          `json:"claim_token"`
	ClaimedAt     time.Time       `json:"claimed_at"`
	Attempt       int             `json:"attempt"`
}

// machineKeyBoundTo reports whether an API key is bound to one machine. A
// machine key is bound by its name ("machine:<machine-id>") or by carrying a
// "machine:<machine-id>" scope. The command channel is machine identity: an
// unbound (organization-wide) key must not reach it.
func machineKeyBoundTo(keyName string, scopes []string, machineID string) bool {
	if strings.TrimSpace(machineID) == "" {
		return false
	}
	want := "machine:" + strings.TrimSpace(machineID)
	if strings.EqualFold(strings.TrimSpace(keyName), want) {
		return true
	}
	for _, scope := range scopes {
		if strings.EqualFold(strings.TrimSpace(scope), want) {
			return true
		}
	}
	return false
}

// authorizeMachineCommand gates one command-channel call. It re-resolves the
// presenting key to its record so the machine binding (key name and scopes)
// can be verified against the machine in the path, and refuses any key bound
// to another machine or to none.
func (server *Server) authorizeMachineCommand(writer http.ResponseWriter, request *http.Request, organizationID, machineID string) bool {
	principal := principalFrom(request.Context())
	if principal.APIKeyID == "" || principal.OrganizationID != organizationID || strings.TrimSpace(machineID) == "" {
		writeError(writer, http.StatusForbidden, "AGENT_COMMAND_FORBIDDEN", "machine-scoped API key required")
		return false
	}
	token, ok := parseBearer(request.Header.Get("Authorization"))
	if !ok {
		writeError(writer, http.StatusForbidden, "AGENT_COMMAND_FORBIDDEN", "machine-scoped API key required")
		return false
	}
	record, err := server.database.AuthenticateAPIKey(request.Context(), tokenDigest(server.config.TokenPepper, token), time.Now().UTC())
	if err != nil {
		writeError(writer, http.StatusForbidden, "AGENT_COMMAND_FORBIDDEN", "machine-scoped API key required")
		return false
	}
	if !machineKeyBoundTo(record.Name, principal.Scopes, machineID) {
		writeError(writer, http.StatusForbidden, "AGENT_COMMAND_MACHINE_FORBIDDEN", "the API key is not bound to this machine")
		return false
	}
	return true
}

func (server *Server) handleAgentCommandPoll(writer http.ResponseWriter, request *http.Request) {
	org := request.PathValue("organizationID")
	machine := request.PathValue("machineID")
	if !server.authorizeMachineCommand(writer, request, org, machine) {
		return
	}
	for step := 0; step < maxClaimSteps; step++ {
		command, err := server.database.ClaimAgentCommand(request.Context(), org, machine)
		if errors.Is(err, pgx.ErrNoRows) {
			writeNoContent(writer)
			return
		}
		if err != nil {
			server.writeInternalError(writer, request, http.StatusInternalServerError, "AGENT_COMMAND_POLL_FAILED", err)
			return
		}
		if command.Action == claimLeaseAction {
			// Internal lease row: settle or re-arm it and keep looking for
			// real work. A re-arm is the newest row in the queue; claiming
			// further would walk into it, so the poll ends here and the next
			// poll picks real work up.
			if server.settleClaimLease(request.Context(), org, machine, command) {
				writeNoContent(writer)
				return
			}
			continue
		}
		// A claimed relay command proves its migration is live on some agent:
		// re-seed the unmanaged settlement watch from it.
		server.registerCommandMigration(org, command)
		server.armClaimLease(request.Context(), org, machine, command)
		writeJSON(writer, http.StatusOK, map[string]any{"id": command.ID, "action": command.Action, "payload": command.Payload, "claim_token": command.ClaimToken, "created_at": command.CreatedAt})
		return
	}
	writeNoContent(writer)
}

// writeNoContent answers a poll with no deliverable work. A 204 must carry no
// body at all — writeJSON would encode null after the status line.
func writeNoContent(writer http.ResponseWriter) {
	writer.WriteHeader(http.StatusNoContent)
}

func (server *Server) handleAgentCommandComplete(writer http.ResponseWriter, request *http.Request) {
	org := request.PathValue("organizationID")
	machine := request.PathValue("machineID")
	id := request.PathValue("commandID")
	if !server.authorizeMachineCommand(writer, request, org, machine) {
		return
	}
	if id == "" {
		writeError(writer, http.StatusForbidden, "AGENT_COMMAND_FORBIDDEN", "machine-scoped API key required")
		return
	}
	var input struct {
		Failed     bool   `json:"failed"`
		Error      string `json:"error,omitempty"`
		ClaimToken string `json:"claim_token,omitempty"`
	}
	if !decodeJSON(writer, request, &input) {
		return
	}
	if err := server.database.CompleteAgentCommand(request.Context(), id, machine, strings.TrimSpace(input.ClaimToken), input.Failed, strings.TrimSpace(input.Error)); err != nil {
		if errors.Is(err, database.ErrAgentCommandClaim) {
			writeError(writer, http.StatusConflict, "AGENT_COMMAND_COMPLETE_FAILED", "command is not claimed by this machine")
			return
		}
		server.writeInternalError(writer, request, http.StatusInternalServerError, "AGENT_COMMAND_COMPLETE_FAILED", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "completed"})
}

// claimLeaseDuration is the effective claim lease, defaulting when the server
// was built without one.
func (server *Server) claimLeaseDuration() time.Duration {
	if server.commandClaimLease <= 0 {
		return defaultCommandClaimLease
	}
	return server.commandClaimLease
}

// armClaimLease records a lease watchdog for one freshly claimed command. The
// watchdog is an ordinary queued command row the poll loop consumes: if the
// claim is still unsettled when the lease expires, the guarded command is
// failed and requeued for redelivery. The watchdog is armed after the claim
// commits, so its payload carries the one-use claim token needed to release
// the guarded row.
func (server *Server) armClaimLease(ctx context.Context, org, machine string, command database.AgentCommandRecord) {
	claimedAt := time.Now().UTC()
	if command.ClaimedAt != nil {
		claimedAt = command.ClaimedAt.UTC()
	}
	server.enqueueClaimLease(ctx, org, machine, claimLeaseRecord{
		TargetID: command.ID, TargetAction: command.Action, TargetPayload: command.Payload,
		ClaimToken: command.ClaimToken, ClaimedAt: claimedAt, Attempt: 1,
	}, command.ID)
}

// enqueueClaimLease writes one lease watchdog row, re-arming included. The
// audit names the guarded command so operators can trace redeliveries.
func (server *Server) enqueueClaimLease(ctx context.Context, org, machine string, lease claimLeaseRecord, guardedID string) {
	payload, err := json.Marshal(lease)
	if err != nil {
		return
	}
	leaseID, err := model.NewID()
	if err != nil {
		return
	}
	auditID, err := model.NewID()
	if err != nil {
		return
	}
	if err := server.database.EnqueueAgentCommand(ctx, database.AgentCommandRecord{
		ID: leaseID, OrganizationID: org, MachineID: machine, Action: claimLeaseAction, Payload: payload,
	}, database.AuditInput{ID: auditID, OrganizationID: org, Action: "agent_command.lease", ResourceType: "agent_command", ResourceID: guardedID, Metadata: map[string]any{"machine_id": machine, "attempt": lease.Attempt}}); err != nil {
		server.logger.Warn("could not arm agent command claim lease", "command_id", guardedID, "error", err)
	}
}

// settleClaimLease processes one lease watchdog and reports whether it
// re-armed a successor. Before the deadline it only re-arms; past the
// deadline it releases and requeues the guarded command if that claim never
// settled, and retires quietly when it did.
func (server *Server) settleClaimLease(ctx context.Context, org, machine string, leaseRow database.AgentCommandRecord) bool {
	var lease claimLeaseRecord
	if err := json.Unmarshal(leaseRow.Payload, &lease); err != nil || lease.TargetID == "" || lease.ClaimToken == "" {
		server.completeAgentCommandRow(ctx, machine, leaseRow, true, "claim lease payload is invalid")
		return false
	}
	if time.Now().UTC().Before(lease.ClaimedAt.Add(server.claimLeaseDuration())) {
		// The guarded command is still within its claim lease. Re-arm so a
		// later poll can still detect a stranded claim.
		lease.Attempt++
		server.enqueueClaimLease(ctx, org, machine, lease, lease.TargetID)
		server.completeAgentCommandRow(ctx, machine, leaseRow, false, "claim lease re-armed")
		return true
	}
	err := server.database.CompleteAgentCommand(ctx, lease.TargetID, machine, lease.ClaimToken, true, "claim lease expired before the command settled; requeued for redelivery")
	switch {
	case errors.Is(err, database.ErrAgentCommandClaim):
		// The guarded command already settled — or a sibling watchdog
		// requeued it first. Either way there is nothing left to release.
		server.completeAgentCommandRow(ctx, machine, leaseRow, false, "guarded command already settled")
		return false
	case err != nil:
		// Transient failure: keep the watchdog alive instead of losing it.
		server.logger.Warn("claim lease could not release the guarded command", "command_id", lease.TargetID, "error", err)
		lease.Attempt++
		server.enqueueClaimLease(ctx, org, machine, lease, lease.TargetID)
		server.completeAgentCommandRow(ctx, machine, leaseRow, true, "claim lease settlement failed; re-armed")
		return true
	}
	redeliveryID, idErr := model.NewID()
	if idErr != nil {
		server.completeAgentCommandRow(ctx, machine, leaseRow, true, "claim lease requeue failed")
		return false
	}
	auditID, auditErr := model.NewID()
	if auditErr != nil {
		server.completeAgentCommandRow(ctx, machine, leaseRow, true, "claim lease requeue failed")
		return false
	}
	if err := server.database.EnqueueAgentCommand(ctx, database.AgentCommandRecord{
		ID: redeliveryID, OrganizationID: org, MachineID: machine, Action: lease.TargetAction, Payload: lease.TargetPayload,
	}, database.AuditInput{ID: auditID, OrganizationID: org, Action: "agent_command.requeue", ResourceType: "agent_command", ResourceID: lease.TargetID, Metadata: map[string]any{"machine_id": machine, "redelivered_as": redeliveryID}}); err != nil {
		server.logger.Warn("claim lease could not requeue the guarded command", "command_id", lease.TargetID, "error", err)
		server.completeAgentCommandRow(ctx, machine, leaseRow, true, "claim lease requeue failed")
		return false
	}
	server.logger.Info("agent command requeued after claim lease expiry", "command_id", lease.TargetID, "redelivered_as", redeliveryID, "machine_id", machine)
	server.completeAgentCommandRow(ctx, machine, leaseRow, false, "guarded command requeued for redelivery")
	return false
}

// completeAgentCommandRow settles one internal control row it just claimed.
func (server *Server) completeAgentCommandRow(ctx context.Context, machine string, row database.AgentCommandRecord, failed bool, message string) {
	if err := server.database.CompleteAgentCommand(ctx, row.ID, machine, row.ClaimToken, failed, message); err != nil && !errors.Is(err, database.ErrAgentCommandClaim) {
		server.logger.Warn("could not settle agent command control row", "command_id", row.ID, "error", err)
	}
}
