package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"shift.dev/shift/internal/billing"
)

// organizations.go: membership lookup and member administration.

// Organizations returns one page of the organizations the user belongs to.
func (store *Store) Organizations(ctx context.Context, userID string, page Page) ([]OrganizationRecord, error) {
	rows, err := store.pool.Query(ctx, `SELECT o.id,o.name,m.role,o.created_at FROM organizations o JOIN organization_members m ON m.organization_id=o.id WHERE m.user_id=$1 ORDER BY o.created_at`+page.clause(), userID)
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

// OwnerEmail returns the organization's founding owner address — the billing
// contact of last resort when the caller is an API key and carries no email.
func (store *Store) OwnerEmail(ctx context.Context, organizationID string) (string, error) {
	var email string
	err := store.pool.QueryRow(ctx, `SELECT u.email FROM organization_members m JOIN users u ON u.id=m.user_id
		WHERE m.organization_id=$1 AND m.role='owner' AND u.disabled_at IS NULL ORDER BY u.created_at LIMIT 1`, organizationID).Scan(&email)
	return email, err
}

// CountMembers counts an organization's current members. Per-seat billing
// sizes its ceiling against this number, and checkout refuses to buy fewer
// seats than the team already has. Deactivated (SCIM-deleted) accounts are
// not members in the billing sense: they cannot sign in and do not occupy a
// seat.
func (store *Store) CountMembers(ctx context.Context, organizationID string) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `SELECT count(*) FROM organization_members m JOIN users u ON u.id = m.user_id WHERE m.organization_id=$1 AND u.disabled_at IS NULL`, organizationID).Scan(&count)
	return count, err
}

// ErrSeatCeilingExceeded reports that a member add would exceed the seat
// quantity the organization purchased for its per-seat plan.
var ErrSeatCeilingExceeded = errors.New("seat count must cover every organization member")

// lockSeatGate serializes per-organization membership growth. It takes the
// entitlements row lock (the same gate machine registration and checkpoint
// storage use) so concurrent adds queue behind one another instead of both
// reading the same member count; when no entitlements row exists it falls
// back to a per-organization advisory lock, which serializes just the same.
// Callers must hold a transaction.
func lockSeatGate(ctx context.Context, tx pgx.Tx, organizationID string) error {
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT true FROM entitlements WHERE organization_id=$1 FOR UPDATE`, organizationID).Scan(&locked); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "org-seats:"+organizationID); err != nil {
			return err
		}
	}
	return nil
}

// seatCeilingFor reads the per-seat ceiling an insert must respect, under the
// caller's row lock. It mirrors billing.SeatCeiling exactly: flat-with-included
// plans (Business: 5 included, extra seats purchased on top) admit
// IncludedSeats at minimum and grow with the purchased count, legacy per-seat
// plans bound by the raw purchase. A non-positive ceiling means unlimited.
func seatCeilingFor(ctx context.Context, tx pgx.Tx, organizationID string) (int, error) {
	var plan string
	var seats int
	err := tx.QueryRow(ctx, `SELECT COALESCE(plan,''),COALESCE(seats,0) FROM entitlements WHERE organization_id=$1`, organizationID).Scan(&plan, &seats)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	if known, ok := billing.FindPlan(plan); ok {
		return billing.SeatCeiling(known, seats), nil
	}
	return 0, nil
}

// AddMember upserts one organization membership. seatCeiling > 0 enforces the
// per-seat billing ceiling inside the same transaction as the insert; the
// ceiling check is serialized on the organization's entitlements row, so two
// concurrent adds cannot both slip past a members-only check. Re-updating an
// existing member (a role change) never counts against the ceiling; a
// ceiling of zero or less means unlimited.
func (store *Store) AddMember(ctx context.Context, organizationID, email, role string, seatCeiling int, audit AuditInput) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var userID string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE lower(email)=lower($1) AND disabled_at IS NULL`, email).Scan(&userID); err != nil {
		return err
	}
	if seatCeiling > 0 {
		if err := lockSeatGate(ctx, tx, organizationID); err != nil {
			return err
		}
	}
	var existingRole string
	err = tx.QueryRow(ctx, `SELECT role FROM organization_members WHERE organization_id=$1 AND user_id=$2 FOR UPDATE`, organizationID, userID).Scan(&existingRole)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if existingRole == "owner" && role != "owner" {
		return ErrOwnerRoleImmutable
	}
	if existingRole == "" && seatCeiling > 0 {
		var members int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM organization_members m JOIN users u ON u.id = m.user_id WHERE m.organization_id=$1 AND u.disabled_at IS NULL`, organizationID).Scan(&members); err != nil {
			return err
		}
		if members+1 > seatCeiling {
			return ErrSeatCeilingExceeded
		}
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

// FreeOrganizationsForEmailDomain counts organizations whose owner email
// domain matches and whose entitlement is the free plan — the multi-account
// velocity heuristic (plan 4 BR-13b). Audit-only; bans nothing.
func (store *Store) FreeOrganizationsForEmailDomain(ctx context.Context, domain string) (int, error) {
	var count int
	err := store.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT o.id) FROM organizations o JOIN organization_members m ON m.organization_id = o.id AND m.role = 'owner' JOIN users u ON u.id = m.user_id JOIN entitlements e ON e.organization_id = o.id WHERE lower(split_part(u.email, '@', 2)) = lower($1) AND (e.plan = 'free' OR e.plan = '') AND e.status = 'active'`, domain).Scan(&count)
	return count, err
}
