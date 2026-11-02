package database

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"shift.dev/shift/internal/model"
)

// ErrAgentCommandClaim reports a completion that could not be matched to the
// machine and claim token that polled the command: a stale replay, a command
// claimed by another machine, or one already completed.
var ErrAgentCommandClaim = errors.New("agent command is not claimed by this machine")

type AgentCommandRecord struct {
	ID, OrganizationID, MachineID, Action string
	Payload                               json.RawMessage
	Status, ErrorMessage                  string
	ClaimToken                            string
	ExpiresAt                             *time.Time
	CreatedAt, ClaimedAt, CompletedAt     *time.Time
}

// defaultCommandTTL bounds how long a queued command stays deliverable. An
// agent that never polls must not accumulate instructions that execute days
// later against stale state.
const defaultCommandTTL = 24 * time.Hour

func (s *Store) EnqueueAgentCommand(ctx context.Context, record AgentCommandRecord, audit AuditInput) error {
	if record.ID == "" || record.OrganizationID == "" || record.MachineID == "" || record.Action == "" || len(record.Payload) == 0 {
		return errors.New("agent command fields are required")
	}
	expiresAt := time.Now().UTC().Add(defaultCommandTTL)
	if record.ExpiresAt != nil && record.ExpiresAt.After(time.Now().UTC()) {
		expiresAt = *record.ExpiresAt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `INSERT INTO agent_commands(id,organization_id,machine_id,action,payload,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, record.ID, record.OrganizationID, record.MachineID, record.Action, record.Payload, expiresAt)
	if err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClaimAgentCommand hands the oldest live command to one machine. Expired
// commands are never delivered — an instruction whose window has passed must
// not execute against stale state — and the claim stamps a one-use token the
// completion must echo, so only the machine that polled can finalize it.
func (s *Store) ClaimAgentCommand(ctx context.Context, org, machine string) (AgentCommandRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return AgentCommandRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var claimToken string
	if claimToken, err = newClaimToken(); err != nil {
		return AgentCommandRecord{}, err
	}
	var r AgentCommandRecord
	err = tx.QueryRow(ctx, `WITH picked AS (SELECT id FROM agent_commands WHERE organization_id=$1 AND machine_id=$2 AND status='queued' AND (expires_at IS NULL OR expires_at>now()) ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
		UPDATE agent_commands c SET status='claimed',claimed_at=now(),claim_token=$3 FROM picked WHERE c.id=picked.id
		RETURNING c.id,c.organization_id,c.machine_id,c.action,c.payload,c.status,c.error_message,COALESCE(c.claim_token,''),c.expires_at,c.created_at,c.claimed_at,c.completed_at`,
		org, machine, claimToken).Scan(&r.ID, &r.OrganizationID, &r.MachineID, &r.Action, &r.Payload, &r.Status, &r.ErrorMessage, &r.ClaimToken, &r.ExpiresAt, &r.CreatedAt, &r.ClaimedAt, &r.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentCommandRecord{}, pgx.ErrNoRows
	}
	if err != nil {
		return AgentCommandRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentCommandRecord{}, err
	}
	return r, nil
}

// newClaimToken mints the one-use completion secret stamped at claim time.
func newClaimToken() (string, error) {
	return model.NewID()
}

// CompleteAgentCommand finalizes one claimed command. The claim token and
// machine must match the claim: a command claimed by one machine can never be
// completed by another principal replaying its id, and a completion cannot
// replay against a command that is no longer claimed.
func (s *Store) CompleteAgentCommand(ctx context.Context, id, machineID, claimToken string, failed bool, message string) error {
	if id == "" || machineID == "" || claimToken == "" {
		return ErrAgentCommandClaim
	}
	status := "done"
	if failed {
		status = "failed"
	}
	result, err := s.pool.Exec(ctx, `UPDATE agent_commands SET status=$4,error_message=$5,completed_at=now() WHERE id=$1 AND machine_id=$2 AND claim_token=$3 AND status='claimed'`, id, machineID, claimToken, status, message)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrAgentCommandClaim
	}
	return nil
}
