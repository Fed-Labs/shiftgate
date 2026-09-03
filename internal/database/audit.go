package database

import "context"

// audit.go: reading and writing audit events. Mutations elsewhere in this
// package call insertAudit directly so the trail commits with the change.

func (store *Store) AuditEvents(ctx context.Context, organizationID string, limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := store.pool.Query(ctx, `SELECT id,COALESCE(organization_id,''),COALESCE(actor_user_id,''),action,resource_type,resource_id,metadata,request_id,COALESCE(remote_addr::text,''),created_at FROM audit_events WHERE organization_id=$1 ORDER BY created_at DESC LIMIT $2`, organizationID, limit)
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
	defer tx.Rollback(ctx)
	if err := insertAudit(ctx, tx, input); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
