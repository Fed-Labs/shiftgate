package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/model"
	"time"

	"github.com/jackc/pgx/v5"
)

// referral.go: the give-a-month-get-a-month referral loop. Codes belong to
// organizations; activation is the referred org's first successful migration;
// the reward is credited once per activation, max 12. Self-referral is
// blocked at claim time by the same-email-domain check in the route.

// ReferralCode is one organization's share code.
type ReferralCode struct {
	Code           string
	OrganizationID string
	CreatedAt      time.Time
}

// ErrReferralSelfClaim is returned when a claim would credit the code's own
// organization (same org, or same email domain).
var ErrReferralSelfClaim = errors.New("referral code cannot be claimed by its own organization")

// ErrReferralAlreadyClaimed is returned when an organization tries to claim a
// second, different referral code; the first claim keeps the credit.
var ErrReferralAlreadyClaimed = errors.New("organization has already claimed a referral code")

// EnsureReferralCode returns the organization's code, creating one on first
// use. The code is random, not derived from the org id. Concurrent first-use
// calls converge on one row: the insert ignores any unique violation and the
// winner's row is re-read, so one organization never owns two codes.
func (store *Store) EnsureReferralCode(ctx context.Context, organizationID string) (ReferralCode, error) {
	var record ReferralCode
	load := func() error {
		return store.pool.QueryRow(ctx, `SELECT code,organization_id,created_at FROM referral_codes WHERE organization_id=$1`, organizationID).
			Scan(&record.Code, &record.OrganizationID, &record.CreatedAt)
	}
	err := load()
	if err == nil {
		return record, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReferralCode{}, err
	}
	bytes := make([]byte, 6)
	if _, err := rand.Read(bytes); err != nil {
		return ReferralCode{}, err
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO referral_codes(code,organization_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, "ref-"+hex.EncodeToString(bytes), organizationID); err != nil {
		return ReferralCode{}, err
	}
	if err := load(); err != nil {
		return ReferralCode{}, err
	}
	return record, nil
}

// ClaimReferral records that an organization signed up with a referral code.
// The code must exist and must not belong to the claiming organization —
// neither the same organization nor the same owner email domain. One referred
// organization claims at most one code, ever: a retried claim of the same
// code is a no-op, and a second, different code is refused with
// ErrReferralAlreadyClaimed while the first claim keeps the credit.
func (store *Store) ClaimReferral(ctx context.Context, code, referredOrgID string) error {
	var ownerOrg string
	err := store.pool.QueryRow(ctx, `SELECT organization_id FROM referral_codes WHERE code=$1`, code).Scan(&ownerOrg)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgx.ErrNoRows
		}
		return err
	}
	if ownerOrg == referredOrgID {
		return ErrReferralSelfClaim
	}
	ownerEmail, err := store.OwnerEmail(ctx, ownerOrg)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	referredEmail, err := store.OwnerEmail(ctx, referredOrgID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if domain := EmailDomain(ownerEmail); domain != "" && domain == EmailDomain(referredEmail) {
		return ErrReferralSelfClaim
	}
	id, err := model.NewID()
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `INSERT INTO referral_events(id,ref_code,referred_org_id) VALUES($1,$2,$3) ON CONFLICT (referred_org_id) DO NOTHING`, id, code, referredOrgID)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var existing string
		if err := store.pool.QueryRow(ctx, `SELECT ref_code FROM referral_events WHERE referred_org_id=$1`, referredOrgID).Scan(&existing); err != nil {
			return err
		}
		if existing != code {
			return ErrReferralAlreadyClaimed
		}
		return nil
	}
	_, err = store.pool.Exec(ctx, `UPDATE organizations SET referred_by=$2 WHERE id=$1 AND referred_by=''`, referredOrgID, code)
	return err
}

// ReferralRewardCap is the advertised "up to 12" rewards per referral code.
const ReferralRewardCap = 12

