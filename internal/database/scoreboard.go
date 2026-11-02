package database

import (
	"context"
	"fmt"
	"time"

	"shift.dev/shift/internal/billing"
)

// scoreboard.go: the privacy-minimal counters behind the public numbers page
// and the weekly scoreboard (master BR-05). Counts only — never workload
// contents, emails, or per-org details beyond what the public page ships.

// PublicStats is the public numbers-page payload (W10): installs, restore
// medians, MRR band, cost band. Bands are rounded deliberately: the page
// sells transparency, not a financial statement.
type PublicStats struct {
	Organizations int    `json:"organizations"`
	Machines      int    `json:"machines_registered"`
	Checkpoints   int    `json:"checkpoints_created"`
	Migrations    int    `json:"migrations_total"`
	MigrationsOK  int    `json:"migrations_succeeded"`
	RestoreP50MS  int64  `json:"restore_p50_ms"`
	MRRBandUSD    string `json:"mrr_band_usd"`
	CogsBandUSD   string `json:"cogs_band_usd"`
	GeneratedAt   string `json:"generated_at"`
}

// PublicStats reads the public counters. MRR/COGS are banded at query time
// (the raw sums never leave this function) so the public surface cannot leak
// exact revenue.
func (store *Store) PublicStats(ctx context.Context) (PublicStats, error) {
	stats := PublicStats{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM organizations`).Scan(&stats.Organizations); err != nil {
		return stats, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM machines`).Scan(&stats.Machines); err != nil {
		return stats, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM checkpoints`).Scan(&stats.Checkpoints); err != nil {
		return stats, err
	}
	var total, succeeded int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE status='completed') FROM migration_jobs`).Scan(&total, &succeeded); err != nil {
		return stats, err
	}
	stats.Migrations = total
	stats.MigrationsOK = succeeded
	var p50 *int64
	if err := store.pool.QueryRow(ctx, `SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY (result->>'duration_ms')::bigint) FROM migration_jobs WHERE status='completed' AND mode='cold' AND result ? 'duration_ms'`).Scan(&p50); err != nil {
		return stats, err
	}
	if p50 != nil {
		stats.RestoreP50MS = *p50
	}
	mrr, err := store.entitledMRRCents(ctx)
	if err != nil {
		return stats, err
	}
	stats.MRRBandUSD = band(mrr/100, []int64{0, 100, 500, 1000, 5000, 10000, 50000})
	var gb int64
	_ = store.pool.QueryRow(ctx, `SELECT COALESCE(SUM(used_storage_bytes),0) / 1073741824 FROM entitlements`).Scan(&gb)
	stats.CogsBandUSD = band(gb*2/100, []int64{0, 10, 50, 100, 500, 1000, 5000}) // $0.020/GB-mo
	return stats, nil
}

// band rounds a dollar value to a public band like "$500–1k".
func band(value int64, steps []int64) string {
	lower := int64(0)
	for _, step := range steps {
		if value < step {
			break
		}
		lower = step
	}
	upper := int64(0)
	for _, step := range steps {
		if step > lower {
			upper = step
			break
		}
	}
	if upper == 0 {
		return "$" + formatUSD(lower) + "+"
	}
	return "$" + formatUSD(lower) + "–" + formatUSD(upper)
}

func formatUSD(value int64) string {
	if value >= 1000 {
		return fmt.Sprintf("%dk", value/1000)
	}
	return fmt.Sprintf("%d", value)
}

const entitledStatusesSQL = `status IN ('active','trialing','past_due')`

func planMRRCents(planName string, seats, seatsIncluded int) int64 {
	plan, ok := billing.FindPlan(planName)
	if !ok {
		return 0
	}
	included := seatsIncluded
	if included < plan.IncludedSeats {
		included = plan.IncludedSeats
	}
	cents := int64(plan.PriceCents)
	if extra := seats - included; extra > 0 {
		cents += int64(plan.SeatOverageCents) * int64(extra)
	}
	return cents
}

