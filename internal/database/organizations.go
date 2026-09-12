package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// organizations.go: membership lookup and member administration.

func (store *Store) Organizations(ctx context.Context, userID string) ([]OrganizationRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT o.id,o.name,m.role,o.created_at FROM organizations o JOIN organization_members m ON m.organization_id=o.id WHERE m.user_id=$1 ORDER BY o.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []OrganizationRecord
	for rows.Next() {
		var record OrganizationRecord
		if err := rows.Scan(&record.ID, &record.Name, &record.Role, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) MemberRole(ctx context.Context, organizationID, userID string) (string, error) {
	var role string
	err := store.pool.QueryRow(ctx, `SELECT role FROM organization_members WHERE organization_id=$1 AND user_id=$2`, organizationID, userID).Scan(&role)
	return role, err
}

func (store *Store) AddMember(ctx context.Context, organizationID, email, role string, audit AuditInput) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email)=lower($1) AND disabled_at IS NULL`, email).Scan(&userID); err != nil {
		return err
	}
	var existingRole string
	err = tx.QueryRow(ctx, `SELECT role FROM organization_members WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, userID).Scan(&existingRole)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if existingRole == "owner" && role != "owner" {
		return ErrOwnerRoleImmutable
	}
	if _, err := tx.Exec(ctx, `INSERT INTO organization_members(organization_id,user_id,role) VALUES($1,$2,$3)
		ON CONFLICT(organization_id,user_id) DO UPDATE SET role=EXCLUDED.role`, organizationID, userID, role); err != nil {
		return err
	}
	audit.ResourceID = userID
	if err := insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
