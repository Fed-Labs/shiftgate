package database

import "context"

// entitlements.go: plan entitlements and Stripe subscription application.
// ApplyStripeSubscription records the event id first (ON CONFLICT DO NOTHING),
// so a redelivered webhook is detected and skipped instead of double-applying
// a plan change.

func (store *Store) Entitlement(ctx context.Context, organizationID string) (EntitlementRecord, error) {
	var record EntitlementRecord
	err := store.pool.QueryRow(ctx, `SELECT organization_id,plan,status,max_storage_bytes,used_storage_bytes,max_machines,COALESCE(stripe_customer_id,''),COALESCE(stripe_subscription_id,'') FROM entitlements WHERE organization_id=$1`, organizationID).
		Scan(&record.OrganizationID, &record.Plan, &record.Status, &record.MaxStorageBytes, &record.UsedStorageBytes, &record.MaxMachines, &record.StripeCustomerID, &record.StripeSubscriptionID)
	return record, err
}

func (store *Store) ApplyStripeSubscription(ctx context.Context, eventID, eventType string, payload []byte, organizationID, plan, status, customerID, subscriptionID string, maxStorageBytes int64, maxMachines int) (bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	command, err := tx.Exec(ctx, `INSERT INTO stripe_events(event_id,event_type,payload) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, eventID, eventType, payload)
	if err != nil {
		return false, err
	}
	if command.RowsAffected() == 0 {
		return false, nil
	}
	if organizationID != "" {
		if _, err := tx.Exec(ctx, `UPDATE entitlements SET plan=$2,status=$3,max_storage_bytes=$4,max_machines=$5,stripe_customer_id=NULLIF($6,''),stripe_subscription_id=NULLIF($7,''),updated_at=now() WHERE organization_id=$1`,
			organizationID, plan, status, maxStorageBytes, maxMachines, customerID, subscriptionID); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE stripe_events SET processed_at=now() WHERE event_id=$1`, eventID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
