package database

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrOwnershipConflict = errors.New("workload ownership epoch or token is stale")

func tokenHash(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func (store *Store) AcquireOwnership(ctx context.Context, organizationID, workloadID, machineID, token string, lease time.Duration, audit AuditInput) (OwnershipRecord, error) {
	if organizationID == "" || workloadID == "" || machineID == "" || token == "" || lease <= 0 {
		return OwnershipRecord{}, errors.New("ownership fields and lease are required")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return OwnershipRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var workloadOrganization string
	if err := tx.QueryRow(ctx, `SELECT organization_id FROM workloads WHERE id=$1 FOR UPDATE`, workloadID).Scan(&workloadOrganization); err != nil {
		return OwnershipRecord{}, err
	}
	if workloadOrganization != organizationID {
		return OwnershipRecord{}, ErrOwnershipConflict
	}
	var current OwnershipRecord
	err = tx.QueryRow(ctx, `SELECT workload_id,organization_id,epoch,owner_machine_id,state,lease_expires_at,updated_at FROM workload_ownership WHERE workload_id=$1 FOR UPDATE`, workloadID).
		Scan(&current.WorkloadID, &current.OrganizationID, &current.Epoch, &current.OwnerMachineID, &current.State, &current.LeaseExpiresAt, &current.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		current = OwnershipRecord{WorkloadID: workloadID, OrganizationID: organizationID, Epoch: 0, State: "unowned"}
	} else if err != nil {
		return OwnershipRecord{}, err
	}
	if current.OrganizationID != "" && current.OrganizationID != organizationID {
		return OwnershipRecord{}, ErrOwnershipConflict
	}
	if current.State == "owned" && current.LeaseExpiresAt != nil && current.LeaseExpiresAt.After(time.Now().UTC()) && current.OwnerMachineID != machineID {
		return OwnershipRecord{}, ErrOwnershipConflict
	}
	current.Epoch++
	current.OrganizationID = organizationID
	current.OwnerMachineID = machineID
	current.State = "owned"
	current.LeaseExpiresAt = ptrTime(time.Now().UTC().Add(lease))
	current.UpdatedAt = time.Now().UTC()
	_, err = tx.Exec(ctx, `INSERT INTO workload_ownership(workload_id,organization_id,epoch,owner_machine_id,state,token_hash,lease_expires_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(workload_id) DO UPDATE SET organization_id=EXCLUDED.organization_id,epoch=EXCLUDED.epoch,owner_machine_id=EXCLUDED.owner_machine_id,state=EXCLUDED.state,token_hash=EXCLUDED.token_hash,lease_expires_at=EXCLUDED.lease_expires_at,updated_at=EXCLUDED.updated_at`,
		workloadID, organizationID, current.Epoch, machineID, current.State, tokenHash(token), current.LeaseExpiresAt, current.UpdatedAt)
	if err != nil {
		return OwnershipRecord{}, err
	}
	if err := insertAudit(ctx, tx, audit); err != nil {
		return OwnershipRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return OwnershipRecord{}, err
	}
	return current, nil
}

func (store *Store) RenewOwnership(ctx context.Context, workloadID, machineID, token string, epoch uint64, lease time.Duration) error {
	command, err := store.pool.Exec(ctx, `UPDATE workload_ownership SET lease_expires_at=$5,updated_at=now() WHERE workload_id=$1 AND owner_machine_id=$2 AND epoch=$3 AND token_hash=$4 AND state='owned'`, workloadID, machineID, epoch, tokenHash(token), time.Now().UTC().Add(lease))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrOwnershipConflict
	}
	return nil
}

func (store *Store) MarkOwnershipFenced(ctx context.Context, organizationID, workloadID, machineID, token string, epoch uint64) error {
	command, err := store.pool.Exec(ctx, `UPDATE workload_ownership SET state='fenced',lease_expires_at=NULL,updated_at=now() WHERE organization_id=$1 AND workload_id=$2 AND owner_machine_id=$3 AND epoch=$4 AND token_hash=$5`, organizationID, workloadID, machineID, epoch, tokenHash(token))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrOwnershipConflict
	}
	return nil
}

func ptrTime(value time.Time) *time.Time { return &value }