// GrantReferralReward marks one activated referral as rewarded and reports
// whether this call granted the reward. It is idempotent (a second call
// returns false) and enforces ReferralRewardCap per code under the event row
// lock, so concurrent activations cannot overshoot the cap. The month credit
// for both parties is applied by the caller once this returns true.
func (store *Store) GrantReferralReward(ctx context.Context, referredOrgID string) (bool, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var eventID, refCode string
	var rewarded bool
	var activatedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT id,ref_code,reward_granted,activated_at FROM referral_events WHERE referred_org_id=$1 FOR UPDATE`, referredOrgID).
		Scan(&eventID, &refCode, &rewarded, &activatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if rewarded || activatedAt == nil {
		return false, nil
	}
	var granted int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM referral_events WHERE ref_code=$1 AND reward_granted`, refCode).Scan(&granted); err != nil {
		return false, err
	}
	if granted >= ReferralRewardCap {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE referral_events SET reward_granted=true WHERE id=$1`, eventID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// ActivateReferral marks the referred organization's first successful
// migration and reports whether a reward is now due (idempotent: a second
// activation returns reward=false).
func (store *Store) ActivateReferral(ctx context.Context, referredOrgID string, at time.Time) (bool, error) {
	command, err := store.pool.Exec(ctx, `UPDATE referral_events SET activated_at=$2 WHERE referred_org_id=$1 AND activated_at IS NULL`, referredOrgID, at.UTC())
	if err != nil {
		return false, err
	}
	return command.RowsAffected() > 0, nil
}

// ReferralStats summarizes one code's activity for the settings card.
type ReferralStats struct {
	Code      string `json:"code"`
	Invited   int    `json:"invited"`
	Activated int    `json:"activated"`
	Rewards   int    `json:"rewards_granted"`
	Badge     string `json:"badge,omitempty"`
}

// ReferralStatsFor loads the organization's referral scoreboard.
func (store *Store) ReferralStatsFor(ctx context.Context, organizationID string) (ReferralStats, error) {
	record, err := store.EnsureReferralCode(ctx, organizationID)
	if err != nil {
		return ReferralStats{}, err
	}
	stats := ReferralStats{Code: record.Code}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(activated_at) FILTER (WHERE activated_at IS NOT NULL), COUNT(*) FILTER (WHERE reward_granted) FROM referral_events WHERE ref_code=$1`, record.Code).
		Scan(&stats.Invited, &stats.Activated, &stats.Rewards); err != nil {
		return ReferralStats{}, err
	}
	if stats.Activated >= 3 {
		stats.Badge = "Founding Mover"
	}
	return stats, nil
}

// referralRewardDays is the advertised "a month" of the give-a-month-get-a-
// month reward.
const referralRewardDays = 30

// CreditReferralMonth applies the reward's month to both parties: the
// referring organization and the referred organization each get a month of
// Pro capacity from now. Capacity is only ever added (GREATEST — a running
// trial or larger plan is never downgraded by a reward), the month is
// expressed as an extended trial expiry so the existing release pass returns
// each party to its earned tier when the window runs out, and an organization
// whose capacity is subscription-governed keeps the subscription's numbers at
// the next webhook sync. Call it once per activation, after
// GrantReferralReward reports the grant.
func (store *Store) CreditReferralMonth(ctx context.Context, referredOrgID string, now time.Time) error {
	var refCode string
	if err := store.pool.QueryRow(ctx, `SELECT ref_code FROM referral_events WHERE referred_org_id=$1 AND reward_granted`, referredOrgID).Scan(&refCode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	var referrerOrg string
	if err := store.pool.QueryRow(ctx, `SELECT organization_id FROM referral_codes WHERE code=$1`, refCode).Scan(&referrerOrg); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	pro, ok := billing.FindPlan("pro")
	if !ok {
		return errors.New("pro plan missing from catalog")
	}
	monthEnd := now.UTC().AddDate(0, 0, referralRewardDays)
	for _, organizationID := range []string{referrerOrg, referredOrgID} {
		if organizationID == "" {
			continue
		}
		if _, err := store.pool.Exec(ctx, `UPDATE entitlements SET
			max_storage_bytes = GREATEST(max_storage_bytes, $2),
			max_machines = GREATEST(max_machines, $3),
			trial_expires_at = GREATEST(COALESCE(trial_expires_at, $4), $4),
			updated_at = now()
		WHERE organization_id = $1`, organizationID, billing.EffectiveStorageBytes(pro, 1), pro.MaxMachines, monthEnd); err != nil {
			return err
		}
	}
	return nil
}
