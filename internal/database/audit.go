package database

import "context"

// audit.go: reading and writing audit events. Mutations elsewhere in this
// package call insertAudit directly so the trail commits with the change.

// AuditEvents returns one page of an organization's audit trail, newest
// first. limit clamps to the documented 500-row maximum (an over-large request
// is served the maximum, never silently downgraded to the default), and
// offset walks deeper into the trail so events beyond the first page stay
// reachable.
func (store *Store) AuditEvents(ctx context.Context, organizationID string, limit, offset int) ([]AuditRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxPageLimit {
		limit = MaxPageLimit
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := store.pool.Query(ctx, `SELECT id,COALESCE(organization_id,''),COALESCE(actor_user_id,''),action,resource_type,resource_id,metadata,request_id,COALESCE(remote_addr::text,''),created_at FROM audit_events WHERE organization_id=$1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, organizationID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AuditRecord
	for rows.Next() {
		var record AuditRecord
		if err := rows.Scan(&record.ID, &record.OrganizationID, &record.ActorUserID, &record.Action, &record.ResourceType, &record.ResourceID, &record.Metadata, &record.RequestID, &record.RemoteAddr, &record.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, record)
	}
	return result, rows.Err()
}

func (store *Store) RecordAudit(ctx context.Context, input AuditInput) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := insertAudit(ctx, tx, input); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