func (store *Store) entitledMRRCents(ctx context.Context) (int64, error) {
	rows, err := store.pool.Query(ctx, `SELECT COALESCE(plan,''), COALESCE(seats,0), COALESCE(seats_included,0) FROM entitlements WHERE `+entitledStatusesSQL)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var plan string
		var seats, seatsIncluded int
		if err := rows.Scan(&plan, &seats, &seatsIncluded); err != nil {
			return 0, err
		}
		total += planMRRCents(plan, seats, seatsIncluded)
	}
	return total, rows.Err()
}

// ScoreboardCounters is the weekly scoreboard (master BR-05): one week of
// counts plus the week before, so the response is week-over-week without a
// second round trip. Privacy-minimal: counts only.
type ScoreboardCounters struct {
	From                string         `json:"from"`
	To                  string         `json:"to"`
	NewOrgs             int            `json:"new_orgs"`
	NewOrgsPrev         int            `json:"new_orgs_prev"`
	MachinesRegistered  int            `json:"machines_registered"`
	MachinesPrev        int            `json:"machines_registered_prev"`
	CheckpointsCreated  int            `json:"checkpoints_created"`
	CheckpointsPrev     int            `json:"checkpoints_created_prev"`
	MigrationsAttempted int            `json:"migrations_attempted"`
	MigrationsSucceeded int            `json:"migrations_succeeded"`
	MigrationsFailed    int            `json:"migrations_failed"`
	ActiveSubscriptions map[string]int `json:"active_subscriptions_by_plan"`
	MRRCents            int64          `json:"mrr_cents"`
}

// Scoreboard reads one week of counters (and the prior week for the deltas).
func (store *Store) Scoreboard(ctx context.Context, now time.Time) (ScoreboardCounters, error) {
	current := now.UTC()
	weekStart := current.AddDate(0, 0, -7)
	counters := ScoreboardCounters{
		From: weekStart.Format("2006-01-02"), To: current.Format("2006-01-02"),
		ActiveSubscriptions: map[string]int{},
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM organizations WHERE created_at >= $1`, weekStart).Scan(&counters.NewOrgs); err != nil {
		return counters, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM organizations WHERE created_at >= $1 AND created_at < $2`, weekStart.AddDate(0, 0, -7), weekStart).Scan(&counters.NewOrgsPrev); err != nil {
		return counters, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM machines WHERE created_at >= $1`, weekStart).Scan(&counters.MachinesRegistered); err != nil {
		return counters, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM machines WHERE created_at >= $1 AND created_at < $2`, weekStart.AddDate(0, 0, -7), weekStart).Scan(&counters.MachinesPrev); err != nil {
		return counters, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM checkpoints WHERE created_at >= $1`, weekStart).Scan(&counters.CheckpointsCreated); err != nil {
		return counters, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM checkpoints WHERE created_at >= $1 AND created_at < $2`, weekStart.AddDate(0, 0, -7), weekStart).Scan(&counters.CheckpointsPrev); err != nil {
		return counters, err
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE status='completed'), COUNT(*) FILTER (WHERE status='failed') FROM migration_jobs WHERE created_at >= $1`, weekStart).
		Scan(&counters.MigrationsAttempted, &counters.MigrationsSucceeded, &counters.MigrationsFailed); err != nil {
		return counters, err
	}
	rows, err := store.pool.Query(ctx, `SELECT COALESCE(plan,''), COALESCE(seats,0), COALESCE(seats_included,0) FROM entitlements WHERE `+entitledStatusesSQL)
	if err != nil {
		return counters, err
	}
	defer rows.Close()
	for rows.Next() {
		var plan string
		var seats, seatsIncluded int
		if err := rows.Scan(&plan, &seats, &seatsIncluded); err != nil {
			return counters, err
		}
		counters.ActiveSubscriptions[plan]++
		counters.MRRCents += planMRRCents(plan, seats, seatsIncluded)
	}
	return counters, rows.Err()
}
